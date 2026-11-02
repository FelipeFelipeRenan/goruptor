package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
	_ "github.com/lib/pq"
)

// ErrBatcherClosed é devolvido por Push depois de Close.
var ErrBatcherClosed = errors.New("batcher: fechado")

// maxBatchSize respeita o limite do Postgres de 65535 parâmetros por comando:
// cada ordem usa 4, então 16000 ordens = 64000 parâmetros.
const maxBatchSize = 16000

const defaultFlushInterval = 5 * time.Second

// BatchSink grava um lote de ordens. O de produção faz um INSERT em massa no Postgres.
// O slice é reutilizado pelo Batcher depois que o sink retorna: copie se precisar guardá-lo.
type BatchSink func(ctx context.Context, batch []disruptor.OrderEvent) error

// Batcher junta ordens em lotes e as grava de forma assíncrona (por tamanho ou
// por tempo, o que vier primeiro). Close drena o que sobrou e faz o flush final.
type Batcher struct {
	DB *sql.DB // nil quando criado com NewBatcherWithSink

	orderCh   chan disruptor.OrderEvent
	batchSize int
	interval  time.Duration
	sink      BatchSink

	flushCtx    context.Context
	cancelFlush context.CancelFunc

	closeMu sync.RWMutex // protege closed e o fechamento do canal
	closed  bool
	done    chan struct{} // fechado quando o worker termina
}

// NewBatcher conecta no Postgres, garante a tabela e inicia o worker.
func NewBatcher(connStr string, batchSize int) (*Batcher, error) {
	if batchSize <= 0 || batchSize > maxBatchSize {
		return nil, fmt.Errorf("batcher: batchSize deve estar entre 1 e %d, veio %d", maxBatchSize, batchSize)
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, err
	}

	_, err = db.Exec(`
    CREATE TABLE IF NOT EXISTS trade_history (
        order_id BIGINT PRIMARY KEY,
        price BIGINT NOT NULL,
        quantity BIGINT NOT NULL,
        side VARCHAR(10) NOT NULL,
        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    );`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("criando a tabela trade_history: %w", err)
	}

	b := NewBatcherWithSink(postgresSink(db), batchSize, defaultFlushInterval)
	b.DB = db
	return b, nil
}

// NewBatcherWithSink cria um Batcher que grava através de sink. Útil em testes
// e para trocar o destino (Postgres) sem mexer na lógica de lotes.
func NewBatcherWithSink(sink BatchSink, batchSize int, interval time.Duration) *Batcher {
	if interval <= 0 {
		interval = defaultFlushInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &Batcher{
		orderCh:     make(chan disruptor.OrderEvent, 50000),
		batchSize:   batchSize,
		interval:    interval,
		sink:        sink,
		flushCtx:    ctx,
		cancelFlush: cancel,
		done:        make(chan struct{}),
	}
	go b.startWorker()
	return b
}

// Push enfileira uma ordem para arquivamento. Bloqueia se a fila (50 mil) estiver
// cheia; retorna ErrBatcherClosed depois de Close.
func (b *Batcher) Push(event disruptor.OrderEvent) error {
	b.closeMu.RLock()
	defer b.closeMu.RUnlock()
	if b.closed {
		return ErrBatcherClosed
	}
	b.orderCh <- event
	return nil
}

// Close para de aceitar ordens, grava tudo que já foi enfileirado (flush final)
// e fecha o banco. Se ctx expirar antes, aborta a gravação em andamento e
// devolve ctx.Err(). É idempotente.
func (b *Batcher) Close(ctx context.Context) error {
	b.closeMu.Lock()
	if !b.closed {
		b.closed = true
		close(b.orderCh)
	}
	b.closeMu.Unlock()

	var err error
	select {
	case <-b.done:
	case <-ctx.Done():
		b.cancelFlush() // aborta o INSERT em andamento
		select {
		case <-b.done:
		case <-time.After(2 * time.Second):
		}
		err = ctx.Err()
	}

	b.cancelFlush()
	if b.DB != nil {
		if cerr := b.DB.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

func (b *Batcher) startWorker() {
	defer close(b.done)

	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()

	batch := make([]disruptor.OrderEvent, 0, b.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		b.flush(batch)
		batch = batch[:0]
	}

	for {
		select {
		case event, ok := <-b.orderCh:
			if !ok { // canal fechado e já esvaziado: flush final
				flush()
				return
			}
			batch = append(batch, event)
			if len(batch) >= b.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (b *Batcher) flush(batch []disruptor.OrderEvent) {
	if err := b.sink(b.flushCtx, batch); err != nil {
		log.Printf("❌ Erro no Bulk Insert (%d ordens perdidas do arquivo): %v", len(batch), err)
		return
	}
	log.Printf("📦 Lote de %d ordens arquivado com sucesso!", len(batch))
}

// postgresSink devolve o sink que faz o INSERT em massa na tabela trade_history.
func postgresSink(db *sql.DB) BatchSink {
	return func(ctx context.Context, batch []disruptor.OrderEvent) error {
		stmt, args := buildInsert(batch)
		_, err := db.ExecContext(ctx, stmt, args...)
		return err
	}
}

// buildInsert monta o INSERT em massa (4 parâmetros por ordem).
func buildInsert(batch []disruptor.OrderEvent) (string, []interface{}) {
	valueStrings := make([]string, 0, len(batch))
	valueArgs := make([]interface{}, 0, len(batch)*4)

	i := 1
	for _, order := range batch {
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
	return stmt, valueArgs
}
