package disruptor

import (
	"runtime"
	"sync/atomic"
)

type EventHandler interface {
	OnEvent(event OrderEvent, sequence uint64)
}

// RingBuffer estruturado com preenchimento de linhas de cache (Cache-line padding)
// para evitar False Sharing entre os núcleos do Ryzen.
type RingBuffer struct {
	events  []OrderEvent
	bitMask uint64

	// Espaçamento para isolar os cursores na cache L1/L2
	_              [8]uint64
	claimCursor    uint64 // NOVO: Cursor onde os produtores tiram a "senha" instantaneamente

	_              [8]uint64
	producerCursor uint64 // Cursor que confirma que a escrita na RAM terminou

	_              [8]uint64
	consumerCursor uint64 // Cursor onde o motor de matching está a ler
	_              [8]uint64
}

// NewRingBuffer inicializa o anel (A capacidade TEM de ser potência de 2)
func NewRingBuffer(size uint64) *RingBuffer {
	// Validação matemática de segurança para a máscara de bits funcionar
	if size&(size-1) != 0 {
		panic("A capacidade do RingBuffer deve ser uma potência de 2 (ex: 131072)")
	}

	return &RingBuffer{
		events:  make([]OrderEvent, size),
		bitMask: size - 1,
	}
}

// Publish insere um evento no anel de forma totalmente Lock-Free
func (b *RingBuffer) Publish(event OrderEvent) {
	// 1. A RESERVA ATÓMICA
	// A instrução XADD do processador dá um número único a cada goroutine instantaneamente.
	// Não há filas de espera geridas pelo SO (adeus Mutex!).
	seq := atomic.AddUint64(&b.claimCursor, 1) - 1

	// 2. CONTROLO DE FLUXO (Backpressure)
	// Se o anel der a volta e bater no consumidor, a goroutine cede a CPU.
	capacity := uint64(len(b.events))
	for seq-atomic.LoadUint64(&b.consumerCursor) >= capacity {
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
		runtime.Gosched()
	}
}

func (b *RingBuffer) StartConsumer(handler EventHandler) {
	var nextSequence uint64 = 0

	// Loop Infinito do Consumidor
	for {
		// 1. Barreira: Espera a goroutine do TCP confirmar a publicação final
		for nextSequence >= atomic.LoadUint64(&b.producerCursor) {
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