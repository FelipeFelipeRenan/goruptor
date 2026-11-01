package disruptor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// fakeJournal é um Journal em memória que registra exatamente o que o
// journaler fez: a ordem dos Append, quantos eventos já foram "fsyncados" e
// quantos fsyncs aconteceram. Permite injetar lentidão e falhas.
type fakeJournal struct {
	mu        sync.Mutex
	appended  []OrderEvent
	synced    int // quantos eventos existiam no último Sync concluído
	syncCount int
	appendCnt int

	syncDelay       time.Duration
	failAppendAfter int // >0: o Append de número N em diante falha (nenhum evento do lote é gravado)
}

func (j *fakeJournal) Append(events []OrderEvent) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.appendCnt++
	if j.failAppendAfter > 0 && j.appendCnt >= j.failAppendAfter {
		return errors.New("disco cheio (injetado)")
	}
	j.appended = append(j.appended, events...)
	return nil
}

func (j *fakeJournal) Sync() error {
	if j.syncDelay > 0 {
		time.Sleep(j.syncDelay) // fsync lento
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.syncCount++
	j.synced = len(j.appended)
	return nil
}

func (j *fakeJournal) syncedCount() int { j.mu.Lock(); defer j.mu.Unlock(); return j.synced }
func (j *fakeJournal) syncs() int       { j.mu.Lock(); defer j.mu.Unlock(); return j.syncCount }
func (j *fakeJournal) snapshot() []OrderEvent {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]OrderEvent(nil), j.appended...)
}

// recHandler registra a ordem em que o motor recebeu os eventos.
// É chamado só pela goroutine consumidora; leia depois do Shutdown.
type recHandler struct {
	events []OrderEvent
	hook   func(e OrderEvent, seq uint64)
}

func (h *recHandler) OnEvent(e OrderEvent, seq uint64) {
	if h.hook != nil {
		h.hook(e, seq)
	}
	h.events = append(h.events, e)
}

// start sobe os estágios e espera ambos estarem rodando (Shutdown só espera
// estágios já iniciados).
func start(rb *RingBuffer, h EventHandler) {
	go rb.StartJournaler()
	go rb.StartConsumer(h)
	for atomic.LoadUint32(&rb.consumerStarted) == 0 ||
		(rb.journal != nil && atomic.LoadUint32(&rb.journalerStarted) == 0) {
		time.Sleep(100 * time.Microsecond)
	}
}

func shutdown(t *testing.T, rb *RingBuffer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rb.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// within falha o teste se f não terminar a tempo (detecta deadlocks).
func within(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("travou: não terminou em %v", d)
	}
}

// evID codifica produtor e índice no OrderID: id = producer<<32 | i.
func evID(p, i int) uint64 { return uint64(p)<<32 | uint64(i) }

// ---------------------------------------------------------------------------
// Propriedade central do ponto 2: a ordem do WAL == a ordem do motor
// ---------------------------------------------------------------------------

// Antes da correção, o WAL era gravado por cada goroutine ANTES do Publish.
// Com produtores concorrentes a ordem do arquivo divergia da sequência do anel,
// e o replay reconstruía um livro diferente do que estava na memória.
// Aqui: 8 produtores brigando por um anel minúsculo (256 slots, muita pressão).
func TestJournal_WALOrderEqualsEngineOrder(t *testing.T) {
	const producers, perProducer = 8, 4000

	j := &fakeJournal{}
	rb := NewJournaledRingBuffer(256, j, JournalOptions{})
	h := &recHandler{}
	start(rb, h)

	seqOf := make([][]uint64, producers) // sequência devolvida por Publish, por produtor
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		seqOf[p] = make([]uint64, perProducer)
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				seq, err := rb.Publish(OrderEvent{OrderID: evID(p, i), Price: 1, Quantity: 1})
				if err != nil {
					t.Errorf("Publish: %v", err)
					return
				}
				if err := rb.WaitDurable(seq); err != nil {
					t.Errorf("WaitDurable: %v", err)
					return
				}
				seqOf[p][i] = seq
			}
		}(p)
	}
	within(t, 60*time.Second, wg.Wait)
	shutdown(t, rb)

	wal := j.snapshot()
	total := producers * perProducer
	if len(wal) != total || len(h.events) != total {
		t.Fatalf("esperava %d eventos; WAL=%d motor=%d", total, len(wal), len(h.events))
	}

	// 1) WAL e motor veem EXATAMENTE a mesma sequência.
	for i := range wal {
		if wal[i] != h.events[i] {
			t.Fatalf("divergência na posição %d: WAL=%+v motor=%+v", i, wal[i], h.events[i])
		}
	}
	// 2) A sequência devolvida por Publish aponta para o evento certo no WAL.
	for p := 0; p < producers; p++ {
		for i := 0; i < perProducer; i++ {
			if got := wal[seqOf[p][i]].OrderID; got != evID(p, i) {
				t.Fatalf("seq %d deveria ser %x, WAL tem %x", seqOf[p][i], evID(p, i), got)
			}
		}
	}
	// 3) A ordem de cada produtor é preservada (sequências estritamente crescentes).
	for p := 0; p < producers; p++ {
		for i := 1; i < perProducer; i++ {
			if seqOf[p][i] <= seqOf[p][i-1] {
				t.Fatalf("produtor %d: ordem quebrada em %d", p, i)
			}
		}
	}
}

