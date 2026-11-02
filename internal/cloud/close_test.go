package cloud

import "testing"

// Close não perde o que já estava na fila: os workers ainda recebem tudo e só
// depois o canal termina.
func TestAWSPublisher_CloseKeepsQueuedTrades(t *testing.T) {
	p := &AWSPublisher{TradeCh: make(chan TradeEvent, 8)}
	for i := uint64(1); i <= 5; i++ {
		p.TradeCh <- TradeEvent{Price: i, Quantity: 1}
	}
	p.Close()

	var got []uint64
	for tr := range p.TradeCh { // termina porque o canal foi fechado
		got = append(got, tr.Price)
	}
	if len(got) != 5 || got[0] != 1 || got[4] != 5 {
		t.Fatalf("recebidos %v; esperava os 5 trades em ordem", got)
	}
}
