package ingest

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

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

func (f *fakePublisher) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.published) }

type fakeArchiver struct {
	err error
	mu  sync.Mutex
	got []disruptor.OrderEvent
}

func (a *fakeArchiver) Push(e disruptor.OrderEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.got = append(a.got, e)
	return a.err
}

func (a *fakeArchiver) count() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.got) }

func good(id uint64) disruptor.OrderEvent {
	return disruptor.OrderEvent{OrderID: id, Price: 65000, Quantity: 2, Side: disruptor.Buy}
}

// ---------------------------------------------------------------------------
// ParseSide / Validate
// ---------------------------------------------------------------------------

func TestParseSide(t *testing.T) {
	ok := map[string]disruptor.Side{
		"BUY": disruptor.Buy, "buy": disruptor.Buy, " Buy ": disruptor.Buy,
		"SELL": disruptor.Sell, "sell": disruptor.Sell, "SeLl": disruptor.Sell,
	}
	for in, want := range ok {
		got, err := ParseSide(in)
		if err != nil || got != want {
			t.Errorf("ParseSide(%q) = %v, %v; esperava %v", in, got, err, want)
		}
	}
	// Antes, QUALQUER coisa que não fosse "SELL" virava compra.
	for _, in := range []string{"", "B", "COMPRA", "HOLD", "0", "BUYSELL", "buy;sell"} {
		if _, err := ParseSide(in); !errors.Is(err, ErrInvalidOrder) {
			t.Errorf("ParseSide(%q) deveria ser ErrInvalidOrder, veio %v", in, err)
		}
	}
}

