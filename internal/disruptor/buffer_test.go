package disruptor

import (
	"runtime"
	"sync/atomic"
	"testing"
)

func BenchmarkRingBuffer_Publish(b *testing.B) {
	rb := NewRingBuffer(1024)

	dummyEvent := OrderEvent{
		OrderID:   1,
		AccountID: 999,
		Price:     6500000,
		Quantity:  2,
		AssetID:   1, // BTC/USD
		Side:      Buy,
	}

	var isDone uint32
	go func() {
		for atomic.LoadUint32(&isDone) == 0 {
			pc := atomic.LoadUint64(&rb.producerCursor)
			atomic.StoreUint64(&rb.consumerCursor, pc)

			runtime.Gosched()
		}
	}()

	b.ResetTimer()

	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		rb.Publish(dummyEvent)
	}

	b.StopTimer()
	atomic.StoreUint32(&isDone, 1)
}
