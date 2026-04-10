package matching

import (
	"container/heap"
	"container/list"
)

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

	askPrices *MinPriceHeap
	bidPrices *MaxPriceHeap

	// Precisamos saber rapidamente qual é o topo do livro (Best Bid e Best Ask)
	bestBid uint64
	bestAsk uint64
}

// NewOrderBook inicializa a memória do motor
func NewOrderBook(AssetID uint32) *OrderBook {

	minH := &MinPriceHeap{}
	maxH := &MaxPriceHeap{}

	heap.Init(minH)
	heap.Init(maxH)
	return &OrderBook{
		AssetID:   AssetID,
		bidsMap:   make(map[uint64]*PriceLevel),
		asksMap:   make(map[uint64]*PriceLevel),
		askPrices: minH,
		bidPrices: maxH,
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

		if order.Side == Buy {
			heap.Push(ob.bidPrices, order.Price)
		} else {
			heap.Push(ob.askPrices, order.Price)
		}
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

// Process é a porta de entrada do Consumidor.
// Ele decide se a ordem vai atacar o livro ou se vai descansar na fila.
func (ob *OrderBook) Process(order Order) {
	if order.Side == Buy {
		ob.matchBuy(order)
	} else {
		ob.matchSell(order) // A lógica de venda é o espelho da compra
	}
}

// matchBuy é o predador de ordens de Venda (Asks).
func (ob *OrderBook) matchBuy(order Order) {
	// LOOP DE ATAQUE:
	// 1. Eu ainda tenho quantidade para comprar?
	// 2. Existe alguém vendendo? (bestAsk > 0)
	// 3. O preço do vendedor é menor ou igual ao máximo que estou disposto a pagar?
	for order.Quantity > 0 && ob.bestAsk > 0 && order.Price >= ob.bestAsk {
		// Pegamos a prateleira com as ofertas mais baratas em O(1)
		bestLevel := ob.asksMap[ob.bestAsk]

		// Percorremos a fila de pessoas vendendo neste preço (Prioridade de Tempo - FIFO)
		element := bestLevel.Orders.Front()

		for element != nil && order.Quantity > 0 {
			// Extraímos a ordem que estava descansando
			restingOrder := element.Value.(Order)

			// Calcula quanto podemos cruzar (o menor entre o que eu quero e o que ele tem)
			var tradeQty uint64
			if order.Quantity < restingOrder.Quantity {
				tradeQty = order.Quantity
			} else {
				tradeQty = restingOrder.Quantity
			}

			// TODO: SQS em breve aqui

			// Deduzimos as quantidades
			order.Quantity -= tradeQty
			bestLevel.TotalVolume -= tradeQty

			if restingOrder.Quantity == 0 {
				// O vendedor vendeu tudo. Removemos ele da fila e avançamos para o próximo.
				next := element.Next()
				bestLevel.Orders.Remove(next)
				element = next
			} else {
				// O vendedor ainda tem saldo, mas a MINHA ordem zerou.
				// Atualizamos a ordem dele na fila e paramos o ataque.
				element.Value = restingOrder
				break
			}

		}
		// A prateleira de preço esvaziou? Removemos ela e subimos o preço.
		if bestLevel.Orders.Len() == 0 {
			delete(ob.asksMap, ob.bestAsk)
			heap.Pop(ob.askPrices)

			if ob.askPrices.Len() > 0 {
				ob.bestAsk = (*ob.askPrices)[0]
			} else {
				ob.bestAsk = 0
			}
		}

	}
	// O ataque acabou. Se ainda sobrou quantidade na minha ordem original,
	// significa que acabei com os vendedores baratos. Minha ordem agora vai descansar.
	if order.Quantity > 0 {
		ob.addOrder(order)
	}
}

// matchSell é o predador de ordens de Compra (Bids).
// Ele varre o livro pegando o dinheiro de quem está pagando mais caro.
func (ob *OrderBook) matchSell(order Order) {
	// LOOP DE ATAQUE:
	// 1. Tenho quantidade para vender?
	// 2. Tem alguém querendo comprar? (bestBid > 0)
	// 3. O comprador está pagando MAIOR ou IGUAL ao mínimo que eu aceito receber?
	for order.Quantity > 0 && ob.bestBid > 0 && order.Price <= ob.bestBid {

		// Pegamos a prateleira com os compradores mais agressivos (O(1))
		bestLevel := ob.bidsMap[ob.bestBid]

		// Percorremos a fila (Prioridade de Tempo)
		element := bestLevel.Orders.Front()
		for element != nil && order.Quantity > 0 {
			restingOrder := element.Value.(Order)

			var tradeQty uint64
			if order.Quantity < restingOrder.Quantity {
				tradeQty = order.Quantity
			} else {
				tradeQty = restingOrder.Quantity
			}

			// TODO: criar evento em breve
			order.Quantity -= tradeQty
			restingOrder.Quantity -= tradeQty
			bestLevel.TotalVolume -= tradeQty

			if restingOrder.Quantity == 0 {
				next := element.Next()
				bestLevel.Orders.Remove(element)
				element = next
			} else {
				element.Value = restingOrder
				break
			}
		}

		// A prateleira de compradores esvaziou?
		if bestLevel.Orders.Len() == 0 {
			delete(ob.bidsMap, ob.bestBid)

			// A MÁGICA DO HEAP: Pop tira o maior preço. O próximo maior sobe pro topo.
			heap.Pop(ob.bidPrices)

			if ob.bidPrices.Len() > 0 {
				ob.bestBid = (*ob.bidPrices)[0]
			} else {
				ob.bestBid = 0 // Faltaram compradores no mercado
			}
		}
	}

	// O ataque acabou. Se eu não consegui vender tudo, a sobra da minha ordem
	// vai para o livro de vendas descansar até alguém vir comprar.
	if order.Quantity > 0 {
		ob.addOrder(order)
	}
}
