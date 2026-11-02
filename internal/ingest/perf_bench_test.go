package ingest

import (
	"sync/atomic"
	"testing"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
)

type instantPublisher struct{ seq atomic.Uint64 }

func (p *instantPublisher) Publish(disruptor.OrderEvent) (uint64, error) {
	return p.seq.Add(1) - 1, nil
}
func (p *instantPublisher) WaitDurable(uint64) error { return nil }

type nopArchiver struct{}

func (nopArchiver) Push(disruptor.OrderEvent) error { return nil }

// BenchmarkIngress_Submit: custo de validar + deduplicar + (publicar/confirmar
// instantâneos). Mostra o que a ENTRADA custa antes de qualquer disco ou motor.
func BenchmarkIngress_Submit(b *testing.B) {
	in := New(&instantPublisher{}, nopArchiver{})
	var ids atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			id := ids.Add(1)
			if err := in.Submit(disruptor.OrderEvent{OrderID: id, Price: 1000 + id%64, Quantity: 1, Side: disruptor.Side(id & 1)}); err != nil {
				b.Error(err)
				return
			}
		}
	})
}
