package storage

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
)

type Batcher struct {
	db        *sql.DB
	orderCh   chan disruptor.OrderEvent
	batch     []disruptor.OrderEvent
	batchSize int
}

func NewBatcher(connStr string, batchSize int) (*Batcher, error) {
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, err
	}

	b := &Batcher{
		db:        db,
		orderCh:   make(chan disruptor.OrderEvent, 50000),
		batch:     make([]disruptor.OrderEvent, 0, batchSize),
		batchSize: batchSize,
	}

	go b.startWorker()
	return b, nil
}

func (b *Batcher) Push(event disruptor.OrderEvent) {
	b.orderCh <- event
}

func (b *Batcher) startWorker() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case event := <-b.orderCh:
			b.batch = append(b.batch, event)
			if len(b.batch) >= b.batchSize {
				b.flush()
			}
		case <-ticker.C:
			if len(b.batch) > 0 {
				b.flush()
			}
		}
	}
}

func (b *Batcher) flush() {
	if len(b.batch) == 0 {
		return
	}

	valueStrings := make([]string, 0, len(b.batch))
	valueArgs := make([]interface{}, 0, len(b.batch)*4)

	i := 1
	for _, order := range b.batch {
		valueStrings = append(valueStrings, fmt.Sprintf("($%d, $%d, $%d, $%d)", i, i+1, i+2, i+3))

		sideStr := "BUY"
		if order.Side == disruptor.Sell {
			sideStr = "SELL"
		}

		valueArgs = append(valueArgs, order.OrderID, order.Price, order.Quantity, sideStr)
		i += 4
	}

	stmt := fmt.Sprintf("INSERT INTO trade_history (order_id, price, quantity, side) VALUES %s ON CONFLICT DO NOTHING",
		strings.Join(valueStrings, ","))

	_, err := b.db.Exec(stmt, valueArgs...)
	if err != nil {
		log.Printf("❌ Erro no Bulk Insert do RDS: %v", err)
	} else {
		log.Printf("📦 Lote de %d ordens arquivado no PostgreSQL com sucesso!", len(b.batch))
	}

	b.batch = make([]disruptor.OrderEvent, 0, b.batchSize)
}
