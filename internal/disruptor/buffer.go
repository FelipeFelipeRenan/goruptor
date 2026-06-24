package disruptor

import (
	"runtime"
	"sync"
	"sync/atomic"
)

type EventHandler interface {
	OnEvent(event OrderEvent, sequence uint64)
}

type RingBuffer struct {
	events  []OrderEvent
	bitMask uint64

	mu             sync.Mutex
	_              [8]uint64
	producerCursor uint64

	_              [8]uint64
	consumerCursor uint64
	_              [8]uint64
}

func NewRingBuffer(size uint64) *RingBuffer {
	return &RingBuffer{
		events:  make([]OrderEvent, size),
		bitMask: size - 1,
	}
}

func (b *RingBuffer) Publish(event OrderEvent) {
	b.mu.Lock() // 2. Ninguém entra ao mesmo tempo

	seq := b.producerCursor // 3. Leitura limpa e segura

	// 4. Barreira: Espera se o tambor estiver cheio
	for seq-atomic.LoadUint64(&b.consumerCursor) >= uint64(len(b.events)) {
		b.mu.Unlock()     // Liberta a fila enquanto espera
		runtime.Gosched() // Cede a CPU
		b.mu.Lock()       // Tranca de novo para verificar
		seq = b.producerCursor
	}

	b.events[seq&b.bitMask] = event

	
	atomic.StoreUint64(&b.producerCursor, seq+1)

	b.mu.Unlock() // 5. Liberta o próximo produtor
}
func (b *RingBuffer) StartConsumer(handler EventHandler) {
	var nextSequence uint64 = 0

	// Loop Infinito do Consumidor
	for {
		// 1. Barreira: Espera a Mão (Produtor) colocar uma bala nova
		for nextSequence >= atomic.LoadUint64(&b.producerCursor) {
			runtime.Gosched() // Muito mais rápido e amigável com a CPU
		}

		// 2. Pega a bala da câmara
		event := b.events[nextSequence&b.bitMask]

		// 3. Dispara a Regra de Negócio (O nosso Motor de Matching)
		handler.OnEvent(event, nextSequence)

		// 4. Prepara para a próxima bala e avisa que essa já foi disparada
		nextSequence++
		atomic.StoreUint64(&b.consumerCursor, nextSequence)
	}
}
