package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder registra a ordem em que as peças foram desligadas.
type recorder struct {
	mu    sync.Mutex
	steps []string
}

func (r *recorder) add(s string) { r.mu.Lock(); r.steps = append(r.steps, s); r.mu.Unlock() }
func (r *recorder) got() string  { r.mu.Lock(); defer r.mu.Unlock(); return strings.Join(r.steps, ",") }

type comp struct {
	name string
	r    *recorder
	err  error
	ctx  *context.Context
}

func (c *comp) Shutdown(ctx context.Context) error {
	c.r.add(c.name)
	if c.ctx != nil {
		*c.ctx = ctx
	}
	return c.err
}
func (c *comp) Close() error                   { c.r.add(c.name); return c.err }
func (c *comp) CloseCtx(context.Context) error { c.r.add(c.name); return c.err }

type batcherFake struct{ *comp }

func (b batcherFake) Close(ctx context.Context) error { return b.CloseCtx(ctx) }

func newFull(r *recorder) (*Components, map[string]*comp) {
	m := map[string]*comp{}
	for _, n := range []string{"http", "tcp", "ring", "wal", "batcher"} {
		m[n] = &comp{name: n, r: r}
	}
	done := make(chan struct{})
	c := &Components{
		HTTP:        m["http"],
		TCP:         m["tcp"],
		Ring:        m["ring"],
		WAL:         m["wal"],
		Batcher:     batcherFake{m["batcher"]},
		CloseTrades: func() { r.add("trades-close"); close(done) },
		TradesDone:  done,
		Logf:        func(string, ...any) {},
	}
	return c, m
}

func ctx5() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// A ordem importa: quem produz trabalho para primeiro, depois drena-se o anel,
// e só então fecham-se os destinos (WAL, Postgres, SQS).
func TestShutdown_Order(t *testing.T) {
	r := &recorder{}
	c, _ := newFull(r)
	ctx, cancel := ctx5()
	defer cancel()

	if err := c.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	want := "http,tcp,ring,wal,batcher,trades-close"
	if r.got() != want {
		t.Fatalf("ordem de desligamento\n got: %s\nwant: %s", r.got(), want)
	}
}

// Fechar o canal de trades com o motor ainda rodando faria o motor dar panic
// (send em canal fechado). Se o anel não parou, o canal NÃO pode ser fechado.
func TestShutdown_DoesNotCloseTradesIfEngineDidNotStop(t *testing.T) {
	r := &recorder{}
	c, m := newFull(r)
	m["ring"].err = context.DeadlineExceeded
	ctx, cancel := ctx5()
	defer cancel()

	err := c.Shutdown(ctx)
	if err == nil || !strings.Contains(err.Error(), "anel") {
		t.Fatalf("esperava erro do anel, veio %v", err)
	}
	if strings.Contains(r.got(), "trades-close") {
		t.Fatalf("o canal de trades foi fechado com o motor ainda ativo: %s", r.got())
	}
	// Os demais passos ainda rodam: o WAL e o lote do Postgres precisam ser fechados.
	if !strings.Contains(r.got(), "wal") || !strings.Contains(r.got(), "batcher") {
		t.Fatalf("passos seguintes não rodaram: %s", r.got())
	}
}

func TestShutdown_AFailingStepDoesNotSkipTheOthers(t *testing.T) {
	r := &recorder{}
	c, m := newFull(r)
	m["http"].err = errors.New("porta presa")
	m["wal"].err = errors.New("disco")
	ctx, cancel := ctx5()
	defer cancel()

	err := c.Shutdown(ctx)
	if err == nil || !strings.Contains(err.Error(), "servidor HTTP") || !strings.Contains(err.Error(), "WAL") {
		t.Fatalf("esperava os dois erros juntos, veio %v", err)
	}
	if r.got() != "http,tcp,ring,wal,batcher,trades-close" {
		t.Fatalf("um passo com erro pulou os seguintes: %s", r.got())
	}
}

func TestShutdown_PassesTheDeadlineToEveryComponent(t *testing.T) {
	r := &recorder{}
	c, m := newFull(r)
	var seen context.Context
	m["tcp"].ctx = &seen
	ctx, cancel := ctx5()
	defer cancel()

	c.Shutdown(ctx)
	if dl, ok := seen.Deadline(); !ok || time.Until(dl) <= 0 {
		t.Fatal("o contexto com prazo não chegou ao componente")
	}
}

func TestShutdown_TradesDoneTimeoutIsReported(t *testing.T) {
	r := &recorder{}
	c, _ := newFull(r)
	c.TradesDone = make(chan struct{}) // os workers do SQS nunca terminam
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := c.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "SQS") {
		t.Fatalf("esperava timeout do SQS, veio %v", err)
	}
}

func TestShutdown_AllNilIsANoOp(t *testing.T) {
	c := &Components{Logf: func(string, ...any) {}}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
