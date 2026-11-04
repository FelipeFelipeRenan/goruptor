package matching

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FelipeFelipeRenan/goruptor/internal/cloud"
	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
	"github.com/FelipeFelipeRenan/goruptor/internal/storage"
)

// dumpBook serializa o estado COMPLETO do livro (cada ordem, em cada nível, na
// ordem da fila). Dois livros com o mesmo dump são indistinguíveis.
func dumpBook(ob *OrderBook) string {
	ob.mu.RLock()
	defer ob.mu.RUnlock()

	var sb strings.Builder
	side := func(name string, m map[uint64]*PriceLevel) {
		prices := make([]uint64, 0, len(m))
		for p := range m {
			prices = append(prices, p)
		}
		sort.Slice(prices, func(i, j int) bool { return prices[i] < prices[j] })
		for _, p := range prices {
			lvl := m[p]
			fmt.Fprintf(&sb, "%s %d vol=%d:", name, p, lvl.TotalVolume)
			for e := lvl.Orders.Front(); e != nil; e = e.Next() {
				o := e.Value.(Order)
				fmt.Fprintf(&sb, " [%d x%d]", o.ID, o.Quantity)
			}
			sb.WriteByte('\n')
		}
	}
	side("BID", ob.bidsMap)
	side("ASK", ob.asksMap)
	fmt.Fprintf(&sb, "bestBid=%d bestAsk=%d", ob.bestBid, ob.bestAsk)
	return sb.String()
}

func drain(ch chan cloud.TradeEvent) []cloud.TradeEvent {
	var out []cloud.TradeEvent
	for {
		select {
		case t := <-ch:
			out = append(out, t)
		default:
			return out
		}
	}
}

// randomOrder gera ordens numa faixa estreita de preço para haver muito
// matching, execuções parciais e varredura de vários níveis.
func randomOrder(r *rand.Rand, id uint64) disruptor.OrderEvent {
	return disruptor.OrderEvent{
		OrderID:  id,
		Price:    uint64(990 + r.Intn(21)),
		Quantity: uint64(1 + r.Intn(9)),
		Side:     disruptor.Side(r.Intn(2)),
	}
}

// O teste que fecha o ponto 2.
//
// Cenário: vários produtores concorrentes, WAL real em disco, motor real.
// Depois de tudo, reinicia: um livro novo reproduz o WAL e TEM que ficar
// idêntico ao livro vivo, com a mesma sequência de trades.
//
// Isso só vale se a ordem do arquivo for a ordem do anel. No desenho antigo
// (cada goroutine fazia wal.Log e depois Publish), a ordem do arquivo era
// decidida por uma corrida diferente da que decidia a ordem do motor, então o
// replay reconstruía um livro diferente do que estava na memória.
func TestRecovery_ConcurrentProducersReplayReproducesLiveBook(t *testing.T) {
	const producers, perProducer = 8, 2500
	total := producers * perProducer

	walPath := filepath.Join(t.TempDir(), "journal.wal")
	wal, err := storage.NewWAL(walPath)
	if err != nil {
		t.Fatal(err)
	}

	liveTrades := make(chan cloud.TradeEvent, total*2)
	live := NewOrderBook(1, &cloud.AWSPublisher{TradeCh: liveTrades})
	rb := disruptor.NewJournaledRingBuffer(1024, wal, disruptor.JournalOptions{})
	go rb.StartJournaler()
	go rb.StartConsumer(NewEngineHandler(live))

	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(p) + 1))
			for i := 0; i < perProducer; i++ {
				id := uint64(p*perProducer + i + 1)
				seq, err := rb.Publish(randomOrder(r, id))
				if err == nil {
					err = rb.WaitDurable(seq)
				}
				if err != nil {
					t.Errorf("ordem %d: %v", id, err)
					return
				}
			}
		}(p)
	}
	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rb.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := wal.Close(); err != nil {
		t.Fatal(err)
	}

	// --- "reinício do processo": WAL novo sobre o mesmo arquivo + livro novo ---
	wal2, err := storage.NewWAL(walPath)
	if err != nil {
		t.Fatal(err)
	}
	defer wal2.Close()
	past, err := wal2.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(past) != total {
		t.Fatalf("WAL tem %d ordens, esperava %d", len(past), total)
	}

	replayTrades := make(chan cloud.TradeEvent, total*2)
	replayed := NewOrderBook(1, &cloud.AWSPublisher{TradeCh: replayTrades})
	for _, e := range past {
		replayed.Process(Order{ID: e.OrderID, Quantity: e.Quantity, Price: e.Price, Side: Side(e.Side)})
	}

	if a, b := dumpBook(live), dumpBook(replayed); a != b {
		t.Fatalf("livro após replay difere do livro vivo\n--- vivo ---\n%s\n--- replay ---\n%s", a, b)
	}

	lt, rt := drain(liveTrades), drain(replayTrades)
	if len(lt) == 0 {
		t.Fatal("o cenário deveria gerar trades")
	}
	if len(lt) != len(rt) {
		t.Fatalf("trades: vivo=%d replay=%d", len(lt), len(rt))
	}
	for i := range lt {
		if lt[i] != rt[i] {
			t.Fatalf("trade %d difere: vivo=%+v replay=%+v", i, lt[i], rt[i])
		}
	}
	t.Logf("%d ordens, %d trades: livro e trades idênticos após replay", total, len(lt))
}

