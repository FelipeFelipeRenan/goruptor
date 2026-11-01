package disruptor

import "time"

// Journal é o destino durável dos eventos (tipicamente o WAL em disco).
//
// O RingBuffer chama Append e Sync SEMPRE de uma única goroutine (o journaler)
// e SEMPRE em ordem de sequência. Isso é o que garante que o arquivo reflita
// exatamente a ordem em que o motor vai processar as ordens, tornando o replay
// determinístico.
type Journal interface {
	// Append entrega os eventos ao sistema operacional, na ordem recebida.
	// Ao retornar sem erro, os dados sobrevivem a um crash do processo
	// (mas não necessariamente a uma queda de energia: isso é o Sync).
	Append(events []OrderEvent) error

	// Sync força tudo que foi anexado até agora para o armazenamento estável (fsync).
	Sync() error
}

// JournalOptions ajusta o comportamento do journaler.
type JournalOptions struct {
	// MaxBatch limita quantos eventos entram num mesmo Append/Sync (group commit).
	// Zero usa 4096.
	MaxBatch int

	// SyncInterval define a política de durabilidade:
	//
	//   0  (padrão) -> fsync a cada lote. Um evento só é liberado para o motor e
	//                  só é confirmado (WaitDurable) depois de estar em disco.
	//                  Sobrevive a queda de energia. Várias ordens concorrentes
	//                  dividem o custo de um único fsync.
	//   >0          -> o evento é liberado após o Append (já no page cache do SO)
	//                  e o fsync acontece no máximo a cada SyncInterval.
	//                  Sobrevive a crash do processo, mas uma queda de energia
	//                  pode perder até SyncInterval de ordens já confirmadas.
	SyncInterval time.Duration
}

func (o JournalOptions) maxBatch() uint64 {
	if o.MaxBatch <= 0 {
		return 4096
	}
	return uint64(o.MaxBatch)
}
