package api

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
	"github.com/FelipeFelipeRenan/goruptor/internal/ingest"
)

// ---------------------------------------------------------------------------
// Fakes e helpers
// ---------------------------------------------------------------------------

// fakePublisher deixa o teste decidir quando (e se) a ordem fica durável.
type fakePublisher struct {
	publishErr error
	waitErr    error
	durable    chan struct{} // se não nil, WaitDurable bloqueia até fechar
	entered    chan struct{} // se não nil, recebe um sinal quando WaitDurable começa

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
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.durable != nil {
		<-f.durable
	}
	return f.waitErr
}

type fakeArchiver struct {
	mu sync.Mutex
	n  int
}

func (a *fakeArchiver) Push(disruptor.OrderEvent) error {
	a.mu.Lock()
	a.n++
	a.mu.Unlock()
	return nil
}
func (a *fakeArchiver) count() int { a.mu.Lock(); defer a.mu.Unlock(); return a.n }

func frame(id, price, qty uint64, side byte) []byte {
	b := make([]byte, frameSize)
	binary.BigEndian.PutUint64(b[0:8], id)
	binary.BigEndian.PutUint64(b[8:16], price)
	binary.BigEndian.PutUint64(b[16:24], qty)
	b[24] = side
	return b
}

// startServer sobe um TCPServer real em 127.0.0.1:0 e o encerra ao fim do teste.
func startServer(t *testing.T, pub *fakePublisher, arc *fakeArchiver) (*TCPServer, string, <-chan error) {
	t.Helper()
	s := NewTCPServer(ingest.New(pub, arc))
	if err := s.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- s.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	return s, s.Addr().String(), served
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func readAck(c net.Conn, wait time.Duration) (byte, error) {
	c.SetReadDeadline(time.Now().Add(wait))
	var b [1]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

func expectAck(t *testing.T, c net.Conn, want byte) {
	t.Helper()
	got, err := readAck(c, 2*time.Second)
	if err != nil || got != want {
		t.Fatalf("esperava ACK %d, veio %d (err=%v)", want, got, err)
	}
}

// ---------------------------------------------------------------------------
// Protocolo
// ---------------------------------------------------------------------------

func TestTCP_AckOnlyAfterOrderIsDurable(t *testing.T) {
	pub := &fakePublisher{durable: make(chan struct{})}
	_, addr, _ := startServer(t, pub, &fakeArchiver{})
	c := dial(t, addr)

	c.Write(frame(7, 100, 2, 0))
	if _, err := readAck(c, 50*time.Millisecond); err == nil {
		t.Fatal("ACK enviado antes de a ordem estar durável")
	}

	close(pub.durable)
	expectAck(t, c, AckOK)
	if e := pub.published[0]; e.OrderID != 7 || e.Price != 100 || e.Quantity != 2 {
		t.Fatalf("frame decodificado errado: %+v", e)
	}
}

func TestTCP_RejectsWhenJournalFailsAndClosesConnection(t *testing.T) {
	pub := &fakePublisher{waitErr: disruptor.ErrJournalFailed}
	_, addr, _ := startServer(t, pub, &fakeArchiver{})
	c := dial(t, addr)

	c.Write(frame(1, 1, 1, 0))
	expectAck(t, c, AckRejected)

	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("depois de AckRejected a conexão deveria fechar (EOF), veio %v", err)
	}
}

// Ordem inválida é culpa do cliente: recebe AckInvalid e a conexão SEGUE aberta.
func TestTCP_InvalidFramesGetAckInvalidAndConnectionStaysOpen(t *testing.T) {
	pub := &fakePublisher{}
	_, addr, _ := startServer(t, pub, &fakeArchiver{})
	c := dial(t, addr)

	bad := [][]byte{
		frame(1, 100, 1, 7),   // lado lixo
		frame(2, 0, 1, 0),     // preço zero
		frame(3, 100, 0, 1),   // quantidade zero
		frame(0, 100, 1, 0),   // order_id zero
		frame(4, 100, 1, 255), // outro lado lixo
	}
	for i, f := range bad {
		c.Write(f)
		got, err := readAck(c, 2*time.Second)
		if err != nil || got != AckInvalid {
			t.Fatalf("frame inválido %d: esperava AckInvalid, veio %d (err=%v)", i, got, err)
		}
	}
	if n := len(pub.published); n != 0 {
		t.Fatalf("%d ordens inválidas chegaram ao anel", n)
	}

	// A mesma conexão continua útil.
	c.Write(frame(10, 100, 1, 0))
	expectAck(t, c, AckOK)
}

func TestTCP_DuplicateOrderGetsAckDuplicate(t *testing.T) {
	pub := &fakePublisher{}
	arc := &fakeArchiver{}
	_, addr, _ := startServer(t, pub, arc)

	a, b := dial(t, addr), dial(t, addr) // o dedupe vale entre conexões
	a.Write(frame(5, 100, 1, 0))
	expectAck(t, a, AckOK)
	b.Write(frame(5, 100, 1, 0))
	expectAck(t, b, AckDuplicate)
	b.Write(frame(6, 100, 1, 0)) // e a conexão segue aberta
	expectAck(t, b, AckOK)

	if len(pub.published) != 2 || arc.count() != 2 {
		t.Fatalf("publicadas=%d arquivadas=%d; esperava 2/2", len(pub.published), arc.count())
	}
}

// Frames pipelined e fragmentados em pedaços arbitrários continuam sendo
// remontados e confirmados na ordem, um ACK por ordem.
func TestTCP_PipelinedAndFragmentedFrames(t *testing.T) {
	pub := &fakePublisher{}
	arc := &fakeArchiver{}
	_, addr, _ := startServer(t, pub, arc)
	c := dial(t, addr)

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
		expectAck(t, c, AckOK)
	}
	for i, e := range pub.published {
		if e.OrderID != uint64(i+1) {
			t.Fatalf("ordem %d publicada fora de ordem: id=%d", i, e.OrderID)
		}
	}
	if arc.count() != n {
		t.Fatalf("arquivadas %d de %d", arc.count(), n)
	}
}

