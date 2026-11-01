package disruptor

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrClosed é devolvido por Publish depois de Close/Shutdown.
	ErrClosed = errors.New("disruptor: ring buffer fechado")

	// ErrJournalFailed indica que o journal (WAL) falhou. O anel entra em
	// fail-stop: nenhuma ordem nova é aceita e o motor não processa nada que
	// não esteja em disco.
	ErrJournalFailed = errors.New("disruptor: falha no journal")
)

// closedBit marca o claimCursor como fechado. Ver Close.
const closedBit = uint64(1) << 63

type EventHandler interface {
	OnEvent(event OrderEvent, sequence uint64)
}

// RingBuffer estruturado com preenchimento de linhas de cache (Cache-line padding)
// para evitar False Sharing entre os núcleos do Ryzen.
//
// Fluxo com journal (NewJournaledRingBuffer):
//
//	produtores --Publish--> [anel] --journaler--> WAL (Append+fsync em lote)
//	                           \                      |
//	                            +--motor (só até journalCursor)
//	produtor --WaitDurable(seq)--> ACK ao cliente
//
// Garantias:
//   - a ordem no WAL é exatamente a ordem de sequência do anel;
//   - o motor nunca processa um evento que ainda não esteja no journal;
//   - WaitDurable(seq) só retorna nil depois que o evento está no journal.
type RingBuffer struct {
	events  []OrderEvent
	bitMask uint64

	// Espaçamento para isolar os cursores na cache L1/L2
	_           [8]uint64
	claimCursor uint64 // Cursor onde os produtores tiram a "senha" instantaneamente

	_              [8]uint64
	producerCursor uint64 // Cursor que confirma que a escrita na RAM terminou

	_              [8]uint64
	consumerCursor uint64 // Cursor onde o motor de matching está a ler

	_             [8]uint64
	journalCursor uint64 // Tudo antes disto já está no journal (só usado com journal)
	_             [8]uint64

	// --- ciclo de vida (raramente escrito) ---
	finalSeq   uint64 // total de sequências aceitas; válido quando closedFlag == 1
	closedFlag uint32
	failedFlag uint32
	closeOnce  sync.Once

	consumerStarted  uint32
	journalerStarted uint32
	consumerDone     chan struct{}
	journalerDone    chan struct{}

	// --- journal ---
	journal Journal // nil = sem journal
	jopts   JournalOptions
	durMu   sync.Mutex
	durCond *sync.Cond
	failErr error // protegido por durMu
}

// NewRingBuffer inicializa o anel (A capacidade TEM de ser potência de 2).
// Sem journal: o motor consome assim que o evento é publicado.
func NewRingBuffer(size uint64) *RingBuffer {
	// Validação matemática de segurança para a máscara de bits funcionar
	if size == 0 || size&(size-1) != 0 {
		panic("A capacidade do RingBuffer deve ser uma potência de 2 (ex: 131072)")
	}

	b := &RingBuffer{
		events:        make([]OrderEvent, size),
		bitMask:       size - 1,
		consumerDone:  make(chan struct{}),
		journalerDone: make(chan struct{}),
	}
	b.durCond = sync.NewCond(&b.durMu)
	return b
}

// NewJournaledRingBuffer cria um anel com journal. Lembre de iniciar o estágio
// de journal com `go rb.StartJournaler()` além do `go rb.StartConsumer(...)`.
func NewJournaledRingBuffer(size uint64, j Journal, opts JournalOptions) *RingBuffer {
	b := NewRingBuffer(size)
	b.journal = j
	b.jopts = opts
	return b
}

