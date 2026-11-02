package matching

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/FelipeFelipeRenan/goruptor/internal/cloud"
)

// ---------------------------------------------------------------------------
// Modelo de referência: a regra de preço-tempo escrita da forma mais ingênua
// possível (listas simples, busca linear, sem heap, sem mapa de níveis).
//
// Serve de "oráculo": se o motor otimizado e este modelo concordarem em milhares
// de ordens aleatórias (trades, ordem dos trades e estado final do livro),
// uma otimização não mudou o comportamento.
// ---------------------------------------------------------------------------

type resting struct {
	id, qty, price, seq uint64
}

type trade struct{ price, qty uint64 }

type refBook struct {
	bids, asks []resting
	seq        uint64
	trades     []trade
}

func (r *refBook) process(o Order) {
	r.seq++
	qty := o.Quantity

	if o.Side == Buy {
		for qty > 0 {
			i := bestIndex(r.asks, func(a, b resting) bool { return a.price < b.price })
			if i < 0 || o.Price < r.asks[i].price {
				break
			}
			qty = r.fill(&r.asks, i, qty)
		}
	} else {
		for qty > 0 {
			i := bestIndex(r.bids, func(a, b resting) bool { return a.price > b.price })
			if i < 0 || o.Price > r.bids[i].price {
				break
			}
			qty = r.fill(&r.bids, i, qty)
		}
	}

	if qty > 0 { // a sobra descansa no fim da fila do seu preço
		side := &r.asks
		if o.Side == Buy {
			side = &r.bids
		}
		*side = append(*side, resting{id: o.ID, qty: qty, price: o.Price, seq: r.seq})
	}
}

// fill executa um trade contra book[i] ao preço da ordem que estava descansando.
func (r *refBook) fill(book *[]resting, i int, qty uint64) uint64 {
	b := *book
	q := qty
	if b[i].qty < q {
		q = b[i].qty
	}
	r.trades = append(r.trades, trade{price: b[i].price, qty: q})
	b[i].qty -= q
	if b[i].qty == 0 {
		*book = append(b[:i], b[i+1:]...)
	}
	return qty - q
}

// bestIndex acha o melhor preço; no empate de preço vence a ordem mais antiga (menor seq).
func bestIndex(book []resting, better func(a, b resting) bool) int {
	best := -1
	for i, o := range book {
		if best < 0 || better(o, book[best]) || (!better(book[best], o) && o.seq < book[best].seq) {
			best = i
		}
	}
	return best
}

