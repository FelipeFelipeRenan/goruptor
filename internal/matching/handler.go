package matching

import (
	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
)

// EngineHandler é o "Cão" do revólver. Ele escuta o Ring Buffer e aciona o Motor.
//
// OnEvent roda na única goroutine consumidora, no caminho crítico de TODAS as
// ordens. Por isso não deve fazer I/O (print, log, rede, disco): um único
// fmt.Printf por ordem custava ~1 µs e limitava o motor a uma fração do que o
// order book aguenta. Observabilidade deve sair por contadores atômicos ou por
// um canal não bloqueante consumido fora daqui.
type EngineHandler struct {
	orderBook *OrderBook
}

// NewEngineHandler cria a ponte de conexão
func NewEngineHandler(orderBook *OrderBook) *EngineHandler {
	return &EngineHandler{
		orderBook: orderBook,
	}
}

// OnEvent traduz o evento do Disruptor para a linguagem do Motor e o processa.
func (h *EngineHandler) OnEvent(event disruptor.OrderEvent, sequence uint64) {
	h.orderBook.Process(Order{
		ID:       event.OrderID,
		Quantity: event.Quantity,
		Price:    event.Price,
		// Como ambos os Sides são uint8 por baixo dos panos, podemos converter direto
		Side: Side(event.Side),
	})
}
