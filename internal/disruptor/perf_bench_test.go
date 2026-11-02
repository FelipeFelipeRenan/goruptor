package disruptor

import (
	"fmt"
	"sync/atomic"
	"testing"
)

// nopHandler descarta o evento: isola o custo do ANEL (produtores + consumidor).
type nopHandler struct{ n atomic.Uint64 }

func (h *nopHandler) OnEvent(OrderEvent, uint64) { h.n.Add(1) }

// nopJournal é um journal instantâneo: isola o custo de COORDENAÇÃO
// (journaler + acordar quem espera), sem nenhum custo de disco.
type nopJournal struct{}

func (nopJournal) Append([]OrderEvent) error { return nil }
func (nopJournal) Sync() error               { return nil }

// BenchmarkRing_ManyProducers: P produtores concorrentes publicando num anel
// com consumidor real. P alto (como 100+ conexões TCP) expõe a serialização
// do commit de publicação.
func BenchmarkRing_ManyProducers(b *testing.B) {
	for _, par := range []int{1, 8, 64} { // goroutines = par * GOMAXPROCS
		b.Run(fmt.Sprintf("par=%d", par), func(b *testing.B) {
			rb := NewRingBuffer(1 << 16)
			h := &nopHandler{}
			go rb.StartConsumer(h)

			var ids atomic.Uint64
			b.SetParallelism(par)
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					rb.Publish(OrderEvent{OrderID: ids.Add(1), Price: 1, Quantity: 1})
				}
			})
			b.StopTimer()
			rb.Close()
		})
	}
}

// BenchmarkRing_WaitDurableHerd: N goroutines publicam e esperam a confirmação
// de um journal INSTANTÂNEO. Sem disco, o que sobra é o custo de acordar muitos
// esperadores a cada lote ("thundering herd").
func BenchmarkRing_WaitDurableHerd(b *testing.B) {
	for _, par := range []int{8, 64} {
		b.Run(fmt.Sprintf("par=%d", par), func(b *testing.B) {
			rb := NewJournaledRingBuffer(1<<16, nopJournal{}, JournalOptions{})
			go rb.StartJournaler()
			go rb.StartConsumer(&nopHandler{})

			var ids atomic.Uint64
			b.SetParallelism(par)
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					seq, err := rb.Publish(OrderEvent{OrderID: ids.Add(1), Price: 1, Quantity: 1})
					if err == nil {
						err = rb.WaitDurable(seq)
					}
					if err != nil {
						b.Error(err)
						return
					}
				}
			})
			b.StopTimer()
			rb.Close()
		})
	}
}
