package main

import (
	"io"
	"net"
	"testing"
	"time"
)

// servidor falso: lê frames de 25 bytes e responde um ACK por frame, após delay.
func fakeServer(t *testing.T, delay time.Duration, ack byte) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, frameSize)
				for {
					if _, err := io.ReadFull(c, buf); err != nil {
						return
					}
					time.Sleep(delay)
					if _, err := c.Write([]byte{ack}); err != nil {
						return
					}
				}
			}()
		}
	}()
	return l.Addr().String()
}

func TestRunConn_MeasuresLatencyAndCountsACKs(t *testing.T) {
	for _, window := range []int{0, 1, 8} {
		addr := fakeServer(t, 2*time.Millisecond, ackOK)
		res := runConn(addr, 1, 50, 10*time.Second, window)
		if res.err != nil || res.confirmed != 50 || res.rejected != 0 || len(res.lat) != 50 {
			t.Fatalf("window=%d: %+v", window, res)
		}
		st := summarize(res.lat)
		if st.min < 2 { // o servidor demora 2ms por ACK
			t.Fatalf("window=%d: latência mínima %.2fms menor que o atraso do servidor (2ms)", window, st.min)
		}
		if !(st.min <= st.p50 && st.p50 <= st.p95 && st.p95 <= st.p99 && st.p99 <= st.max && st.min <= st.mean && st.mean <= st.max) {
			t.Fatalf("window=%d: estatísticas inconsistentes: %+v", window, st)
		}
	}
}

// Com janela 1 a ordem seguinte só sai depois do ACK: a latência reflete o servidor
// (~2ms), não uma fila. Sem janela, a rajada acumula fila e a latência cresce.
func TestRunConn_WindowBoundsLatency(t *testing.T) {
	addr := fakeServer(t, 2*time.Millisecond, ackOK)
	w1 := summarize(runConn(addr, 1, 40, 10*time.Second, 1).lat)
	burst := summarize(runConn(addr, 1000, 40, 10*time.Second, 0).lat)
	if w1.max > 20 {
		t.Fatalf("com window=1 a latência máxima deveria ficar perto de 2ms, veio %.2fms", w1.max)
	}
	if burst.max < 5*w1.p50 {
		t.Fatalf("a rajada deveria mostrar fila: max=%.2fms vs window1 p50=%.2fms", burst.max, w1.p50)
	}
}

func TestRunConn_CountsRejections(t *testing.T) {
	addr := fakeServer(t, 0, 3) // sempre "duplicada"
	res := runConn(addr, 1, 20, 10*time.Second, 4)
	if res.err != nil || res.confirmed != 0 || res.rejected != 20 {
		t.Fatalf("%+v", res)
	}
}

func TestRunConn_ServerClosingDoesNotHangWithWindow(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		c, _ := l.Accept()
		buf := make([]byte, frameSize)
		io.ReadFull(c, buf) // lê um frame e fecha sem responder
		c.Close()
	}()
	done := make(chan result, 1)
	go func() { done <- runConn(l.Addr().String(), 1, 100, 5*time.Second, 2) }()
	select {
	case res := <-done:
		if res.err == nil {
			t.Fatal("esperava erro")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("travou com o servidor fechando a conexão")
	}
}

func TestSummarize_Percentiles(t *testing.T) {
	var lat []time.Duration
	for i := 1; i <= 100; i++ {
		lat = append(lat, time.Duration(i)*time.Millisecond)
	}
	st := summarize(lat)
	if st.min != 1 || st.max != 100 || st.p50 != 50 || st.p95 != 95 || st.p99 != 99 || st.mean != 50.5 {
		t.Fatalf("%+v", st)
	}
	if (summarize(nil) != latStats{}) {
		t.Fatal("vazio deveria zerar")
	}
}
