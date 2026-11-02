//go:build unix

package disruptor

import (
	"context"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func cpuTime() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

type countHandler struct{ n atomic.Uint64 }

func (h *countHandler) OnEvent(OrderEvent, uint64) { h.n.Add(1) }

// Sem nenhuma ordem, motor + journaler não podem queimar um núcleo inteiro
// (antes da espera adaptativa: ~100% de um núcleo parado).
func TestIdle_DoesNotBurnCPU(t *testing.T) {
	rb := NewJournaledRingBuffer(1<<12, nopJournal{}, JournalOptions{})
	go rb.StartJournaler()
	go rb.StartConsumer(&nopHandler{})
	defer rb.Shutdown(context.Background())

	time.Sleep(300 * time.Millisecond) // deixa a espera "esfriar"
	c0, w0 := cpuTime(), time.Now()
	time.Sleep(2 * time.Second)
	cpu, wall := cpuTime()-c0, time.Since(w0)

	pct := float64(cpu) / float64(wall) * 100
	t.Logf("ocioso: %.1f%% de um núcleo", pct)
	if pct > 50 {
		t.Fatalf("ocioso consumindo %.0f%% de um núcleo (esperado bem abaixo de 50%%)", pct)
	}
}

// Depois de um período parado (dormindo), o primeiro evento ainda é processado
// rápido: a espera adaptativa não pode deixar o motor "dormindo" por muito tempo.
func TestIdle_WakesQuicklyAfterQuietPeriod(t *testing.T) {
	rb := NewJournaledRingBuffer(1<<12, nopJournal{}, JournalOptions{})
	h := &countHandler{}
	go rb.StartJournaler()
	go rb.StartConsumer(h)
	defer rb.Shutdown(context.Background())

	time.Sleep(300 * time.Millisecond)
	var worst time.Duration
	for i := 0; i < 20; i++ {
		start := time.Now()
		seq, err := rb.Publish(OrderEvent{OrderID: uint64(i + 1), Price: 1, Quantity: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := rb.WaitDurable(seq); err != nil {
			t.Fatal(err)
		}
		for h.n.Load() < uint64(i+1) {
			if time.Since(start) > time.Second {
				t.Fatalf("evento %d não processado em 1s", i)
			}
			time.Sleep(10 * time.Microsecond)
		}
		if d := time.Since(start); d > worst {
			worst = d
		}
		time.Sleep(20 * time.Millisecond) // volta a ficar ocioso
	}
	t.Logf("pior latência publish->motor após ociosidade: %v", worst)
	if worst > 50*time.Millisecond {
		t.Fatalf("latência pós-ociosidade alta demais: %v", worst)
	}
}
