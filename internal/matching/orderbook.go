package matching

import "container/list"

// Side define a direção da ordem
type Side uint8

const (
	Buy  Side = 0
	Sell Side = 1
)

// Order representa o estado da ordem dentro do Motor
type Order struct {
	ID       uint64
	Quantity uint64
	Price    uint64
	Side     Side
}

// PriceLevel é uma 'prateleira' de preço.
// Guarda todas as ordens de pessoas que querem exatamente este mesmo preço.
type PriceLevel struct {
	Price       uint64
	TotalVolume uint64     // Soma rápida de todas as quantidades aqui
	Orders      *list.List // Fila (FIFO) usando linked list nativa do Go
}

// OrderBook é a arena onde Bids e Asks se encontram
type OrderBook struct {
	AssetID uint32

	// Mapas para achar o nível de preço em O(1)
	bidsMap map[uint64]*PriceLevel
	asksMap map[uint64]*PriceLevel

	// Precisamos saber rapidamente qual é o topo do livro (Best Bid e Best Ask)
	bestBid uint64
	bestAsk uint64
}

// NewOrderBook inicializa a memória do motor
func NewOrderBook(AssetID uint32) *OrderBook {
	return &OrderBook{
		AssetID: AssetID,
		bidsMap: make(map[uint64]*PriceLevel),
		asksMap: make(map[uint64]*PriceLevel),
	}
}

// addOrder coloca uma ordem passiva na fila de espera do livro.
// Esta operação é O(1) - Velocidade instantânea, independente do tamanho do livro.
func (ob *OrderBook) addOrder(order Order) {
	var bookMap map[uint64]*PriceLevel

	// 1. Descobrimos em qual lado do mapa vamos mexer
	if order.Side == Buy {
		bookMap = ob.bidsMap
	} else {
		bookMap = ob.asksMap
	}

	// 2. Procuramos a prateleira (PriceLevel) em O(1)
	level, exists := bookMap[order.Price]

	// 3. Se não existe ninguém querendo esse preço ainda, criamos a prateleira
	if !exists {
		level = &PriceLevel{
			Price:       order.Price,
			TotalVolume: 0,
			Orders:      list.New(), // Cria uma fila (Lista Duplamente Encadeada) vazia
		}
		bookMap[order.Price] = level
	}

	// 4. Adicionamos a ordem no FINAL da fila (PushBack) respeitando a Prioridade de Tempo
	// E somamos a quantidade dessa ordem ao volume total daquele preço
	level.Orders.PushBack(order)
	level.TotalVolume += order.Quantity

	// 5. Atualizamos os ponteiros de topo do livro (Best Bid / Best Ask)
	ob.updateBestPrices(order.Side, order.Price)
}

// updateBestPrices garante que o motor sempre saiba qual é o preço mais agressivo
func (ob *OrderBook) updateBestPrices(side Side, price uint64) {
	if side == Buy {

		// O melhor comprador é sempre quem paga MAIS CARO
		if price > ob.bestBid {
			ob.bestBid = price
		}
	} else {

		// O melhor vendedor é sempre quem cobra MAIS BARATO
		// Se bestAsk for 0, significa que o livro de vendas estava vazio
		if price < ob.bestAsk || ob.bestAsk == 0 {
			ob.bestAsk = price
		}
	}
}