// ---------------------------------------------------------------------------
// Shutdown
// ---------------------------------------------------------------------------

// Conexões ociosas (bloqueadas lendo) não podem segurar o desligamento.
func TestTCP_ShutdownWakesIdleConnectionsAndServeReturnsNil(t *testing.T) {
	s, addr, served := startServer(t, &fakePublisher{}, &fakeArchiver{})
	idle := []net.Conn{dial(t, addr), dial(t, addr), dial(t, addr)}

	// Garante que o servidor já registrou as 3 conexões.
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.conns)
		s.mu.Unlock()
		if n == len(idle) || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Shutdown demorou %v com conexões ociosas", d)
	}

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve deveria retornar nil após Shutdown, veio %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve não retornou")
	}

	// As conexões ociosas foram encerradas pelo servidor.
	for i, c := range idle {
		c.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("conexão ociosa %d deveria receber EOF, veio %v", i, err)
		}
	}
	// E a porta não aceita mais ninguém.
	if c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("o servidor ainda aceita conexões depois do Shutdown")
	}
}

// O ponto central: uma ordem já lida e esperando o disco NÃO é abandonada pelo
// Shutdown. Ele espera, o cliente recebe o ACK de verdade, e só então retorna.
func TestTCP_ShutdownWaitsForInFlightOrderAndDeliversItsAck(t *testing.T) {
	pub := &fakePublisher{durable: make(chan struct{}), entered: make(chan struct{}, 1)}
	arc := &fakeArchiver{}
	s, addr, _ := startServer(t, pub, arc)
	c := dial(t, addr)

	c.Write(frame(1, 100, 1, 0))
	<-pub.entered // a ordem está esperando o journal

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone <- s.Shutdown(ctx)
	}()

	select {
	case <-shutdownDone:
		t.Fatal("Shutdown retornou com uma ordem ainda esperando o disco")
	case <-time.After(100 * time.Millisecond):
	}

	close(pub.durable) // o disco confirma
	expectAck(t, c, AckOK)
	if err := <-shutdownDone; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if arc.count() != 1 {
		t.Fatal("a ordem em voo deveria ter sido arquivada")
	}
}

// Se o disco nunca responde, o contexto manda: as conexões são derrubadas e o
// cliente NÃO recebe ACK (portanto sabe que a ordem não foi confirmada).
func TestTCP_ShutdownDeadlineForcesConnectionsClosedWithoutAck(t *testing.T) {
	pub := &fakePublisher{durable: make(chan struct{}), entered: make(chan struct{}, 1)}
	s, addr, _ := startServer(t, pub, &fakeArchiver{})
	c := dial(t, addr)

	c.Write(frame(1, 100, 1, 0))
	<-pub.entered

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("esperava DeadlineExceeded, veio %v", err)
	}
	if got, err := readAck(c, time.Second); err == nil {
		t.Fatalf("o cliente recebeu ACK %d de uma ordem que nunca ficou durável", got)
	}
	close(pub.durable) // libera a goroutine presa para o teste terminar limpo
}

// ---------------------------------------------------------------------------
// HTTP: tradução de erros
// ---------------------------------------------------------------------------

func TestHTTPErrorMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
	}{
		{ingest.ErrInvalidOrder, 400},
		{fmt.Errorf("%w: price deve estar entre 1 e 10", ingest.ErrInvalidOrder), 400},
		{fmt.Errorf("%w: 42", ingest.ErrDuplicateOrder), 409},
		{fmt.Errorf("%w: %w", ingest.ErrUnavailable, disruptor.ErrClosed), 503},
		{fmt.Errorf("%w: %w", ingest.ErrUnavailable, disruptor.ErrJournalFailed), 503},
		{errors.New("erro qualquer"), 503},
	}
	for _, c := range cases {
		status, msg := httpError(c.err)
		if status != c.status || msg == "" {
			t.Errorf("httpError(%v) = %d %q; esperava status %d", c.err, status, msg, c.status)
		}
	}
	// Erros do servidor não vazam detalhes internos.
	if _, msg := httpError(fmt.Errorf("%w: %w", ingest.ErrUnavailable, errors.New("disco /dev/sda1 cheio"))); msg != "ordem não confirmada, tente novamente" {
		t.Errorf("mensagem 503 vazou detalhe interno: %q", msg)
	}
}
