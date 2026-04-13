package matching

import (
	"fmt"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
)

// EngineHandler é o "Cão" do revólver. Ele escuta o Ring Buffer e aciona o Motor.
type EngineHandler struct {
	orderBook *OrderBook
}

// NewEngineHandler cria a ponte de conexão
func NewEngineHandler(orderBook *OrderBook) *EngineHandler {
	return &EngineHandler{
		orderBook: orderBook,
	}
}

// OnEvent é a função que o Disruptor vai chamar 60 milhões de vezes por segundo.
func (h *EngineHandler) OnEvent(event disruptor.OrderEvent, sequence uint64) {
	// 1. Traduzimos a "Bala" do Disruptor para a linguagem do Motor
	order := Order{
		ID:       event.OrderID,
		Quantity: event.Quantity,
		Price:    event.Price,
		// Como ambos os Sides são uint8 por baixo dos panos, podemos converter direto
		Side: Side(event.Side),
	}

	// 2. Opcional: Imprimir na tela só pra gente ver a mágica acontecendo hoje
	sideStr := "COMPRA"
	if order.Side == Sell {
		sideStr = "VENDA"
	}

	fmt.Printf("[SEQ %d] Recebida ordem de %s: %d BTC a $%d\n", sequence, sideStr, order.Quantity, order.Price)

	// 3. O Disparo! Injeta a ordem no Pac-Man.
	h.orderBook.Process(order)
}