func TestValidate(t *testing.T) {
	mod := func(f func(*disruptor.OrderEvent)) disruptor.OrderEvent {
		e := good(1)
		f(&e)
		return e
	}
	cases := []struct {
		name string
		ev   disruptor.OrderEvent
		ok   bool
	}{
		{"ordem normal", good(1), true},
		{"venda", mod(func(e *disruptor.OrderEvent) { e.Side = disruptor.Sell }), true},
		{"preço mínimo", mod(func(e *disruptor.OrderEvent) { e.Price = 1 }), true},
		{"preço máximo", mod(func(e *disruptor.OrderEvent) { e.Price = MaxPrice }), true},
		{"quantidade mínima", mod(func(e *disruptor.OrderEvent) { e.Quantity = 1 }), true},
		{"quantidade máxima", mod(func(e *disruptor.OrderEvent) { e.Quantity = MaxQuantity }), true},
		{"order_id máximo", mod(func(e *disruptor.OrderEvent) { e.OrderID = ^uint64(0) }), true},

		{"order_id zero", mod(func(e *disruptor.OrderEvent) { e.OrderID = 0 }), false},
		{"preço zero", mod(func(e *disruptor.OrderEvent) { e.Price = 0 }), false},
		{"preço acima do máximo", mod(func(e *disruptor.OrderEvent) { e.Price = MaxPrice + 1 }), false},
		{"quantidade zero", mod(func(e *disruptor.OrderEvent) { e.Quantity = 0 }), false},
		{"quantidade acima do máximo", mod(func(e *disruptor.OrderEvent) { e.Quantity = MaxQuantity + 1 }), false},
		{"lado inválido", mod(func(e *disruptor.OrderEvent) { e.Side = 2 }), false},
		{"lado 255 (byte lixo do TCP)", mod(func(e *disruptor.OrderEvent) { e.Side = 255 }), false},
		{"quantidade que estouraria o volume", mod(func(e *disruptor.OrderEvent) { e.Quantity = 1 << 63 }), false},
	}
	for _, c := range cases {
		err := Validate(c.ev)
		if c.ok && err != nil {
			t.Errorf("%s: esperava válida, veio %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrInvalidOrder) {
			t.Errorf("%s: esperava ErrInvalidOrder, veio %v", c.name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// IDSet
// ---------------------------------------------------------------------------

// 200 goroutines disputam o MESMO id: exatamente uma pode vencer.
func TestIDSet_ExactlyOneWinnerPerID(t *testing.T) {
	s := NewIDSet()
	const contenders = 200
	var winners atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if s.Register(42) {
				winners.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("%d vencedores para o mesmo id; esperava exatamente 1", winners.Load())
	}
}

func TestIDSet_ManyDistinctIDsAcrossShards(t *testing.T) {
	s := NewIDSet()
	const goroutines, per = 16, 5000
	var wg sync.WaitGroup
	var dup atomic.Int64
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 1; i <= per; i++ { // IDs sequenciais: o pior caso para a distribuição
				if !s.Register(uint64(g*per + i)) {
					dup.Add(1)
				}
			}
		}(g)
	}
	wg.Wait()
	if dup.Load() != 0 || s.Len() != goroutines*per {
		t.Fatalf("duplicatas falsas=%d len=%d (esperava %d)", dup.Load(), s.Len(), goroutines*per)
	}

	// Os IDs sequenciais precisam se espalhar pelos shards (senão o lock vira gargalo).
	used := 0
	for i := range s.shards {
		if len(s.shards[i].m) > 0 {
			used++
		}
	}
	if used < idShards*3/4 {
		t.Fatalf("só %d de %d shards usados: distribuição ruim", used, idShards)
	}
}

func TestIDSet_ForgetReleasesID(t *testing.T) {
	s := NewIDSet()
	s.Register(7)
	if s.Register(7) {
		t.Fatal("segundo Register deveria falhar")
	}
	s.Forget(7)
	if !s.Register(7) {
		t.Fatal("depois de Forget o ID deveria poder ser registrado de novo")
	}
}

// ---------------------------------------------------------------------------
// Submit
// ---------------------------------------------------------------------------

func TestSubmit_InvalidNeverTouchesEngineOrIDs(t *testing.T) {
	pub, arc := &fakePublisher{}, &fakeArchiver{}
	in := New(pub, arc)

	bad := good(1)
	bad.Quantity = 0
	if err := in.Submit(bad); !errors.Is(err, ErrInvalidOrder) {
		t.Fatalf("esperava ErrInvalidOrder, veio %v", err)
	}
	if pub.count() != 0 || arc.count() != 0 || in.IDs().Len() != 0 {
		t.Fatal("ordem inválida não pode publicar, arquivar nem consumir o order_id")
	}
	// O mesmo ID, já corrigido, continua disponível.
	if err := in.Submit(good(1)); err != nil {
		t.Fatalf("ordem corrigida deveria passar: %v", err)
	}
}

func TestSubmit_DuplicateIsRejectedAndNotExecutedTwice(t *testing.T) {
	pub, arc := &fakePublisher{}, &fakeArchiver{}
	in := New(pub, arc)

	if err := in.Submit(good(5)); err != nil {
		t.Fatal(err)
	}
	if err := in.Submit(good(5)); !errors.Is(err, ErrDuplicateOrder) {
		t.Fatalf("esperava ErrDuplicateOrder, veio %v", err)
	}
	if pub.count() != 1 || arc.count() != 1 {
		t.Fatalf("a duplicata vazou: publicadas=%d arquivadas=%d", pub.count(), arc.count())
	}
}

// O cenário mais perigoso: o cliente (ou um retry de rede) dispara a MESMA
// ordem várias vezes ao mesmo tempo. Só uma pode chegar ao motor.
func TestSubmit_ConcurrentDuplicatesExecuteOnce(t *testing.T) {
	pub, arc := &fakePublisher{}, &fakeArchiver{}
	in := New(pub, arc)

	var ok, dup atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			switch err := in.Submit(good(99)); {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrDuplicateOrder):
				dup.Add(1)
			default:
				t.Errorf("erro inesperado: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if ok.Load() != 1 || dup.Load() != 99 || pub.count() != 1 {
		t.Fatalf("aceitas=%d duplicadas=%d publicadas=%d (esperava 1/99/1)", ok.Load(), dup.Load(), pub.count())
	}
}

func TestSubmit_PublishFailureFreesTheID(t *testing.T) {
	pub := &fakePublisher{publishErr: disruptor.ErrClosed}
	arc := &fakeArchiver{}
	in := New(pub, arc)

	err := in.Submit(good(3))
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, disruptor.ErrClosed) {
		t.Fatalf("esperava ErrUnavailable embrulhando ErrClosed, veio %v", err)
	}
	if in.IDs().Len() != 0 {
		t.Fatal("nada entrou no anel: o ID deveria ter sido liberado para um novo envio")
	}

	pub.publishErr = nil // "servidor reiniciou"
	if err := in.Submit(good(3)); err != nil {
		t.Fatalf("retry deveria passar: %v", err)
	}
}

// Falha ESPERANDO o disco é ambígua (a ordem pode ter chegado ao WAL). Reenviar
// tem que ser tratado como duplicata, não como uma segunda execução.
func TestSubmit_DurabilityFailureKeepsIDAndSkipsArchive(t *testing.T) {
	pub := &fakePublisher{waitErr: disruptor.ErrJournalFailed}
	arc := &fakeArchiver{}
	in := New(pub, arc)

	err := in.Submit(good(8))
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, disruptor.ErrJournalFailed) {
		t.Fatalf("esperava ErrUnavailable embrulhando ErrJournalFailed, veio %v", err)
	}
	if arc.count() != 0 {
		t.Fatal("ordem não confirmada não pode ser arquivada")
	}
	if err := in.Submit(good(8)); !errors.Is(err, ErrDuplicateOrder) {
		t.Fatalf("reenvio de resultado ambíguo deveria ser duplicata, veio %v", err)
	}
	if pub.count() != 1 {
		t.Fatalf("a ordem foi publicada %d vezes", pub.count())
	}
}

func TestSubmit_ArchiveFailureDoesNotUndoAConfirmedOrder(t *testing.T) {
	pub, arc := &fakePublisher{}, &fakeArchiver{err: errors.New("postgres fora do ar")}
	in := New(pub, arc)
	if err := in.Submit(good(2)); err != nil {
		t.Fatalf("a ordem está durável; falha de arquivo não pode virar erro: %v", err)
	}
}

func TestSubmit_ArchivesOnlyAfterDurable(t *testing.T) {
	pub := &fakePublisher{durable: make(chan struct{})}
	arc := &fakeArchiver{}
	in := New(pub, arc)

	done := make(chan error, 1)
	go func() { done <- in.Submit(good(1)) }()

	time.Sleep(20 * time.Millisecond)
	if arc.count() != 0 {
		t.Fatal("ordem arquivada antes de estar durável")
	}
	select {
	case <-done:
		t.Fatal("Submit retornou antes de o journal confirmar")
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
