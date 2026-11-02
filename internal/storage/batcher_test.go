package storage

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
)

// recSink registra os lotes que o Batcher entregou (copiando, porque o slice é reutilizado).
type recSink struct {
	mu      sync.Mutex
	batches [][]disruptor.OrderEvent
	delay   time.Duration
	failN   int // as primeiras failN chamadas falham
	calls   int
}

func (s *recSink) sink(ctx context.Context, batch []disruptor.OrderEvent) error {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls <= s.failN {
		return errors.New("postgres fora do ar (injetado)")
	}
	s.batches = append(s.batches, append([]disruptor.OrderEvent(nil), batch...))
	return nil
}

func (s *recSink) ids() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []uint64
	for _, b := range s.batches {
		for _, e := range b {
			out = append(out, e.OrderID)
		}
	}
	return out
}

func (s *recSink) sizes() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int
	for _, b := range s.batches {
		out = append(out, len(b))
	}
	return out
}

func ctxTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func ev(id uint64) disruptor.OrderEvent {
	return disruptor.OrderEvent{OrderID: id, Price: 1, Quantity: 1}
}

func TestBatcher_FlushesBySizeAndCloseFlushesTheRest(t *testing.T) {
	rs := &recSink{}
	b := NewBatcherWithSink(rs.sink, 10, time.Hour) // sem flush por tempo
	for i := uint64(1); i <= 25; i++ {
		if err := b.Push(ev(i)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := ctxTimeout(5 * time.Second)
	defer cancel()
	if err := b.Close(ctx); err != nil {
		t.Fatal(err)
	}

	if got := rs.sizes(); len(got) != 3 || got[0] != 10 || got[1] != 10 || got[2] != 5 {
		t.Fatalf("lotes %v; esperava [10 10 5] (o último é o flush final do Close)", got)
	}
	for i, id := range rs.ids() {
		if id != uint64(i+1) {
			t.Fatalf("ordem trocada na posição %d: id=%d", i, id)
		}
	}
}

func TestBatcher_FlushesByInterval(t *testing.T) {
	rs := &recSink{}
	b := NewBatcherWithSink(rs.sink, 1000, 20*time.Millisecond)
	defer b.Close(context.Background())

	b.Push(ev(1))
	b.Push(ev(2))
	deadline := time.Now().Add(2 * time.Second)
	for len(rs.ids()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("o lote parcial nunca foi gravado pelo timer")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Fila grande + sink lento: Close tem que esperar TUDO ser gravado, não só o lote atual.
func TestBatcher_CloseDrainsTheWholeBacklog(t *testing.T) {
	rs := &recSink{delay: 2 * time.Millisecond}
	b := NewBatcherWithSink(rs.sink, 50, time.Hour)
	const n = 2000
	for i := uint64(1); i <= n; i++ {
		b.Push(ev(i))
	}
	ctx, cancel := ctxTimeout(30 * time.Second)
	defer cancel()
	if err := b.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(rs.ids()); got != n {
		t.Fatalf("gravadas %d de %d ordens após Close", got, n)
	}
}

func TestBatcher_PushAfterCloseFailsAndCloseIsIdempotent(t *testing.T) {
	b := NewBatcherWithSink((&recSink{}).sink, 10, time.Hour)
	ctx, cancel := ctxTimeout(5 * time.Second)
	defer cancel()
	if err := b.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(ctx); err != nil {
		t.Fatalf("segundo Close: %v", err)
	}
	if err := b.Push(ev(1)); !errors.Is(err, ErrBatcherClosed) {
		t.Fatalf("esperava ErrBatcherClosed, veio %v", err)
	}
}

// Uma falha do Postgres perde aquele lote (limitação conhecida e logada), mas o
// worker não pode morrer: os lotes seguintes precisam continuar sendo gravados.
func TestBatcher_SinkErrorDoesNotKillTheWorker(t *testing.T) {
	rs := &recSink{failN: 1}
	b := NewBatcherWithSink(rs.sink, 5, time.Hour)
	for i := uint64(1); i <= 10; i++ {
		b.Push(ev(i))
	}
	ctx, cancel := ctxTimeout(5 * time.Second)
	defer cancel()
	b.Close(ctx)

	ids := rs.ids()
	if len(ids) != 5 || ids[0] != 6 {
		t.Fatalf("esperava só o 2º lote (6..10) gravado, veio %v", ids)
	}
}

// Sink travado: o contexto do Close manda. O INSERT em andamento é abortado.
func TestBatcher_CloseRespectsContextAndAbortsStuckSink(t *testing.T) {
	var aborted atomic.Bool
	stuck := func(ctx context.Context, _ []disruptor.OrderEvent) error {
		<-ctx.Done() // só termina quando o Batcher cancela o INSERT
		aborted.Store(true)
		return ctx.Err()
	}
	b := NewBatcherWithSink(stuck, 1, time.Hour)
	b.Push(ev(1))
	time.Sleep(20 * time.Millisecond) // o worker já está dentro do sink

	ctx, cancel := ctxTimeout(100 * time.Millisecond)
	defer cancel()
	start := time.Now()
	err := b.Close(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("esperava DeadlineExceeded, veio %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Close demorou %v", d)
	}
	if !aborted.Load() {
		t.Fatal("o INSERT em andamento deveria ter sido abortado")
	}
}

// Pushers concorrentes com o Close: todo Push que retornou nil tem que ser
// gravado exatamente uma vez; nenhum pode causar panic (send em canal fechado).
func TestBatcher_ConcurrentPushAndCloseLosesNothing(t *testing.T) {
	for round := 0; round < 20; round++ {
		rs := &recSink{}
		b := NewBatcherWithSink(rs.sink, 7, 5*time.Millisecond)

		var accepted sync.Map
		var nAccepted atomic.Int64
		var wg sync.WaitGroup
		for p := 0; p < 8; p++ {
			wg.Add(1)
			go func(p int) {
				defer wg.Done()
				for i := 0; ; i++ {
					id := uint64(p)<<32 | uint64(i)
					if err := b.Push(ev(id)); err != nil {
						if !errors.Is(err, ErrBatcherClosed) {
							t.Errorf("erro inesperado: %v", err)
						}
						return
					}
					accepted.Store(id, true)
					nAccepted.Add(1)
				}
			}(p)
		}
		time.Sleep(time.Duration(round%4) * time.Millisecond)
		ctx, cancel := ctxTimeout(10 * time.Second)
		if err := b.Close(ctx); err != nil {
			t.Fatalf("rodada %d: %v", round, err)
		}
		cancel()
		wg.Wait()

		ids := rs.ids()
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for i := 1; i < len(ids); i++ {
			if ids[i] == ids[i-1] {
				t.Fatalf("rodada %d: ordem %x gravada duas vezes", round, ids[i])
			}
		}
		if int64(len(ids)) != nAccepted.Load() {
			t.Fatalf("rodada %d: aceitas=%d gravadas=%d", round, nAccepted.Load(), len(ids))
		}
	}
}

func TestNewBatcher_RejectsBatchSizesPostgresCannotHandle(t *testing.T) {
	for _, n := range []int{0, -1, maxBatchSize + 1, 100000} {
		if _, err := NewBatcher("postgres://nao-conecta", n); err == nil {
			t.Errorf("batchSize %d deveria ser rejeitado (limite de 65535 parâmetros do Postgres)", n)
		}
	}
}