// Publish insere um evento no anel de forma Lock-Free e devolve a sequência
// atribuída. Retorna ErrClosed após Close e ErrJournalFailed se o journal caiu.
//
// Publish NÃO espera o journal. Para confirmar o evento ao cliente, chame
// WaitDurable(seq) com a sequência devolvida.
func (b *RingBuffer) Publish(event OrderEvent) (uint64, error) {
	if atomic.LoadUint32(&b.failedFlag) == 1 {
		return 0, b.journalFailure()
	}

	// 1. A RESERVA ATÓMICA
	// A instrução XADD do processador dá um número único a cada goroutine instantaneamente.
	// Não há filas de espera geridas pelo SO (adeus Mutex!).
	seq := atomic.AddUint64(&b.claimCursor, 1) - 1
	if seq&closedBit != 0 {
		// O anel foi fechado antes da nossa reserva: esta sequência não existe.
		return 0, ErrClosed
	}

	// 2. CONTROLO DE FLUXO (Backpressure)
	// Se o anel der a volta e bater no consumidor, a goroutine cede a CPU.
	capacity := uint64(len(b.events))
	for seq-atomic.LoadUint64(&b.consumerCursor) >= capacity {
		if atomic.LoadUint32(&b.failedFlag) == 1 {
			return 0, b.journalFailure()
		}
		runtime.Gosched()
	}

	// 3. ESCRITA CONCORRENTE
	// Como 'seq' é único para cada goroutine, centenas delas podem escrever
	// em paralelo absoluto nas suas posições isoladas da memória RAM.
	b.events[seq&b.bitMask] = event

	// 4. COMMIT LINEARIZADO (Spinlock de Publicação)
	// Garante que o consumidor não tenta ler a sequência 10 se a goroutine da
	// sequência 9 ainda não acabou de gravar os dados.
	for !atomic.CompareAndSwapUint64(&b.producerCursor, seq, seq+1) {
		if atomic.LoadUint32(&b.failedFlag) == 1 {
			return 0, b.journalFailure()
		}
		runtime.Gosched()
	}
	return seq, nil
}

// WaitDurable bloqueia até o evento de sequência seq estar no journal.
// Retorna nil quando é seguro confirmar a ordem ao cliente; retorna
// ErrJournalFailed (embrulhado) se o journal falhou antes disso.
// Sem journal, retorna imediatamente.
func (b *RingBuffer) WaitDurable(seq uint64) error {
	if b.journal == nil || atomic.LoadUint64(&b.journalCursor) > seq {
		return nil
	}
	b.durMu.Lock()
	defer b.durMu.Unlock()
	for atomic.LoadUint64(&b.journalCursor) <= seq {
		if b.failErr != nil {
			return b.failErr
		}
		b.durCond.Wait()
	}
	return nil
}

// StartConsumer roda o loop do motor. Bloqueia até Close (após drenar tudo)
// ou até o journal falhar (após processar tudo que já estava em disco).
func (b *RingBuffer) StartConsumer(handler EventHandler) {
	atomic.StoreUint32(&b.consumerStarted, 1)
	defer close(b.consumerDone)

	var nextSequence uint64 = 0
	journaled := b.journal != nil

	// Loop Infinito do Consumidor
	for {
		// 1. Barreira: espera o evento estar publicado (e, com journal, gravado).
		for {
			var limit uint64
			if journaled {
				limit = atomic.LoadUint64(&b.journalCursor)
			} else {
				limit = atomic.LoadUint64(&b.producerCursor)
			}
			if nextSequence < limit {
				break
			}
			if atomic.LoadUint32(&b.failedFlag) == 1 {
				return // fail-stop: nada além do que já está em disco
			}
			if atomic.LoadUint32(&b.closedFlag) == 1 && nextSequence >= atomic.LoadUint64(&b.finalSeq) {
				return // fechado e totalmente drenado
			}
			runtime.Gosched()
		}

		// 2. Pega a bala da câmara
		event := b.events[nextSequence&b.bitMask]

		// 3. Dispara a Regra de Negócio (O Motor de Matching)
		handler.OnEvent(event, nextSequence)

		// 4. Prepara para a próxima bala e avisa os produtores que esta posição já está livre
		nextSequence++
		atomic.StoreUint64(&b.consumerCursor, nextSequence)
	}
}