// "ACK implica em disco", observado de fora. Enquanto os produtores ainda estão
// rodando, um leitor independente lê o arquivo CRU várias vezes. Toda ordem
// confirmada ANTES de cada leitura tem que estar no arquivo naquele instante,
// ou seja, o cliente nunca recebe um ACK de algo que um crash perderia.
func TestRecovery_AckedOrdersAreAlwaysInTheFile(t *testing.T) {
	const producers, perProducer = 6, 1500

	walPath := filepath.Join(t.TempDir(), "journal.wal")
	wal, err := storage.NewWAL(walPath)
	if err != nil {
		t.Fatal(err)
	}
	book := NewOrderBook(1, nil)
	rb := disruptor.NewJournaledRingBuffer(512, wal, disruptor.JournalOptions{})
	go rb.StartJournaler()
	go rb.StartConsumer(NewEngineHandler(book))

	var mu sync.Mutex
	var acked []uint64

	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(p) + 100))
			for i := 0; i < perProducer; i++ {
				id := uint64(p*perProducer + i + 1)
				seq, err := rb.Publish(randomOrder(r, id))
				if err == nil {
					err = rb.WaitDurable(seq)
				}
				if err != nil {
					t.Errorf("ordem %d: %v", id, err)
					return
				}
				mu.Lock()
				acked = append(acked, id)
				mu.Unlock()
			}
		}(p)
	}

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()

	checks := 0
	for done := false; !done; {
		select {
		case <-finished:
			done = true
		case <-time.After(2 * time.Millisecond):
		}

		mu.Lock()
		before := append([]uint64(nil), acked...) // confirmadas ANTES da leitura
		mu.Unlock()

		// Leitor independente (só leitura): o arquivo pode estar sendo escrito agora,
		// e uma cauda incompleta é ignorada.
		onDisk, err := storage.ScanFile(walPath)
		if err != nil {
			t.Fatal(err)
		}
		inFile := make(map[uint64]bool, len(before))
		for _, e := range onDisk {
			inFile[e.OrderID] = true
		}
		for _, id := range before {
			if !inFile[id] {
				t.Fatalf("ordem %d já tinha sido confirmada ao cliente mas não estava no arquivo", id)
			}
		}
		checks++
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rb.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	wal.Close()
	t.Logf("%d leituras independentes do arquivo durante a carga, nenhum ACK adiantado", checks)
}
