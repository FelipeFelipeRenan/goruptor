package matching

import (
	"math/rand"
	"reflect"
	"sort"
	"testing"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
)

func TestOrderBook_MatchingLogic(t *testing.T) {
	// 1. Inicializamos o motor para o ativo 1 (BTC/USD)
	ob := NewOrderBook(1, nil)

	// 2. Criamos as ordens dos Vendedores (Asks)
	sell1 := Order{ID: 1, Side: Sell, Price: 65000, Quantity: 10}
	sell2 := Order{ID: 2, Side: Sell, Price: 65100, Quantity: 5}

	// Jogamos no motor (eles vão descansar porque não tem compradores)
	ob.Process(sell1)
	ob.Process(sell2)

	// Validação Intermediária: O Heap funcionou? O bestAsk tem que ser o menor preço (65000)
	if ob.bestAsk != 65000 {
		t.Fatalf("Best Ask incorreto. Esperado: 65000, Recebido: %d", ob.bestAsk)
	}

	// 3. Soltamos o Predador (Compra)
	buy1 := Order{ID: 3, Side: Buy, Price: 65000, Quantity: 12}
	ob.Process(buy1)

	// 4. Verificações do Estado Final

	// A: O bestAsk subiu para 65100 (já que a de 65000 foi devorada)?
	if ob.bestAsk != 65100 {
		t.Errorf("Best Ask não atualizou corretamente. Esperado: 65100, Recebido: %d", ob.bestAsk)
	}

	// B: A prateleira de 65000 nas Vendas foi deletada do mapa?
	if _, exists := ob.asksMap[65000]; exists {
		t.Errorf("A prateleira de Venda a 65000 deveria estar vazia e não existir mais no mapa.")
	}

	// C: O bestBid agora é 65000 (a sobra da nossa ordem)?
	if ob.bestBid != 65000 {
		t.Errorf("Best Bid incorreto. Esperado: 65000, Recebido: %d", ob.bestBid)
	}

	// D: A prateleira de Compras a 65000 foi criada?
	buyLevel, exists := ob.bidsMap[65000]
	if !exists {
		t.Fatalf("A ordem de sobra não descansou no livro de compras.")
	}

	// E: A sobra da ordem de compra tem exatamente 2 de quantidade?
	if buyLevel.TotalVolume != 2 {
		t.Errorf("Volume restante incorreto na compra. Esperado: 2, Recebido: %d", buyLevel.TotalVolume)
	}

	// Se chegou até aqui sem erros, o teste passou!
	t.Log("Todos os cruzamentos, parciais e atualizações de Heap funcionaram perfeitamente!")
}

func TestOrderBook_PartialFill(t *testing.T) {
	// 1. O Isolamento: Iniciamos o Livro sem a AWS (passamos 'nil')
	// Não queremos que o teste unitário dependa da internet ou do Docker!
	ob := NewOrderBook(1, nil)

	// 2. O Cenário: Entra um Vendedor oferecendo 8 BTC a $65.000
	sellOrder := Order{
		ID:       1,
		Quantity: 8,
		Price:    65000,
		Side:     Side(disruptor.Sell),
	}
	ob.Process(sellOrder)

	// Validação de Sanidade: O vendedor realmente foi pra prateleira?
	if ob.asksMap[65000] == nil || ob.asksMap[65000].TotalVolume != 8 {
		t.Fatalf("❌ Erro: O vendedor não foi colocado na prateleira corretamente.")
	}

	// 3. O Ataque: Entra um Comprador querendo 10 BTC a $65.000
	buyOrder := Order{
		ID:       2,
		Quantity: 10,
		Price:    65000,
		Side:     Side(disruptor.Buy),
	}
	ob.Process(buyOrder)

	// 4. A Prova Matemática (As Asserções)

	// Regra A: O Vendedor foi totalmente consumido? A prateleira dele tem que estar vazia.
	if ob.asksMap[65000] != nil && ob.asksMap[65000].TotalVolume > 0 {
		t.Errorf("❌ Erro: A ordem de venda de 8 BTC deveria ter sido totalmente consumida!")
	}

	// Regra B: O Comprador recebeu o troco? Ele tem que estar descansando com 2 BTC.
	if ob.bidsMap[65000] == nil {
		t.Fatalf("❌ Erro: A prateleira de compras sumiu com o troco do cliente!")
	}

	if ob.bidsMap[65000].TotalVolume != 2 {
		t.Errorf("❌ Erro de Partial Fill: Esperava 2 BTC sobrando, mas o motor registrou %d BTC", ob.bidsMap[65000].TotalVolume)
	}
}