// StartJournaler roda o estágio de journal: lê o anel em ordem de sequência,
// grava em lote (group commit), faz fsync conforme JournalOptions e só então
// avança journalCursor, liberando o motor e os WaitDurable.
// Bloqueia até Close (após drenar) ou até uma falha do journal.
func (b *RingBuffer) StartJournaler() {
	if b.journal == nil {
		return
	}
	atomic.StoreUint32(&b.journalerStarted, 1)
	defer close(b.journalerDone)

	var (
		next      uint64
		maxBatch  = b.jopts.maxBatch()
		batch     = make([]OrderEvent, 0, maxBatch)
		lastSync  = time.Now()
		dirty     bool // anexado mas ainda sem fsync (só no modo SyncInterval > 0)
		idleSpins int
	)

	syncNow := func() bool {
		if err := b.journal.Sync(); err != nil {
			b.fail(err)
			return false
		}
		lastSync = time.Now()
		dirty = false
		return true
	}

	for {
		avail := atomic.LoadUint64(&b.producerCursor)

		if next >= avail {
			// Ocioso: fsync pendente do modo por intervalo e checagem de encerramento.
			if dirty && time.Since(lastSync) >= b.jopts.SyncInterval && !syncNow() {
				return
			}
			if atomic.LoadUint32(&b.closedFlag) == 1 && next >= atomic.LoadUint64(&b.finalSeq) {
				if dirty {
					syncNow()
				}
				return
			}
			idleSpins++
			if idleSpins < 256 {
				runtime.Gosched()
			} else {
				time.Sleep(50 * time.Microsecond) // sem trânsito: não queimar CPU
			}
			continue
		}
		idleSpins = 0

		end := avail
		if end-next > maxBatch {
			end = next + maxBatch
		}
		batch = batch[:0]
		for s := next; s < end; s++ {
			batch = append(batch, b.events[s&b.bitMask])
		}

		if err := b.journal.Append(batch); err != nil {
			b.fail(err)
			return
		}
		if b.jopts.SyncInterval <= 0 {
			if !syncNow() {
				return
			}
		} else {
			dirty = true
			if time.Since(lastSync) >= b.jopts.SyncInterval && !syncNow() {
				return
			}
		}

		next = end
		atomic.StoreUint64(&b.journalCursor, next)

		// Acorda os WaitDurable. Pegar o lock evita a perda de wakeup.
		b.durMu.Lock()
		b.durCond.Broadcast()
		b.durMu.Unlock()
	}
}

// fail coloca o anel em fail-stop e acorda todos que esperam o journal.
func (b *RingBuffer) fail(err error) {
	b.durMu.Lock()
	if b.failErr == nil {
		b.failErr = fmt.Errorf("%w: %w", ErrJournalFailed, err)
	}
	atomic.StoreUint32(&b.failedFlag, 1)
	b.durCond.Broadcast()
	b.durMu.Unlock()
}

func (b *RingBuffer) journalFailure() error {
	b.durMu.Lock()
	defer b.durMu.Unlock()
	if b.failErr != nil {
		return b.failErr
	}
	return ErrJournalFailed
}

// Close impede novas publicações. O que já foi aceito continua sendo
// journalado e processado até o fim. É idempotente.
//
// Truque: soma closedBit ao claimCursor. O valor anterior é exatamente o número
// de sequências legítimas (todas vão completar o Publish). Qualquer produtor que
// reservar depois enxerga o bit e recebe ErrClosed, sem custo extra no caminho
// quente e sem janela de corrida entre "checar fechado" e "reservar".
func (b *RingBuffer) Close() {
	b.closeOnce.Do(func() {
		accepted := atomic.AddUint64(&b.claimCursor, closedBit) // valor antigo + closedBit
		atomic.StoreUint64(&b.finalSeq, accepted-closedBit)
		atomic.StoreUint32(&b.closedFlag, 1)
	})
}

// Shutdown fecha o anel e espera os estágios iniciados (journaler e motor)
// drenarem tudo que foi aceito, ou ctx expirar. Só espera estágios cujo
// StartJournaler/StartConsumer já começaram a rodar.
func (b *RingBuffer) Shutdown(ctx context.Context) error {
	b.Close()
	if atomic.LoadUint32(&b.journalerStarted) == 1 {
		select {
		case <-b.journalerDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if atomic.LoadUint32(&b.consumerStarted) == 1 {
		select {
		case <-b.consumerDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
