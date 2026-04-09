package disruptor

import (
	"runtime"
	"sync/atomic"
)

type RingBuffer struct {
	// Array pre-alocado. As Ordens viverão aqui
	events []OrderEvent
	// usado pra calcular a posição no array de forma rapida
	bitMask uint64

	// Cache line padding (64 bytes) para evitar False Sharing
	// Isso impede que as variáveis de cima fiquem na mesma linha de cache do Produtor
	_ [8]uint64
	// cursores de 64-bits que só crescem
	// nunca serão acessados diretamente, apenas via sync/atomic
	producerCursor uint64

	// Mais um padding
	_ [8]uint64

	consumerCursor uint64
	_              [8]uint64
}

func NewRingBuffer(size uint64) *RingBuffer {

	// truque de bitwise pra checar se o tamanho é uma potencia de 2
	if size == 0 || (size&(size-1)) != 0 {
		panic("The size of the Ring Buffer must be a power of 2")
	}

	return &RingBuffer{
		events:         make([]OrderEvent, size), // aloca tudo de uma vez
		bitMask:        size - 1,
		producerCursor: 0,
		consumerCursor: 0,
	}
}

// Publish recebe uma nova ordem e injeta no Ring Buffer usando concorrência Lock-Free.
func (b *RingBuffer) Publish(event OrderEvent) {
	// 1. Pegamos o cursor atual do Produtor.
	// Como só tem 1 goroutine publicando, não precisamos ler isso atomicamente.
	seq := b.producerCursor

	// 2. A Barreira (Slow Consumer Check)
	// Calculamos qual é a "volta completa" no buffer.
	// Se a sequência atual menos o cursor do consumidor for maior ou igual ao
	// tamanho do buffer, significa que o produtor alcançou o consumidor.
	// Não podemos sobrescrever dados que não foram lidos!
	for seq-atomic.LoadUint64(&b.consumerCursor) >= uint64(len(b.events)) {
		// Busy Spinning: Cede o controle da CPU temporariamente para outras
		// goroutines (como o consumidor) trabalharem, em vez de travar (lock).
		runtime.Gosched()
	}

	// 3. A Gravação
	// Calculamos o índice exato usando a nossa máscara de bits super rápida.
	index := seq & b.bitMask
	b.events[index] = event

	// 4. O Commit (Visibilidade de Memória)
	// Só AGORA, depois que o dado está gravado na RAM, nós avançamos o cursor.
	// Usamos StoreUint64 para garantir que qualquer núcleo do processador
	// veja essa atualização instantaneamente.
	atomic.StoreUint64(&b.producerCursor, seq+1)

}