// ACK (WaitDurable retornou nil) => o evento já está no journal E já foi fsyncado.
func TestJournal_AckImpliesDurable(t *testing.T) {
	j := &fakeJournal{syncDelay: 200 * time.Microsecond}
	rb := NewJournaledRingBuffer(1024, j, JournalOptions{})
	start(rb, &recHandler{})

	var violations atomic.Int64
	var wg sync.WaitGroup
	for p := 0; p < 16; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				seq, err := rb.Publish(OrderEvent{OrderID: evID(p, i)})
				if err != nil || rb.WaitDurable(seq) != nil {
					violations.Add(1)
					return
				}
				// No instante do ACK, o evento de sequência seq tem que estar fsyncado.
				if j.syncedCount() <= int(seq) {
					violations.Add(1)
				}
			}
		}(p)
	}
	within(t, 60*time.Second, wg.Wait)
	shutdown(t, rb)

	if v := violations.Load(); v != 0 {
		t.Fatalf("%d ACKs foram liberados antes do fsync", v)
	}
}

// O motor nunca pode agir sobre uma ordem que ainda não está em disco: se o
// processo morresse ali, o livro em memória teria algo que o WAL não tem.
func TestJournal_EngineNeverAheadOfJournal(t *testing.T) {
	j := &fakeJournal{syncDelay: 100 * time.Microsecond}
	rb := NewJournaledRingBuffer(512, j, JournalOptions{})

	var ahead atomic.Int64
	h := &recHandler{hook: func(_ OrderEvent, seq uint64) {
		if j.syncedCount() <= int(seq) {
			ahead.Add(1)
		}
	}}
	start(rb, h)

	var wg sync.WaitGroup
	for p := 0; p < 8; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				rb.Publish(OrderEvent{OrderID: evID(p, i)})
			}
		}(p)
	}
	within(t, 60*time.Second, wg.Wait)
	shutdown(t, rb)

	if len(h.events) != 8*500 {
		t.Fatalf("motor processou %d de %d", len(h.events), 8*500)
	}
	if a := ahead.Load(); a != 0 {
		t.Fatalf("motor processou %d eventos antes de eles estarem em disco", a)
	}
}

// Group commit: com fsync caro (2ms), 64 conexões esperando ao mesmo tempo
// devem dividir poucos fsyncs, não pagar um fsync por ordem.
func TestJournal_GroupCommitAmortizesFsync(t *testing.T) {
	const producers, perProducer = 64, 40
	j := &fakeJournal{syncDelay: 2 * time.Millisecond}
	rb := NewJournaledRingBuffer(4096, j, JournalOptions{})
	start(rb, &recHandler{})

	start0 := time.Now()
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				seq, _ := rb.Publish(OrderEvent{OrderID: evID(p, i)})
				rb.WaitDurable(seq)
			}
		}(p)
	}
	within(t, 60*time.Second, wg.Wait)
	elapsed := time.Since(start0)
	shutdown(t, rb)

	events := producers * perProducer
	syncs := j.syncs()
	t.Logf("%d ordens confirmadas com %d fsyncs (%.1f ordens/fsync) em %v",
		events, syncs, float64(events)/float64(syncs), elapsed.Round(time.Millisecond))

	// Sem agrupamento seriam `events` fsyncs. Exigimos pelo menos 4x de ganho.
	if syncs > events/4 {
		t.Fatalf("group commit não agrupou: %d fsyncs para %d ordens", syncs, events)
	}
}