// referenceSnapshot é a implementação ANTIGA do snapshot (copiar e ordenar todos
// os preços), mantida aqui como oráculo do GetSnapshot de custo constante.
func referenceSnapshot(ob *OrderBook) BookSnapshot {
	snap := BookSnapshot{Asks: []OrderSnapshot{}, Bids: []OrderSnapshot{}}
	var asks, bids []uint64
	for p := range ob.asksMap {
		asks = append(asks, p)
	}
	for p := range ob.bidsMap {
		bids = append(bids, p)
	}
	sort.Slice(asks, func(i, j int) bool { return asks[i] < asks[j] })
	sort.Slice(bids, func(i, j int) bool { return bids[i] > bids[j] })
	for i := 0; i < len(asks) && i < 10; i++ {
		snap.Asks = append(snap.Asks, OrderSnapshot{Price: asks[i], Quantity: ob.asksMap[asks[i]].TotalVolume})
	}
	for i := 0; i < len(bids) && i < 10; i++ {
		snap.Bids = append(snap.Bids, OrderSnapshot{Price: bids[i], Quantity: ob.bidsMap[bids[i]].TotalVolume})
	}
	return snap
}

// O snapshot de custo constante tem de ser idêntico ao antigo (que ordenava
// tudo) em livros de todos os tamanhos: vazio, com menos de 10 níveis, com
// milhares de níveis, e depois de muitas execuções (o heap fica "bagunçado").
func TestGetSnapshot_MatchesFullSortReference(t *testing.T) {
	for _, tc := range []struct {
		name   string
		orders int
		spread uint64
	}{
		{"vazio", 0, 1}, {"poucos_niveis", 7, 3}, {"exatamente_10", 40, 10},
		{"muitos_niveis", 20000, 50000}, {"mercado_apertado", 20000, 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rng := rand.New(rand.NewSource(7))
			ob := NewOrderBook(1, nil)
			for i := 0; i < tc.orders; i++ {
				side := Side(rng.Intn(2))
				// Compras abaixo de 100000 e vendas acima, com alguns cruzamentos.
				var price uint64
				if side == Buy {
					price = 100000 - uint64(rng.Int63n(int64(tc.spread))) + uint64(rng.Intn(3))
				} else {
					price = 100000 + uint64(rng.Int63n(int64(tc.spread))) - uint64(rng.Intn(3))
				}
				ob.Process(Order{ID: uint64(i + 1), Price: price, Quantity: uint64(rng.Intn(5) + 1), Side: side})
				if i%997 == 0 || i == tc.orders-1 {
					got, want := ob.GetSnapshot(), referenceSnapshot(ob)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("após %d ordens:\n got  %+v\n want %+v", i+1, got, want)
					}
				}
			}
			if tc.orders == 0 {
				if got := ob.GetSnapshot(); len(got.Asks) != 0 || len(got.Bids) != 0 {
					t.Fatalf("livro vazio deveria dar snapshot vazio: %+v", got)
				}
			}
		})
	}
}

// Pior caso do heap: os 10 menores preços formam uma cadeia pai->filho pela
// borda direita (posições 0, 2, 6, 14, ..., 1022). O 10º menor está na última
// posição da janela de 2^10-1 entradas; uma janela menor o perderia. Em livros
// aleatórios isso quase nunca acontece, por isso o heap é montado à mão.
func TestGetSnapshot_AdversarialHeapWorstCase(t *testing.T) {
	const n = 3000
	h := make(MinPriceHeap, n)
	for i := range h {
		h[i] = uint64(1_000_000 + i) // crescente no índice: heap de mínima válido
	}
	idx, want := 0, make([]uint64, 0, 10)
	for rank := uint64(1); rank <= 10; rank++ {
		h[idx] = rank
		want = append(want, rank)
		idx = 2*idx + 2 // filho direito
	}
	if idx != 2*1022+2 { // 10 passos: último índice usado foi 1022
		t.Fatalf("montagem do heap inesperada: idx=%d", idx)
	}
	for i := 1; i < n; i++ {
		if h[(i-1)/2] > h[i] {
			t.Fatalf("heap inválido na posição %d", i)
		}
	}
	if got := bestPrices(h, 10, func(a, b uint64) bool { return a < b }); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
