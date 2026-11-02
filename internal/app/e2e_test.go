package app_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FelipeFelipeRenan/goruptor/internal/api"
	"github.com/FelipeFelipeRenan/goruptor/internal/app"
	"github.com/FelipeFelipeRenan/goruptor/internal/cloud"
	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
	"github.com/FelipeFelipeRenan/goruptor/internal/ingest"
	"github.com/FelipeFelipeRenan/goruptor/internal/matching"
	"github.com/FelipeFelipeRenan/goruptor/internal/storage"
)

// stack é o servidor inteiro, em processo, com tudo real exceto o Postgres
// (um sink em memória) e o SQS (um consumidor de canal): TCP -> Ingress -> anel
// -> journaler -> WAL em disco -> motor, mais Batcher e trades.
type stack struct {
	walPath string
	wal     *storage.WAL
	ring    *disruptor.RingBuffer
	book    *matching.OrderBook
	tcp     *api.TCPServer
	ingress *ingest.Ingress
	comps   *app.Components

	processed atomic.Int64 // ordens que o motor executou

	archivedMu sync.Mutex
	archived   []uint64 // ordens que chegaram ao "Postgres"

	tradesSent atomic.Int64 // trades produzidos pelo motor
	tradesRecv atomic.Int64 // trades entregues ao "SQS"
	tradesDone chan struct{}
	tradesCh   chan cloud.TradeEvent
}

type countingHandler struct {
	inner *matching.EngineHandler
	n     *atomic.Int64
}

func (h countingHandler) OnEvent(e disruptor.OrderEvent, seq uint64) {
	h.inner.OnEvent(e, seq)
	h.n.Add(1)
}

func newStack(t *testing.T) *stack {
	t.Helper()
	s := &stack{walPath: filepath.Join(t.TempDir(), "journal.jsonl")}

	var err error
	if s.wal, err = storage.NewWAL(s.walPath); err != nil {
		t.Fatal(err)
	}

	// "SQS": o canal que o motor alimenta e um worker que o consome.
	s.tradesCh = make(chan cloud.TradeEvent, 1<<20)
	s.tradesDone = make(chan struct{})
	go func() {
		for range s.tradesCh {
			s.tradesRecv.Add(1)
		}
		close(s.tradesDone)
	}()
	pub := &cloud.AWSPublisher{TradeCh: s.tradesCh}

	s.book = matching.NewOrderBook(1, pub)
	s.ring = disruptor.NewJournaledRingBuffer(1<<14, s.wal, disruptor.JournalOptions{})
	go s.ring.StartJournaler()
	go s.ring.StartConsumer(countingHandler{matching.NewEngineHandler(s.book), &s.processed})

	// "Postgres": um sink em memória.
	batcher := storage.NewBatcherWithSink(func(_ context.Context, batch []disruptor.OrderEvent) error {
		s.archivedMu.Lock()
		for _, e := range batch {
			s.archived = append(s.archived, e.OrderID)
		}
		s.archivedMu.Unlock()
		return nil
	}, 100, 10*time.Millisecond)

	s.ingress = ingest.New(s.ring, batcher)
	s.tcp = api.NewTCPServer(s.ingress)
	if err := s.tcp.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	go s.tcp.Serve()

	s.comps = &app.Components{
		TCP:         s.tcp,
		Ring:        s.ring,
		WAL:         s.wal,
		Batcher:     batcher,
		CloseTrades: pub.Close,
		TradesDone:  s.tradesDone,
		Logf:        func(string, ...any) {},
	}
	return s
}

func frame(id, price, qty uint64, side byte) []byte {
	b := make([]byte, 25)
	binary.BigEndian.PutUint64(b[0:8], id)
	binary.BigEndian.PutUint64(b[8:16], price)
	binary.BigEndian.PutUint64(b[16:24], qty)
	b[24] = side
	return b
}

