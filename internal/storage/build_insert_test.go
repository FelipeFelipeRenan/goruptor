package storage

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
)

// buildInsertReference é a implementação original (fmt.Sprintf por linha),
// mantida como oráculo: o SQL e os argumentos têm de ser idênticos.
func buildInsertReference(batch []disruptor.OrderEvent) (string, []interface{}) {
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

func TestBuildInsert_IdenticalToReference(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	sizes := []int{0, 1, 2, 3, 9, 10, 24, 25, 99, 100, 1000, maxBatchSize}
	for _, n := range sizes {
		batch := make([]disruptor.OrderEvent, n)
		for i := range batch {
			batch[i] = disruptor.OrderEvent{
				OrderID: r.Uint64(), Price: r.Uint64() >> uint(r.Intn(64)), Quantity: r.Uint64() >> uint(r.Intn(64)),
				Side: disruptor.Side(r.Intn(2)),
			}
		}
		gotS, gotA := buildInsert(batch)
		wantS, wantA := buildInsertReference(batch)
		if gotS != wantS {
			t.Fatalf("n=%d: SQL difere\n got %.120s\nwant %.120s", n, gotS, wantS)
		}
		if !reflect.DeepEqual(gotA, wantA) {
			t.Fatalf("n=%d: argumentos diferem", n)
		}
	}
}
