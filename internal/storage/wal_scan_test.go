package storage

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
)

// O caminho rápido de scan tem de ser INDISTINGUÍVEL do json.Decoder: mesmos
// eventos, mesmo offset íntegro e mesmo veredito de erro, para qualquer entrada.
func assertScanEqualsSlow(t *testing.T, data []byte) {
	t.Helper()
	gotEv, gotOff, gotErr := scan(bytes.NewReader(data))
	wantEv, wantOff, wantErr := scanSlow(bytes.NewReader(data), nil, 0, 0)
	if !reflect.DeepEqual(gotEv, wantEv) && !(len(gotEv) == 0 && len(wantEv) == 0) {
		t.Fatalf("eventos diferem em %q\n got %d eventos, want %d", abbreviate(data), len(gotEv), len(wantEv))
	}
	if gotOff != wantOff || (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("offset/erro diferem em %q: got (%d, %v) want (%d, %v)", abbreviate(data), gotOff, gotErr, wantOff, wantErr)
	}
}

func abbreviate(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}

func encodeAll(evs []disruptor.OrderEvent) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := range evs {
		enc.Encode(&evs[i])
	}
	return buf.Bytes()
}

func randEvent(r *rand.Rand) disruptor.OrderEvent {
	pick := func() uint64 {
		switch r.Intn(5) {
		case 0:
			return 0
		case 1:
			return ^uint64(0)
		case 2:
			return uint64(r.Intn(10))
		default:
			return r.Uint64() >> uint(r.Intn(64))
		}
	}
	return disruptor.OrderEvent{
		OrderID: pick(), AccountID: pick(), Price: pick(), Quantity: pick(),
		AssetID: uint32(pick()), Side: disruptor.Side(pick()),
	}
}

func TestScan_FastPathEqualsDecoder_ValidFiles(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for n := 0; n < 200; n++ {
		evs := make([]disruptor.OrderEvent, r.Intn(50))
		for i := range evs {
			evs[i] = randEvent(r)
		}
		data := encodeAll(evs)
		assertScanEqualsSlow(t, data)
		got, off, err := scan(bytes.NewReader(data))
		if err != nil || len(got) != len(evs) || (len(evs) > 0 && !reflect.DeepEqual(got, evs)) {
			t.Fatalf("round trip falhou: %d/%d err=%v", len(got), len(evs), err)
		}
		if len(evs) > 0 && off != int64(len(data))-1 {
			t.Fatalf("offset %d, esperado %d", off, len(data)-1)
		}
	}
}

// Arquivos com defeitos: bytes trocados/removidos/inseridos, cortes, lixo.
func TestScan_FastPathEqualsDecoder_CorruptedFiles(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	junk := []string{"", " ", "\n", "\r\n", "x", "{", "}", `"`, "0", "-", "1e3", ".5", "null", "\x00", "{}", `{"OrderID":1}`}
	for n := 0; n < 5000; n++ {
		evs := make([]disruptor.OrderEvent, 1+r.Intn(8))
		for i := range evs {
			evs[i] = randEvent(r)
		}
		data := encodeAll(evs)
		for k := r.Intn(3); k >= 0; k-- {
			if len(data) == 0 {
				break
			}
			i := r.Intn(len(data))
			switch r.Intn(4) {
			case 0:
				data[i] = byte(r.Intn(256))
			case 1:
				data = append(data[:i], data[i+1:]...)
			case 2:
				j := junk[r.Intn(len(junk))]
				data = append(data[:i], append([]byte(j), data[i:]...)...)
			case 3:
				data = data[:i]
			}
		}
		assertScanEqualsSlow(t, data)
	}
}

// Valores-limite que o caminho rápido rejeita ou aceita por conta própria.
func TestScan_FastPathEqualsDecoder_Edges(t *testing.T) {
	line := func(id, asset, side string) string {
		return `{"OrderID":` + id + `,"AccountID":0,"Price":1,"Quantity":1,"AssetID":` + asset + `,"Side":` + side + "}\n"
	}
	cases := []string{
		line("1", "0", "0"),
		line("18446744073709551615", "4294967295", "255"), // máximos
		line("18446744073709551616", "0", "0"),            // estoura uint64
		line("1", "4294967296", "0"),                      // estoura uint32
		line("1", "0", "256"),                             // estoura uint8
		line("01", "0", "0"),                              // zero à esquerda
		line("-1", "0", "0"),
		line("1.0", "0", "0"),
		line("1e2", "0", "0"),
		strings.TrimSuffix(line("1", "0", "0"), "\n"),          // sem \n final
		line("1", "0", "0") + "\n\n" + line("2", "0", "1"),     // linha vazia no meio
		line("1", "0", "0") + `{"OrderID":2`,                   // cauda cortada
		"  " + line("1", "0", "0"),                             // espaço antes
		line("1", "0", "0") + line("2", "0", "1") + "\x00\x00", // lixo ao final
		`{"Side":1,"OrderID":5,"AccountID":0,"Price":1,"Quantity":1,"AssetID":0}` + "\n", // ordem de chaves diferente
		"",
	}
	for _, c := range cases {
		assertScanEqualsSlow(t, []byte(c))
	}
}

// Linhas maiores que o buffer de leitura (ReadSlice -> ErrBufferFull) caem no Decoder.
func TestScan_FastPathEqualsDecoder_HugeLine(t *testing.T) {
	data := []byte(strings.Repeat(" ", 300<<10) + `{"OrderID":1,"AccountID":0,"Price":1,"Quantity":1,"AssetID":0,"Side":0}` + "\n")
	assertScanEqualsSlow(t, data)
	evs, _, _ := scan(bytes.NewReader(data))
	if len(evs) != 1 {
		t.Fatalf("esperava 1 evento, veio %d", len(evs))
	}
}

func FuzzScan(f *testing.F) {
	f.Add([]byte(`{"OrderID":1,"AccountID":0,"Price":1,"Quantity":1,"AssetID":0,"Side":0}` + "\n"))
	f.Add([]byte(`{"OrderID":1,"AccountID":0,"Price":1`))
	f.Fuzz(func(t *testing.T, data []byte) { assertScanEqualsSlow(t, data) })
}