// walEvents lê o arquivo do WAL como o recovery faria.
func walEvents(t *testing.T, path string) []disruptor.OrderEvent {
	t.Helper()
	w, err := storage.NewWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	evs, err := w.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// replayTrades reprocessa o WAL num livro novo e conta os trades gerados.
func replayTrades(evs []disruptor.OrderEvent) int {
	ch := make(chan cloud.TradeEvent, 1<<20)
	book := matching.NewOrderBook(1, &cloud.AWSPublisher{TradeCh: ch})
	for _, e := range evs {
		book.Process(matching.Order{ID: e.OrderID, Quantity: e.Quantity, Price: e.Price, Side: matching.Side(e.Side)})
	}
	return len(ch)
}

// ---------------------------------------------------------------------------
// Ponto 3: desligar no meio da carga não perde nenhuma ordem confirmada
// ---------------------------------------------------------------------------

// 24 clientes TCP mandam ordens em loop (cada uma só depois do ACK da anterior).
// No meio disso, dispara-se o Shutdown da MESMA sequência que o main usa.
//
// Depois, cinco conjuntos têm que ser exatamente o mesmo:
//
//	ordens com AckOK == ordens no WAL == ordens executadas pelo motor
//	                 == ordens arquivadas no Postgres
//
// e todo trade que o motor produziu tem que ter sido entregue ao SQS.
func TestGracefulShutdown_UnderLoad_NothingConfirmedIsLost(t *testing.T) {
	for round := 0; round < 8; round++ {
		t.Run(fmt.Sprintf("rodada_%d", round), func(t *testing.T) {
			s := newStack(t)
			addr := s.tcp.Addr().String()

			const clients = 24
			var (
				mu    sync.Mutex
				acked = map[uint64]bool{}
				wg    sync.WaitGroup
			)
			for c := 0; c < clients; c++ {
				wg.Add(1)
				go func(c int) {
					defer wg.Done()
					conn, err := net.Dial("tcp", addr)
					if err != nil {
						return // já desligando
					}
					defer conn.Close()
					for i := 0; i < 1_000_000; i++ {
						id := uint64(c)*1_000_000 + uint64(i) + 1
						if _, err := conn.Write(frame(id, 1000+uint64(i%16), 1, byte(i%2))); err != nil {
							return
						}
						var ack [1]byte
						conn.SetReadDeadline(time.Now().Add(10 * time.Second))
						if _, err := io.ReadFull(conn, ack[:]); err != nil {
							return // servidor encerrou: sem ACK, a ordem NÃO foi confirmada
						}
						if ack[0] != api.AckOK {
							return
						}
						mu.Lock()
						acked[id] = true
						mu.Unlock()
					}
				}(c)
			}

			time.Sleep(time.Duration(150+round*40) * time.Millisecond)

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := s.comps.Shutdown(ctx); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
			wg.Wait()

			// --- conjuntos ---
			inWAL := map[uint64]bool{}
			evs := walEvents(t, s.walPath)
			for _, e := range evs {
				inWAL[e.OrderID] = true
			}
			s.archivedMu.Lock()
			inPG := map[uint64]bool{}
			for _, id := range s.archived {
				inPG[id] = true
			}
			nPG := len(s.archived)
			s.archivedMu.Unlock()

			if len(acked) == 0 {
				t.Fatal("o cenário não confirmou nenhuma ordem; teste inútil")
			}
			// 1) Toda ordem confirmada ao cliente está no WAL, no motor e no Postgres.
			for id := range acked {
				if !inWAL[id] {
					t.Fatalf("ordem %d confirmada ao cliente mas ausente do WAL", id)
				}
				if !inPG[id] {
					t.Fatalf("ordem %d confirmada ao cliente mas ausente do Postgres", id)
				}
			}
			// 2) Nada além do confirmado foi executado: WAL == motor == Postgres == ACKs.
			if len(inWAL) != len(acked) || int(s.processed.Load()) != len(acked) || nPG != len(acked) || len(evs) != len(acked) {
				t.Fatalf("conjuntos divergentes: ACKs=%d WAL=%d (únicas=%d) motor=%d Postgres=%d",
					len(acked), len(evs), len(inWAL), s.processed.Load(), nPG)
			}
			// 3) Todos os trades do motor chegaram ao SQS antes de o processo "sair".
			want := replayTrades(evs)
			if want == 0 {
				t.Fatal("o cenário deveria gerar trades")
			}
			if int(s.tradesRecv.Load()) != want {
				t.Fatalf("trades: motor gerou %d, SQS recebeu %d", want, s.tradesRecv.Load())
			}
			// 4) Depois do desligamento, a porta está fechada e o anel recusa.
			if c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
				c.Close()
				t.Fatal("a porta TCP ainda aceita conexões")
			}
			if _, err := s.ring.Publish(disruptor.OrderEvent{OrderID: 1}); !errors.Is(err, disruptor.ErrClosed) {
				t.Fatalf("o anel deveria recusar com ErrClosed, veio %v", err)
			}
			t.Logf("%d ordens confirmadas, %d trades: ACKs == WAL == motor == Postgres; SQS completo", len(acked), want)
		})
	}
}