// Modo SyncInterval: ACK após o Append (sem esperar o fsync), e fsync raro.
// No Shutdown tudo é fsyncado.
func TestJournal_IntervalModeSyncsRarely(t *testing.T) {
	j := &fakeJournal{}
	rb := NewJournaledRingBuffer(1024, j, JournalOptions{SyncInterval: 50 * time.Millisecond})
	start(rb, &recHandler{})

	const n = 2000
	t0 := time.Now()
	for i := 0; i < n; i++ {
		seq, err := rb.Publish(OrderEvent{OrderID: uint64(i)})
		if err != nil || rb.WaitDurable(seq) != nil {
			t.Fatalf("falha na ordem %d", i)
		}
	}
	elapsed := time.Since(t0)
	shutdown(t, rb)

	maxSyncs := int(elapsed/(50*time.Millisecond)) + 3
	if j.syncs() > maxSyncs {
		t.Fatalf("%d fsyncs em %v; com intervalo de 50ms esperava no máximo ~%d", j.syncs(), elapsed, maxSyncs)
	}
	if j.syncedCount() != n {
		t.Fatalf("após Shutdown só %d de %d eventos foram fsyncados", j.syncedCount(), n)
	}
}

// ---------------------------------------------------------------------------
// Falha do disco: fail-stop sem deadlock
// ---------------------------------------------------------------------------

func TestJournal_FailureIsFailStopAndNeverHangs(t *testing.T) {
	j := &fakeJournal{failAppendAfter: 20} // do 20º lote em diante, o disco "enche"
	rb := NewJournaledRingBuffer(128, j, JournalOptions{})
	h := &recHandler{}
	start(rb, h)

	var mu sync.Mutex
	acked := map[uint64]bool{}
	var failedAcks atomic.Int64

	var wg sync.WaitGroup
	for p := 0; p < 6; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < 5000; i++ {
				ev := OrderEvent{OrderID: evID(p, i)}
				seq, err := rb.Publish(ev)
				if err == nil {
					err = rb.WaitDurable(seq)
				}
				if err != nil {
					if !errors.Is(err, ErrJournalFailed) {
						t.Errorf("erro inesperado: %v", err)
					}
					failedAcks.Add(1)
					return
				}
				mu.Lock()
				acked[ev.OrderID] = true
				mu.Unlock()
			}
		}(p)
	}
	// O ponto principal: ninguém pode ficar preso (spin de backpressure, CAS de commit, Wait).
	within(t, 15*time.Second, wg.Wait)

	if failedAcks.Load() == 0 {
		t.Fatal("a falha injetada deveria ter derrubado produtores")
	}

	// Depois da falha, nada novo é aceito.
	if _, err := rb.Publish(OrderEvent{OrderID: 1}); !errors.Is(err, ErrJournalFailed) {
		t.Fatalf("Publish após falha deveria retornar ErrJournalFailed, veio %v", err)
	}

	// O motor para sozinho depois de processar o que já estava em disco.
	within(t, 5*time.Second, func() { <-rb.consumerDone })

	inWAL := map[uint64]bool{}
	for _, e := range j.snapshot() {
		inWAL[e.OrderID] = true
	}
	// Nenhum ACK pode ter sido dado a uma ordem que não está no journal.
	for id := range acked {
		if !inWAL[id] {
			t.Fatalf("ordem %x foi confirmada ao cliente mas não está no journal", id)
		}
	}
	// O motor nunca processa algo que não esteja no journal.
	for _, e := range h.events {
		if !inWAL[e.OrderID] {
			t.Fatalf("motor processou %x que não está no journal", e.OrderID)
		}
	}
	t.Logf("journal=%d motor=%d confirmadas=%d produtores derrubados=%d",
		len(inWAL), len(h.events), len(acked), failedAcks.Load())
}

// ---------------------------------------------------------------------------
// Shutdown / Close
// ---------------------------------------------------------------------------

