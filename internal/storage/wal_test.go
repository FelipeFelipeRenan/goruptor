package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
)

// O reparo de cauda loga um aviso por abertura; nos testes de "crash em cada
// byte" isso seriam milhares de linhas.
func quietLogs(t *testing.T) {
	t.Helper()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
}

func evs(from, n int) []disruptor.OrderEvent {
	out := make([]disruptor.OrderEvent, n)
	for i := range out {
		id := uint64(from + i)
		out[i] = disruptor.OrderEvent{
			OrderID:  id,
			Price:    1000 + id%7,
			Quantity: 1 + id%3,
			Side:     disruptor.Side(id % 2),
		}
	}
	return out
}

func mustWAL(t *testing.T, path string) *WAL {
	t.Helper()
	w, err := NewWAL(path)
	if err != nil {
		t.Fatalf("NewWAL: %v", err)
	}
	return w
}

func equalEvents(a, b []disruptor.OrderEvent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// within falha o teste se f não terminar a tempo (detecta deadlocks).
func within(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("travou: não terminou em %v", d)
	}
}

func TestWAL_RoundTripPreservesOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.jsonl")
	w := mustWAL(t, path)

	want := evs(1, 300)
	for i := 0; i < len(want); i += 100 { // três lotes
		if err := w.Append(want[i : i+100]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}

	got, err := w.ReadAll()
	if err != nil || !equalEvents(got, want) {
		t.Fatalf("ReadAll divergente (err=%v, len=%d)", err, len(got))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Reabrir o arquivo (reinício do processo) devolve o mesmo conteúdo.
	w2 := mustWAL(t, path)
	defer w2.Close()
	got, _ = w2.ReadAll()
	if !equalEvents(got, want) {
		t.Fatal("conteúdo mudou após reabrir")
	}
}

// O contrato do Journal diz que Append sobrevive a crash do PROCESSO: depois
// dele os bytes já estão no SO, visíveis para outro leitor, sem precisar de Sync.
func TestWAL_AppendReachesOSWithoutSync(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wal.jsonl")
	w := mustWAL(t, path)
	defer w.Close()

	if err := w.Append(evs(1, 10)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path) // leitor independente, sem passar pelo WAL
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(raw, []byte("\n")); n != 10 {
		t.Fatalf("esperava 10 linhas no arquivo logo após Append, achei %d", n)
	}
}

// "Crash em cada byte": simula a queda do processo em QUALQUER ponto da
// escrita. Para cada tamanho k possível do arquivo:
//   - a recuperação tem que devolver exatamente os eventos completos dentro de k bytes
//     (um prefixo íntegro do que foi escrito, nunca lixo, nunca evento a mais);
//   - novos Append depois da recuperação têm que continuar legíveis.
//
// Antes, uma linha truncada deixava os appends seguintes colados no lixo e o
// arquivo ficava ilegível dali em diante.
func TestWAL_CrashAtEveryByte(t *testing.T) {
	quietLogs(t)
	dir := t.TempDir()
	full := evs(1, 12)

	// Gera o arquivo completo e descobre onde termina cada evento (antes do '\n').
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	var ends []int // ends[i] = tamanho do arquivo necessário para o evento i estar íntegro
	for i := range full {
		enc.Encode(&full[i])
		ends = append(ends, buf.Len()-1) // sem o '\n'
	}
	data := buf.Bytes()

	extra := disruptor.OrderEvent{OrderID: 999, Price: 5, Quantity: 5, Side: disruptor.Sell}

	for k := 0; k <= len(data); k++ {
		path := filepath.Join(dir, "wal.jsonl")
		if err := os.WriteFile(path, data[:k], 0o644); err != nil {
			t.Fatal(err)
		}

		var want []disruptor.OrderEvent
		for i, end := range ends {
			if end <= k {
				want = append(want, full[i])
			}
		}

		w, err := NewWAL(path)
		if err != nil {
			t.Fatalf("k=%d: NewWAL: %v", k, err)
		}
		got, _ := w.ReadAll()
		if !equalEvents(got, want) {
			t.Fatalf("k=%d: recuperou %d eventos, esperava %d", k, len(got), len(want))
		}

		if err := w.Append([]disruptor.OrderEvent{extra}); err != nil {
			t.Fatalf("k=%d: Append: %v", k, err)
		}
		w.Close()

		// Reabre: tem que ser prefixo + o evento novo, e o arquivo todo tem que ser JSON Lines válido.
		w2 := mustWAL(t, path)
		got, _ = w2.ReadAll()
		w2.Close()
		if !equalEvents(got, append(append([]disruptor.OrderEvent(nil), want...), extra)) {
			t.Fatalf("k=%d: após Append+reabrir veio %d eventos, esperava %d", k, len(got), len(want)+1)
		}
		raw, _ := os.ReadFile(path)
		for _, line := range bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n")) {
			var e disruptor.OrderEvent
			if err := json.Unmarshal(line, &e); err != nil {
				t.Fatalf("k=%d: linha inválida no arquivo reparado: %q", k, line)
			}
		}
	}
}

