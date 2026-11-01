package api

import "github.com/FelipeFelipeRenan/goruptor/internal/disruptor"

// orderPublisher é o que a camada de rede precisa do RingBuffer.
type orderPublisher interface {
	Publish(event disruptor.OrderEvent) (uint64, error)
	WaitDurable(seq uint64) error
}

// orderArchiver recebe as ordens já confirmadas (ex.: o Batcher do Postgres).
type orderArchiver interface {
	Push(event disruptor.OrderEvent)
}

// acceptOrder é o caminho único de entrada de uma ordem, usado por HTTP e TCP:
//
//  1. Publish: reserva a sequência no anel (lock-free). A partir daqui o
//     journaler grava a ordem NA ORDEM DA SEQUÊNCIA, então a ordem do WAL
//     é a mesma em que o motor vai processar.
//  2. WaitDurable: espera o journaler confirmar (fsync em lote). Só depois
//     disso o motor enxerga a ordem e o cliente recebe o ACK, então
//     "ACK recebido" significa "ordem sobrevive a crash/queda de energia".
//  3. Archive: só ordens confirmadas vão para o Postgres.
//
// Um erro significa que a ordem NÃO foi confirmada: o cliente não deve
// assumir que ela existe.
func acceptOrder(rb orderPublisher, archive orderArchiver, event disruptor.OrderEvent) error {
	seq, err := rb.Publish(event)
	if err != nil {
		return err
	}
	if err := rb.WaitDurable(seq); err != nil {
		return err
	}
	archive.Push(event)
	return nil
}
