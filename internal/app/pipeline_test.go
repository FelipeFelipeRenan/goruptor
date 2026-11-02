package app_test

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/FelipeFelipeRenan/goruptor/internal/api"
)

// Uma rajada única, maior que o tamanho de um lote do servidor, misturando
// ordens válidas, inválidas e duplicadas: os ACKs têm de voltar 1:1 e na ordem
// exata dos frames, e só as válidas podem chegar ao WAL.
func TestTCPPipeline_MixedBurstAcksInOrder(t *testing.T) {
	s := newStack(t)

	const n = 1000
	var wire bytes.Buffer
	want := make([]byte, 0, n)
	valid := 0
	for i := 0; i < n; i++ {
		id := uint64(i + 1)
		switch {
		case i%7 == 3: // preço zero: inválida
			wire.Write(frame(id, 0, 1, 0))
			want = append(want, api.AckInvalid)
		case i%11 == 5: // repete o ID 1 (sempre válido): duplicada
			wire.Write(frame(1, 1000, 1, 0))
			want = append(want, api.AckDuplicate)
		default:
			wire.Write(frame(id, 1000+id%8, 1, byte(id&1)))
			want = append(want, api.AckOK)
			valid++
		}
	}

	conn, err := net.Dial("tcp", s.tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))

	// Tudo num único write: o servidor enxerga a rajada inteira no buffer.
	if _, err := conn.Write(wire.Bytes()); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, n)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("lendo ACKs: %v", err)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ACK %d: veio %d, esperado %d", i, got[i], want[i])
		}
	}

	stopStack(t, s)
	if evs := walEvents(t, s.walPath); len(evs) != valid {
		t.Fatalf("WAL tem %d eventos; esperava %d (só as válidas)", len(evs), valid)
	}
}