// ---------------------------------------------------------------------------
// Ponto 4: a mesma ordem enviada por muitos clientes ao mesmo tempo roda UMA vez
// ---------------------------------------------------------------------------

// 16 clientes disparam os MESMOS 400 order_ids, em paralelo, pelo servidor real.
// Tem que haver exatamente 400 AckOK no total; o resto é AckDuplicate; o WAL e o
// motor veem cada ordem uma única vez.
func TestDuplicateStorm_EachOrderIDExecutesExactlyOnce(t *testing.T) {
	s := newStack(t)
	addr := s.tcp.Addr().String()

	const clients, ids = 16, 400
	var ok, dup, other atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			<-start
			for i := 1; i <= ids; i++ {
				conn.Write(frame(uint64(i), 1000+uint64(i%8), 1, byte(i%2)))
				var ack [1]byte
				conn.SetReadDeadline(time.Now().Add(10 * time.Second))
				if _, err := io.ReadFull(conn, ack[:]); err != nil {
					t.Errorf("cliente %d: %v", c, err)
					return
				}
				switch ack[0] {
				case api.AckOK:
					ok.Add(1)
				case api.AckDuplicate:
					dup.Add(1)
				default:
					other.Add(1)
				}
			}
		}(c)
	}
	close(start)
	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.comps.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	if ok.Load() != ids || dup.Load() != ids*(clients-1) || other.Load() != 0 {
		t.Fatalf("AckOK=%d (esperava %d) AckDuplicate=%d (esperava %d) outros=%d",
			ok.Load(), ids, dup.Load(), ids*(clients-1), other.Load())
	}
	evs := walEvents(t, s.walPath)
	seen := map[uint64]int{}
	for _, e := range evs {
		seen[e.OrderID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("order_id %d aparece %d vezes no WAL", id, n)
		}
	}
	if len(evs) != ids || int(s.processed.Load()) != ids {
		t.Fatalf("WAL=%d motor=%d; esperava %d de cada", len(evs), s.processed.Load(), ids)
	}
}

// Reinício: os IDs do WAL semeiam o novo Ingress, então reenviar uma ordem
// antiga depois de um restart continua sendo recusado.
func TestRestart_ReplayedIDsStillRejectedAsDuplicates(t *testing.T) {
	s := newStack(t)
	conn, _ := net.Dial("tcp", s.tcp.Addr().String())
	for i := 1; i <= 20; i++ {
		conn.Write(frame(uint64(i), 1000, 1, 0))
		var ack [1]byte
		io.ReadFull(conn, ack[:])
	}
	conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.comps.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	// "Novo processo": o Ingress nasce vazio e é semeado com o WAL, como no main.
	evs := walEvents(t, s.walPath)
	if len(evs) != 20 {
		t.Fatalf("WAL tem %d ordens, esperava 20", len(evs))
	}
	in := ingest.New(noopPublisher{}, noopArchiver{})
	for _, e := range evs {
		in.IDs().Register(e.OrderID)
	}
	err := in.Submit(disruptor.OrderEvent{OrderID: 7, Price: 1000, Quantity: 1})
	if !errors.Is(err, ingest.ErrDuplicateOrder) {
		t.Fatalf("reenvio após restart deveria ser duplicata, veio %v", err)
	}
	if err := in.Submit(disruptor.OrderEvent{OrderID: 21, Price: 1000, Quantity: 1}); err != nil {
		t.Fatalf("ordem nova deveria passar: %v", err)
	}
}

type noopPublisher struct{}

func (noopPublisher) Publish(disruptor.OrderEvent) (uint64, error) { return 0, nil }
func (noopPublisher) WaitDurable(uint64) error                     { return nil }

type noopArchiver struct{}

func (noopArchiver) Push(disruptor.OrderEvent) error { return nil }
