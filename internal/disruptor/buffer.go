package disruptor

type RingBuffer struct {
	// Array pre-alocado. As Ordens viverão aqui
	events []OrderEvent
	// usado pra calcular a posição no array de forma rapida
	bitMask uint64

	// cursores de 64-bits que só crescem
	// nunca serão acessados diretamente, apenas via sync/atomic
	producerCursor uint64
	consumerCursor uint64
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
