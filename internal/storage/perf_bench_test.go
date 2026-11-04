package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
)

func benchEvents(n int) []disruptor.OrderEvent {
	out := make([]disruptor.OrderEvent, n)
	for i := range out {
		id := uint64(i + 1)
		out[i] = disruptor.OrderEvent{OrderID: id, Price: 60000 + id%997, Quantity: 1 + id%9, Side: disruptor.Side(id & 1)}
	}
	return out
}

// BenchmarkWAL_Append: custo de CPU de gravar eventos no WAL (codificação + write),
// SEM fsync. É o teto de vazão do journaler quando o disco é rápido.
func BenchmarkWAL_Append(b *testing.B) {
	path := filepath.Join(b.TempDir(), "wal.wal")
	w, err := NewWAL(path)
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()
	batch := benchEvents(256)

	b.ReportAllocs()
	b.SetBytes(0)
	b.ResetTimer()
	for i := 0; i < b.N; i += len(batch) {
		if err := w.Append(batch); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "events/s")
	if info, err := os.Stat(path); err == nil {
		b.ReportMetric(float64(info.Size())/float64(b.N), "bytes/event")
	}
}

// BenchmarkWAL_ReadAll: tempo de boot (replay) por evento. O WAL nunca é truncado,
// então este custo cresce com a vida do sistema.
func BenchmarkWAL_ReadAll(b *testing.B) {
	const n = 200_000
	path := filepath.Join(b.TempDir(), "wal.wal")
	w, _ := NewWAL(path)
	for i := 0; i < n; i += 1000 {
		w.Append(benchEvents(1000))
	}
	w.Close()

	if info, err := os.Stat(path); err == nil {
		defer b.ReportMetric(float64(info.Size())/n, "bytes/event")
	}
	w2, _ := NewWAL(path)
	defer w2.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		evs, err := w2.ReadAll()
		if err != nil || len(evs) != n {
			b.Fatalf("len=%d err=%v", len(evs), err)
		}
	}
	b.ReportMetric(float64(b.N*n)/b.Elapsed().Seconds(), "events/s")
}

// BenchmarkWAL_Boot: o que o servidor faz ao subir: NewWAL (valida e repara o arquivo
// inteiro) + ReadAll (lê de novo para reconstruir o livro), com 500 mil eventos.
func BenchmarkWAL_Boot(b *testing.B) {
	const n = 500_000
	path := filepath.Join(b.TempDir(), "wal.wal")
	w, _ := NewWAL(path)
	for i := 0; i < n; i += 1000 {
		w.Append(benchEvents(1000))
	}
	w.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w2, err := NewWAL(path)
		if err != nil {
			b.Fatal(err)
		}
		evs, err := w2.ReadAll()
		w2.Close()
		if err != nil || len(evs) != n {
			b.Fatalf("len=%d err=%v", len(evs), err)
		}
	}
	b.ReportMetric(float64(b.N*n)/b.Elapsed().Seconds(), "events/s")
}

// BenchmarkBuildInsert: monta o INSERT em massa de um lote de 1000 ordens.
func BenchmarkBuildInsert(b *testing.B) {
	batch := benchEvents(1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buildInsert(batch)
	}
}
