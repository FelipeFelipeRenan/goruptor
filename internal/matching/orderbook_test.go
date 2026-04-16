package matching

import (
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