// Fechar no meio do tráfego: toda ordem aceita (Publish sem erro) tem que
// aparecer no journal E no motor; toda recusada tem que ser ErrClosed.
// Repetido várias vezes para expor corridas (rode com -race).
func TestShutdown_EveryAcceptedOrderIsJournaledAndProcessed(t *testing.T) {
	for round := 0; round < 30; round++ {
		j := &fakeJournal{}
		rb := NewJournaledRingBuffer(256, j, JournalOptions{})
		h := &recHandler{}
		start(rb, h)

		var accepted, refused atomic.Int64
		var wg sync.WaitGroup
		for p := 0; p < 8; p++ {
			wg.Add(1)
			go func(p int) {
				defer wg.Done()
				for i := 0; ; i++ {
					_, err := rb.Publish(OrderEvent{OrderID: evID(p, i)})
					if err != nil {
						if !errors.Is(err, ErrClosed) {
							t.Errorf("erro inesperado: %v", err)
						}
						refused.Add(1)
						return
					}
					accepted.Add(1)
				}
			}(p)
		}

		time.Sleep(time.Duration(round%5) * time.Millisecond)
		shutdown(t, rb)
		within(t, 10*time.Second, wg.Wait)

		acc := int(accepted.Load())
		if len(j.snapshot()) != acc || len(h.events) != acc {
			t.Fatalf("rodada %d: aceitas=%d journal=%d motor=%d", round, acc, len(j.snapshot()), len(h.events))
		}
		if j.syncedCount() != acc {
			t.Fatalf("rodada %d: %d aceitas mas só %d fsyncadas", round, acc, j.syncedCount())
		}
		if refused.Load() != 8 {
			t.Fatalf("rodada %d: esperava 8 produtores recusados, veio %d", round, refused.Load())
		}
	}
}

// Mesmo sem journal o Shutdown drena tudo que foi aceito.
func TestShutdown_WithoutJournalDrains(t *testing.T) {
	rb := NewRingBuffer(64)
	h := &recHandler{}
	start(rb, h)

	var accepted atomic.Int64
	var wg sync.WaitGroup
	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; ; i++ {
				if _, err := rb.Publish(OrderEvent{OrderID: evID(p, i)}); err != nil {
					return
				}
				accepted.Add(1)
			}
		}(p)
	}
	time.Sleep(5 * time.Millisecond)
	shutdown(t, rb)
	within(t, 10*time.Second, wg.Wait)

	if int64(len(h.events)) != accepted.Load() {
		t.Fatalf("aceitas=%d processadas=%d", accepted.Load(), len(h.events))
	}
}

func TestClose_IsIdempotentAndRejectsNewOrders(t *testing.T) {
	rb := NewRingBuffer(8)
	start(rb, &recHandler{})
	rb.Close()
	rb.Close()
	if _, err := rb.Publish(OrderEvent{OrderID: 1}); !errors.Is(err, ErrClosed) {
		t.Fatalf("esperava ErrClosed, veio %v", err)
	}
	shutdown(t, rb)
	shutdown(t, rb) // segunda chamada também retorna nil
}

// Handler travado: Shutdown respeita o contexto em vez de bloquear para sempre.
func TestShutdown_RespectsContextDeadline(t *testing.T) {
	rb := NewRingBuffer(8)
	release := make(chan struct{})
	h := &recHandler{hook: func(OrderEvent, uint64) { <-release }}
	start(rb, h)
	rb.Publish(OrderEvent{OrderID: 1})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := rb.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("esperava DeadlineExceeded, veio %v", err)
	}
	close(release)
	shutdown(t, rb) // agora drena normalmente
}

func TestNewRingBuffer_RejectsNonPowerOfTwo(t *testing.T) {
	for _, size := range []uint64{0, 3, 1000} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("tamanho %d deveria causar panic", size)
				}
			}()
			NewRingBuffer(size)
		}()
	}
}

// Produtores "fire and forget" (não esperam o ACK) enchem o anel enquanto o
// journal está lento. Se o disco falhar nesse momento, os produtores presos no
// spin de backpressure (anel cheio) precisam ser liberados com erro, não ficar
// girando para sempre num motor que parou.
func TestJournal_FailureReleasesProducersBlockedOnBackpressure(t *testing.T) {
	j := &fakeJournal{failAppendAfter: 5, syncDelay: time.Millisecond}
	rb := NewJournaledRingBuffer(64, j, JournalOptions{MaxBatch: 8})
	start(rb, &recHandler{})

	var released atomic.Int64
	var wg sync.WaitGroup
	for p := 0; p < 6; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < 1_000_000; i++ { // jamais terminaria sem a falha
				if _, err := rb.Publish(OrderEvent{OrderID: evID(p, i)}); err != nil {
					if !errors.Is(err, ErrJournalFailed) {
						t.Errorf("erro inesperado: %v", err)
					}
					released.Add(1)
					return
				}
			}
		}(p)
	}
	within(t, 15*time.Second, wg.Wait)
	if released.Load() != 6 {
		t.Fatalf("esperava 6 produtores liberados com erro, veio %d", released.Load())
	}
}
