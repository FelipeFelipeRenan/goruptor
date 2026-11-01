package matching

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
	"github.com/FelipeFelipeRenan/goruptor/internal/storage"
)

// countingHandler envolve o EngineHandler real e conta quantos eventos o
// motor terminou de processar. Assim o benchmark mede o pipeline completo
// (publicação + consumo + matching) e só para o relógio quando o último
// evento foi de fato executado, não quando foi apenas enfileirado.
type countingHandler struct {
	inner     *EngineHandler
	processed atomic.Uint64
}

func (c *countingHandler) OnEvent(e disruptor.OrderEvent, seq uint64) {
	c.inner.OnEvent(e, seq)
	c.processed.Add(1)
}

// BenchmarkPipeline_EndToEnd mede ordens/segundo do caminho
// ring buffer -> EngineHandler -> OrderBook, com N produtores concorrentes.
//
// Este é o benchmark que expõe o custo de qualquer I/O colocado no hot path
// do handler (ex.: um fmt.Printf por ordem).
func BenchmarkPipeline_EndToEnd(b *testing.B) {
	rb := disruptor.NewRingBuffer(1 << 17)
	book := NewOrderBook(1, nil)
	h := &countingHandler{inner: NewEngineHandler(book)}
	go rb.StartConsumer(h)

	var ids atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			id := ids.Add(1)
			// Preços em faixa estreita e lados alternados: há matching de verdade.
			rb.Publish(disruptor.OrderEvent{
				OrderID:  id,
				Price:    1000 + id%64,
				Quantity: 1,
				Side:     disruptor.Side(id & 1),
			})
		}
	})

	for h.processed.Load() < ids.Load() {
		time.Sleep(50 * time.Microsecond)
	}
	elapsed := time.Since(start)
	b.StopTimer()
	b.ReportMetric(float64(ids.Load())/elapsed.Seconds(), "orders/s")
}

// BenchmarkHandler_OnEvent isola o custo do handler + matching, sem o ring buffer.
func BenchmarkHandler_OnEvent(b *testing.B) {
	book := NewOrderBook(1, nil)
	h := NewEngineHandler(book)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := uint64(i + 1)
		h.OnEvent(disruptor.OrderEvent{
			OrderID:  id,
			Price:    1000 + id%64,
			Quantity: 1,
			Side:     disruptor.Side(id & 1),
		}, id)
	}
}

// countingJournal conta quantos fsyncs o group commit realmente fez.
type countingJournal struct {
	disruptor.Journal
	syncs atomic.Uint64
}

func (c *countingJournal) Sync() error { c.syncs.Add(1); return c.Journal.Sync() }

// BenchmarkPipeline_DurableAck mede o caminho REAL de uma ordem confirmada:
// Publish -> journaler (WAL em disco + fsync em lote) -> motor -> WaitDurable.
// Cada produtor só envia a próxima ordem depois do ACK, como um cliente TCP
// síncrono. O número de produtores (-cpu / SetParallelism) é o que alimenta o
// group commit: mais conexões esperando = mais ordens por fsync.
func BenchmarkPipeline_DurableAck(b *testing.B) {
	wal, err := storage.NewWAL(filepath.Join(b.TempDir(), "bench.jsonl"))
	if err != nil {
		b.Fatal(err)
	}
	cj := &countingJournal{Journal: wal}

	rb := disruptor.NewJournaledRingBuffer(1<<17, cj, disruptor.JournalOptions{})
	book := NewOrderBook(1, nil)
	go rb.StartJournaler()
	go rb.StartConsumer(NewEngineHandler(book))

	var ids atomic.Uint64
	b.SetParallelism(32) // ~64 clientes simultâneos em 2 CPUs
	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			id := ids.Add(1)
			seq, err := rb.Publish(disruptor.OrderEvent{
				OrderID: id, Price: 1000 + id%64, Quantity: 1, Side: disruptor.Side(id & 1),
			})
			if err == nil {
				err = rb.WaitDurable(seq)
			}
			if err != nil {
				b.Error(err)
				return
			}
		}
	})

	elapsed := time.Since(start)
	b.StopTimer()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rb.Shutdown(ctx)
	wal.Close()

	b.ReportMetric(float64(ids.Load())/elapsed.Seconds(), "orders/s")
	if s := cj.syncs.Load(); s > 0 {
		b.ReportMetric(float64(ids.Load())/float64(s), "orders/fsync")
	}
}
