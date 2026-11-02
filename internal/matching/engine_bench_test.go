package matching

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Benchmarks do motor com mercados diferentes. O mercado "estreito" (64 preços)
// é o melhor caso: o livro fica pequeno. O mercado "largo" imita o `cannon` com
// {{number}} no preço: quase toda ordem cria um nível novo e o livro cresce.

// BenchmarkOrderBook_WidePrices: preços espalhados em 1..1.000.000, lados aleatórios.
func BenchmarkOrderBook_WidePrices(b *testing.B) {
	ob := NewOrderBook(1, nil)
	r := rand.New(rand.NewSource(1))
	orders := make([]Order, b.N)
	for i := range orders {
		orders[i] = Order{ID: uint64(i + 1), Price: uint64(1 + r.Intn(1_000_000)), Quantity: 1, Side: Side(r.Intn(2))}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range orders {
		ob.Process(orders[i])
	}
}

// bookWithLevels cria um livro com `levels` níveis de preço de cada lado,
// bem afastados do meio (bids em 1..levels, asks acima de 10 milhões).
func bookWithLevels(levels int) *OrderBook {
	ob := NewOrderBook(1, nil)
	id := uint64(1)
	for i := 1; i <= levels; i++ {
		ob.Process(Order{ID: id, Price: uint64(i), Quantity: 5, Side: Buy})
		id++
		ob.Process(Order{ID: id, Price: uint64(10_000_000 + i), Quantity: 5, Side: Sell})
		id++
	}
	return ob
}

// BenchmarkSnapshot: custo de UM snapshot do livro (o que o WebSocket pede a cada 200 ms).
func BenchmarkSnapshot(b *testing.B) {
	for _, levels := range []int{100, 10_000, 100_000} {
		b.Run(itoa(levels)+"_niveis", func(b *testing.B) {
			ob := bookWithLevels(levels)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ob.GetSnapshot()
			}
		})
	}
}

// BenchmarkOrderBook_WithSnapshotReader: o motor processando ordens sem parar
// enquanto OUTRA goroutine pede snapshots (como o broadcaster do WebSocket),
// num livro com 100 mil níveis de cada lado. Mostra quanto o leitor atrapalha o motor.
func BenchmarkOrderBook_WithSnapshotReader(b *testing.B) {
	for _, readerPeriod := range []time.Duration{0, 5 * time.Millisecond} {
		name := "sem_leitor"
		if readerPeriod > 0 {
			name = "leitor_a_cada_5ms"
		}
		b.Run(name, func(b *testing.B) {
			ob := bookWithLevels(100_000)

			var stop atomic.Bool
			var wg sync.WaitGroup
			var snaps atomic.Int64
			if readerPeriod > 0 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for !stop.Load() {
						ob.GetSnapshot()
						snaps.Add(1)
						time.Sleep(readerPeriod)
					}
				}()
			}

			b.ReportAllocs()
			b.ResetTimer()
			// Ordens no meio do mercado, que casam entre si (não mexem nos 200 mil níveis).
			for i := 0; i < b.N; i++ {
				id := uint64(i + 1_000_000)
				ob.Process(Order{ID: id, Price: 5_000_000 + id%32, Quantity: 1, Side: Side(id & 1)})
			}
			b.StopTimer()
			stop.Store(true)
			wg.Wait()
			b.ReportMetric(float64(snaps.Load()), "snapshots")
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