func TestWAL_GarbageTails(t *testing.T) {
	quietLogs(t)
	cases := map[string]string{
		"lixo binário":       "\x00\x01\x02garbage",
		"json truncado":      `{"OrderID":77,"Pri`,
		"linhas em branco":   "\n\n\n",
		"tipo errado":        `{"OrderID":"abc"}`,
		"json válido + lixo": `{"OrderID":1,"Price":1,"Quantity":1,"Side":0} xyz`,
		"texto solto":        "isto não é um WAL",
	}
	for name, tail := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wal.jsonl")
			w := mustWAL(t, path)
			want := evs(1, 5)
			w.Append(want)
			w.Close()

			f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
			f.WriteString(tail)
			f.Close()

			w2 := mustWAL(t, path)
			defer w2.Close()
			got, _ := w2.ReadAll()
			// "json válido + lixo" começa com um evento íntegro (OrderID 1) colado ao lixo.
			if name == "json válido + lixo" {
				want = append(want, disruptor.OrderEvent{OrderID: 1, Price: 1, Quantity: 1, Side: 0})
			}
			if !equalEvents(got, want) {
				t.Fatalf("recuperou %d eventos, esperava %d", len(got), len(want))
			}
			if err := w2.Append(evs(100, 1)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWAL_ClosedRejectsAndCloseIsIdempotent(t *testing.T) {
	w := mustWAL(t, filepath.Join(t.TempDir(), "wal.jsonl"))
	w.Append(evs(1, 3))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("segundo Close deveria ser no-op, veio %v", err)
	}
	if err := w.Append(evs(1, 1)); !errors.Is(err, ErrWALClosed) {
		t.Fatalf("Append após Close: %v", err)
	}
	if err := w.Sync(); !errors.Is(err, ErrWALClosed) {
		t.Fatalf("Sync após Close: %v", err)
	}
}

// Regressão do bug original: no StartWriter antigo, um erro de Sync fazia
// `return` com o mutex ainda travado, e a goroutine morria. Todo Log/ReadAll
// seguinte travava para sempre. Agora um erro de Sync não pode deixar lock preso.
func TestWAL_SyncErrorDoesNotLeakLock(t *testing.T) {
	w := mustWAL(t, filepath.Join(t.TempDir(), "wal.jsonl"))
	w.Append(evs(1, 3))

	w.file.Close() // sabota o descritor: o próximo Sync falha

	within(t, 5*time.Second, func() {
		if err := w.Sync(); err == nil {
			t.Error("Sync deveria falhar com o arquivo fechado")
		}
		// Se o lock tivesse vazado, qualquer uma destas chamadas travaria.
		if err := w.Sync(); err == nil {
			t.Error("segundo Sync deveria falhar também")
		}
		w.Append(evs(4, 1))
		w.ReadAll()
		w.Close()
	})
}

func TestWAL_ReadAllOnMissingFile(t *testing.T) {
	w := mustWAL(t, filepath.Join(t.TempDir(), "wal.jsonl"))
	defer w.Close()
	os.Remove(w.path)
	got, err := w.ReadAll()
	if err != nil || len(got) != 0 {
		t.Fatalf("arquivo ausente deveria dar lista vazia sem erro, veio %v / %v", got, err)
	}
}