// dump gera exatamente o mesmo texto de dumpBook (recovery_test.go).
func (r *refBook) dump() string {
	var sb strings.Builder
	side := func(name string, book []resting) {
		levels := map[uint64][]resting{}
		for _, o := range book {
			levels[o.price] = append(levels[o.price], o)
		}
		prices := make([]uint64, 0, len(levels))
		for p := range levels {
			prices = append(prices, p)
		}
		sort.Slice(prices, func(i, j int) bool { return prices[i] < prices[j] })
		for _, p := range prices {
			orders := levels[p]
			sort.Slice(orders, func(i, j int) bool { return orders[i].seq < orders[j].seq })
			var vol uint64
			for _, o := range orders {
				vol += o.qty
			}
			fmt.Fprintf(&sb, "%s %d vol=%d:", name, p, vol)
			for _, o := range orders {
				fmt.Fprintf(&sb, " [%d x%d]", o.id, o.qty)
			}
			sb.WriteByte('\n')
		}
	}
	side("BID", r.bids)
	side("ASK", r.asks)

	var bestBid, bestAsk uint64
	for _, o := range r.bids {
		if o.price > bestBid {
			bestBid = o.price
		}
	}
	for _, o := range r.asks {
		if bestAsk == 0 || o.price < bestAsk {
			bestAsk = o.price
		}
	}
	fmt.Fprintf(&sb, "bestBid=%d bestAsk=%d", bestBid, bestAsk)
	return sb.String()
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// newCapturedBook devolve um livro cujos trades podem ser lidos depois.
// O canal precisa caber todos os trades (o motor descarta se estiver cheio).
func newCapturedBook(maxTrades int) (*OrderBook, chan cloud.TradeEvent) {
	ch := make(chan cloud.TradeEvent, maxTrades)
	return NewOrderBook(1, &cloud.AWSPublisher{TradeCh: ch}), ch
}

func capturedTrades(ch chan cloud.TradeEvent) []trade {
	var out []trade
	for {
		select {
		case t := <-ch:
			out = append(out, trade{price: t.Price, qty: t.Quantity})
		default:
			return out
		}
	}
}

// compare roda a mesma sequência no motor e no modelo e exige resultados idênticos.
func compare(t testing.TB, orders []Order) {
	t.Helper()
	engine, ch := newCapturedBook(2*len(orders) + 16)
	ref := &refBook{}

	for i, o := range orders {
		engine.Process(o)
		ref.process(o)

		// Estado idêntico depois de CADA ordem: um defeito aparece na ordem que o causou.
		if i%16 == 0 || i == len(orders)-1 {
			if a, b := dumpBook(engine), ref.dump(); a != b {
				t.Fatalf("estado diverge após a ordem #%d %+v\n--- motor ---\n%s\n--- referência ---\n%s", i, o, a, b)
			}
		}
	}

	got, want := capturedTrades(ch), ref.trades
	if len(got) != len(want) {
		t.Fatalf("quantidade de trades: motor=%d referência=%d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("trade #%d: motor=%+v referência=%+v", i, got[i], want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Testes
// ---------------------------------------------------------------------------

// Milhares de ordens aleatórias em vários "mercados": poucos níveis com filas
// longas (muito empate de preço), muitos níveis, quantidades grandes e pequenas,
// ordens de quantidade zero.
func TestOrderBook_MatchesReferenceModel(t *testing.T) {
	markets := []struct {
		name         string
		levels       int
		maxQty       int
		allowZeroQty bool
	}{
		{"3 níveis, filas longas", 3, 50, false},
		{"20 níveis", 20, 9, false},
		{"400 níveis, quantidade pequena", 400, 3, false},
		{"com quantidade zero", 10, 5, true},
	}
	for _, m := range markets {
		for seed := int64(1); seed <= 3; seed++ {
			t.Run(fmt.Sprintf("%s/seed%d", m.name, seed), func(t *testing.T) {
				r := rand.New(rand.NewSource(seed))
				orders := make([]Order, 2500)
				for i := range orders {
					qty := uint64(1 + r.Intn(m.maxQty))
					if m.allowZeroQty && r.Intn(8) == 0 {
						qty = 0
					}
					orders[i] = Order{
						ID:       uint64(i + 1),
						Price:    uint64(1000 + r.Intn(m.levels)),
						Quantity: qty,
						Side:     Side(r.Intn(2)),
					}
				}
				compare(t, orders)
			})
		}
	}
}

// Mercado com tendência: o preço passeia, então o livro esvazia e reabre níveis
// o tempo todo (exercita criar/remover níveis e atualizar melhor bid/ask).
func TestOrderBook_MatchesReferenceModel_DriftingMarket(t *testing.T) {
	r := rand.New(rand.NewSource(99))
	mid := 5000
	orders := make([]Order, 8000)
	for i := range orders {
		mid += r.Intn(5) - 2
		orders[i] = Order{
			ID:       uint64(i + 1),
			Price:    uint64(mid + r.Intn(7) - 3),
			Quantity: uint64(1 + r.Intn(6)),
			Side:     Side(r.Intn(2)),
		}
	}
	compare(t, orders)
}

// FuzzOrderBook deixa o fuzzer do Go procurar uma sequência que faça o motor
// divergir do modelo. `go test` roda só as sementes; para caçar de verdade:
//
//	go test -fuzz FuzzOrderBook -fuzztime 60s ./internal/matching/
func FuzzOrderBook(f *testing.F) {
	f.Add([]byte{0, 5, 3, 0, 1, 5, 3, 0, 0, 4, 9, 0})
	f.Add([]byte{1, 1, 1, 1, 0, 1, 1, 1, 1, 1, 1, 1, 0, 1, 1, 1})
	f.Add([]byte{0, 9, 0, 0, 1, 9, 5, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		var orders []Order
		for i := 0; i+3 < len(data); i += 4 {
			orders = append(orders, Order{
				ID:       uint64(len(orders) + 1),
				Side:     Side(data[i] & 1),
				Price:    uint64(1 + data[i+1]%32), // faixa estreita: muito casamento
				Quantity: uint64(data[i+2] % 8),    // 0..7 (inclui zero)
			})
		}
		compare(t, orders)
	})
}

// ---------------------------------------------------------------------------
// Regras de negócio, explícitas
// ---------------------------------------------------------------------------

// Prioridade de tempo: no mesmo preço, quem chegou primeiro é executado primeiro.
func TestOrderBook_TimePriorityWithinALevel(t *testing.T) {
	ob, ch := newCapturedBook(16)
	ob.Process(Order{ID: 1, Quantity: 3, Price: 100, Side: Sell})
	ob.Process(Order{ID: 2, Quantity: 3, Price: 100, Side: Sell})
	ob.Process(Order{ID: 3, Quantity: 4, Price: 100, Side: Buy}) // come a #1 inteira e 1 da #2

	want := "ASK 100 vol=2: [2 x2]\nbestBid=0 bestAsk=100"
	if got := dumpBook(ob); got != want {
		t.Fatalf("a ordem #1 deveria ter sido executada antes da #2\n%s", got)
	}
	if tr := capturedTrades(ch); len(tr) != 2 || tr[0].qty != 3 || tr[1].qty != 1 {
		t.Fatalf("trades inesperados: %+v", tr)
	}
}

// O trade acontece ao preço de quem estava descansando, não ao da ordem agressora.
func TestOrderBook_TradeExecutesAtTheRestingPrice(t *testing.T) {
	ob, ch := newCapturedBook(16)
	ob.Process(Order{ID: 1, Quantity: 5, Price: 100, Side: Sell})
	ob.Process(Order{ID: 2, Quantity: 5, Price: 120, Side: Buy}) // aceitaria até 120

	if tr := capturedTrades(ch); len(tr) != 1 || tr[0].price != 100 {
		t.Fatalf("esperava 1 trade a 100, veio %+v", tr)
	}
}

// Uma ordem agressora varre vários níveis, do melhor para o pior preço.
func TestOrderBook_SweepsLevelsBestPriceFirst(t *testing.T) {
	ob, ch := newCapturedBook(16)
	ob.Process(Order{ID: 1, Quantity: 2, Price: 103, Side: Sell})
	ob.Process(Order{ID: 2, Quantity: 2, Price: 101, Side: Sell})
	ob.Process(Order{ID: 3, Quantity: 2, Price: 102, Side: Sell})
	ob.Process(Order{ID: 4, Quantity: 5, Price: 103, Side: Buy})

	tr := capturedTrades(ch)
	if len(tr) != 3 || tr[0].price != 101 || tr[1].price != 102 || tr[2].price != 103 {
		t.Fatalf("ordem de execução errada: %+v", tr)
	}
	if got, want := dumpBook(ob), "ASK 103 vol=1: [1 x1]\nbestBid=0 bestAsk=103"; got != want {
		t.Fatalf("estado final errado:\n%s", got)
	}
}

// No modo restauração (replay do WAL) o livro reconstrói o estado SEM reenviar trades.
func TestOrderBook_RestoreModeRebuildsStateWithoutPublishingTrades(t *testing.T) {
	ob, ch := newCapturedBook(16)
	ob.SetRestoreMode(true)
	ob.Process(Order{ID: 1, Quantity: 5, Price: 100, Side: Sell})
	ob.Process(Order{ID: 2, Quantity: 3, Price: 100, Side: Buy})
	ob.SetRestoreMode(false)

	if tr := capturedTrades(ch); len(tr) != 0 {
		t.Fatalf("o replay não pode publicar trades: %+v", tr)
	}
	if got, want := dumpBook(ob), "ASK 100 vol=2: [1 x2]\nbestBid=0 bestAsk=100"; got != want {
		t.Fatalf("estado reconstruído errado:\n%s", got)
	}
}
