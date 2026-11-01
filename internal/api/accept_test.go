package api

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
)

// fakePublisher deixa o teste decidir quando (e se) a ordem fica durável.
type fakePublisher struct {
	publishErr error
	waitErr    error
	durable    chan struct{} // se não nil, WaitDurable bloqueia até fechar

	mu        sync.Mutex
	published []disruptor.OrderEvent
}

func (f *fakePublisher) Publish(e disruptor.OrderEvent) (uint64, error) {
	if f.publishErr != nil {
		return 0, f.publishErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, e)
	return uint64(len(f.published) - 1), nil
}

func (f *fakePublisher) WaitDurable(uint64) error {
	if f.durable != nil {
		<-f.durable
	}
	return f.waitErr
}

type fakeArchiver struct {
	mu     sync.Mutex
	pushed []disruptor.OrderEvent
}

func (a *fakeArchiver) Push(e disruptor.OrderEvent) {
	a.mu.Lock()
	a.pushed = append(a.pushed, e)
	a.mu.Unlock()
}

func (a *fakeArchiver) count() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.pushed) }

func TestAcceptOrder_ArchivesOnlyAfterDurable(t *testing.T) {
	pub := &fakePublisher{durable: make(chan struct{})}
	arc := &fakeArchiver{}

	done := make(chan error, 1)
	go func() { done <- acceptOrder(pub, arc, disruptor.OrderEvent{OrderID: 1}) }()

	time.Sleep(20 * time.Millisecond)
	if arc.count() != 0 {
		t.Fatal("ordem foi arquivada antes de estar durável")
	}
	select {
	case <-done:
		t.Fatal("acceptOrder retornou antes do journal confirmar")
	default:
	}

	close(pub.durable)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if arc.count() != 1 {
		t.Fatal("ordem durável deveria ter sido arquivada")
	}
}

func TestAcceptOrder_ErrorsNeverArchive(t *testing.T) {
	cases := map[string]*fakePublisher{
		"anel fechado":          {publishErr: disruptor.ErrClosed},
		"journal falhou":        {publishErr: disruptor.ErrJournalFailed},
		"falha esperando disco": {waitErr: disruptor.ErrJournalFailed},
	}
	for name, pub := range cases {
		t.Run(name, func(t *testing.T) {
			arc := &fakeArchiver{}
			if err := acceptOrder(pub, arc, disruptor.OrderEvent{OrderID: 1}); err == nil {
				t.Fatal("deveria devolver erro")
			}
			if arc.count() != 0 {
				t.Fatal("ordem não confirmada não pode ir para o Postgres")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Protocolo TCP: o byte de ACK só sai depois do journal
// ---------------------------------------------------------------------------

func frame(id, price, qty uint64, side byte) []byte {
	b := make([]byte, 25)
	binary.BigEndian.PutUint64(b[0:8], id)
	binary.BigEndian.PutUint64(b[8:16], price)
	binary.BigEndian.PutUint64(b[16:24], qty)
	b[24] = side
	return b
}

func serve(t *testing.T, s *TCPServer) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	go s.handleConnection(server)
	t.Cleanup(func() { client.Close() })
	return client
}

func readAck(t *testing.T, c net.Conn, wait time.Duration) (byte, bool) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(wait))
	var b [1]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		return 0, false
	}
	return b[0], true
}

func TestTCP_AckOnlyAfterOrderIsDurable(t *testing.T) {
	pub := &fakePublisher{durable: make(chan struct{})}
	arc := &fakeArchiver{}
	c := serve(t, &TCPServer{ringBuffer: pub, batcher: arc})

	if _, err := c.Write(frame(7, 100, 2, 0)); err != nil {
		t.Fatal(err)
	}

	// Enquanto o disco não confirmou, NÃO pode haver ACK.
	if _, got := readAck(t, c, 50*time.Millisecond); got {
		t.Fatal("ACK enviado antes de a ordem estar durável")
	}

	close(pub.durable)
	ack, got := readAck(t, c, time.Second)
	if !got || ack != AckOK {
		t.Fatalf("esperava AckOK depois do journal, veio %v (recebido=%v)", ack, got)
	}
	if pub.published[0].OrderID != 7 || pub.published[0].Price != 100 || pub.published[0].Quantity != 2 {
		t.Fatalf("frame decodificado errado: %+v", pub.published[0])
	}
}

func TestTCP_RejectsWhenJournalFailsAndClosesConnection(t *testing.T) {
	pub := &fakePublisher{waitErr: disruptor.ErrJournalFailed}
	c := serve(t, &TCPServer{ringBuffer: pub, batcher: &fakeArchiver{}})

	c.Write(frame(1, 1, 1, 0))
	ack, got := readAck(t, c, time.Second)
	if !got || ack != AckRejected {
		t.Fatalf("esperava AckRejected, veio %v (recebido=%v)", ack, got)
	}
	// Depois de uma recusa a conexão é encerrada: o cliente não deve continuar enviando.
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("esperava conexão fechada (EOF), veio %v", err)
	}
}

// Frames pipelined e fragmentados em pedaços arbitrários continuam sendo
// remontados e confirmados na ordem, um ACK por ordem.
func TestTCP_PipelinedAndFragmentedFrames(t *testing.T) {
	pub := &fakePublisher{}
	arc := &fakeArchiver{}
	c := serve(t, &TCPServer{ringBuffer: pub, batcher: arc})

	const n = 50
	var stream []byte
	for i := 1; i <= n; i++ {
		stream = append(stream, frame(uint64(i), 100, 1, byte(i%2))...)
	}

	go func() {
		for i := 0; i < len(stream); { // pedaços de 1 a 7 bytes
			step := 1 + i%7
			if i+step > len(stream) {
				step = len(stream) - i
			}
			c.Write(stream[i : i+step])
			i += step
		}
	}()

	for i := 0; i < n; i++ {
		if ack, got := readAck(t, c, 2*time.Second); !got || ack != AckOK {
			t.Fatalf("ACK %d: %v (recebido=%v)", i, ack, got)
		}
	}
	pub.mu.Lock()
	defer pub.mu.Unlock()
	for i, e := range pub.published {
		if e.OrderID != uint64(i+1) {
			t.Fatalf("ordem %d publicada fora de ordem: id=%d", i, e.OrderID)
		}
	}
	if arc.count() != n {
		t.Fatalf("arquivadas %d de %d", arc.count(), n)
	}
}
