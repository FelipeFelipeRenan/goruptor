package storage

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
)

// Fixa o FORMATO em disco. Os bytes esperados foram calculados por fora (Python,
// CRC32C validado com o vetor "123456789" -> E3069283). Mudar este teste é mudar
// o formato do WAL: exige nova versão e migração.
func TestFormat_GoldenRecord(t *testing.T) {
	e := disruptor.OrderEvent{OrderID: 1, AccountID: 2, Price: 3, Quantity: 4, AssetID: 5, Side: disruptor.Sell}
	b := make([]byte, recordSize)
	putRecord(b, &e)
	const want = "010000000000000002000000000000000300000000000000040000000000000005000000018e40388b"
	if got := hex.EncodeToString(b); got != want {
		t.Fatalf("registro mudou:\n got %s\nwant %s", got, want)
	}
	h := header()
	if string(h[:4]) != "GRWL" || h[4] != 1 || h[5]|h[6]|h[7] != 0 {
		t.Fatalf("cabeçalho mudou: %x", h)
	}
	if recordSize != 41 || headerSize != 8 {
		t.Fatal("tamanhos mudaram")
	}
}

func randEvents(r *rand.Rand, n int) []disruptor.OrderEvent {
	out := make([]disruptor.OrderEvent, n)
	for i := range out {
		pick := func() uint64 {
			switch r.Intn(4) {
			case 0:
				return 0
			case 1:
				return ^uint64(0)
			default:
				return r.Uint64() >> uint(r.Intn(64))
			}
		}
		out[i] = disruptor.OrderEvent{
			OrderID: pick(), AccountID: pick(), Price: pick(), Quantity: pick(),
			AssetID: uint32(pick()), Side: disruptor.Side(pick()),
		}
	}
	return out
}

func TestFormat_RoundTripIncludingExtremes(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	path := filepath.Join(t.TempDir(), "w.wal")
	w := mustWAL(t, path)
	want := randEvents(r, 5000)
	for i := 0; i < len(want); i += 137 { // lotes de tamanhos variados
		end := i + 137
		if end > len(want) {
			end = len(want)
		}
		if err := w.Append(want[i:end]); err != nil {
			t.Fatal(err)
		}
	}
	got, err := w.ReadAll()
	w.Close()
	if err != nil || !equalEvents(got, want) {
		t.Fatalf("round trip falhou: %d/%d err=%v", len(got), len(want), err)
	}
}

// ---- migração do formato JSON ----

func legacyJSON(events []disruptor.OrderEvent) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := range events {
		enc.Encode(&events[i])
	}
	return buf.Bytes()
}

func TestMigration_LegacyJSONBecomesBinaryAndIsPreserved(t *testing.T) {
	quietLogs(t)
	r := rand.New(rand.NewSource(3))
	want := randEvents(r, 300)

	tails := map[string]string{
		"sem cauda":        "",
		"json truncado":    `{"OrderID":77,"Pri`,
		"lixo":             "\x00\x01garbage",
		"sem \\n final":    "\x00", // placeholder trocado abaixo
		"linhas em branco": "\n\n",
	}
	for name, tail := range tails {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "goruptor_journal.wal")
			orig := legacyJSON(want)
			if name == "sem \\n final" {
				orig = bytes.TrimSuffix(orig, []byte("\n"))
				tail = ""
			}
			orig = append(orig, tail...)
			if err := os.WriteFile(path, orig, 0o644); err != nil {
				t.Fatal(err)
			}

			w := mustWAL(t, path)
			got, err := w.ReadAll()
			if err != nil || !equalEvents(got, want) {
				t.Fatalf("migrou %d eventos (err=%v), esperava %d", len(got), err, len(want))
			}
			if err := w.Append(evs(1000, 2)); err != nil {
				t.Fatal(err)
			}
			w.Close()

			// o original ficou intacto ao lado
			bk, _ := filepath.Glob(path + ".legacy-json*")
			if len(bk) != 1 {
				t.Fatalf("esperava 1 backup, achei %v", bk)
			}
			if b, _ := os.ReadFile(bk[0]); !bytes.Equal(b, orig) {
				t.Fatal("o backup difere do arquivo original")
			}
			// e o arquivo principal agora é binário
			raw, _ := os.ReadFile(path)
			if !bytes.HasPrefix(raw, []byte("GRWL")) || len(raw) != headerSize+(len(want)+2)*recordSize {
				t.Fatalf("arquivo principal não está no formato binário esperado (%d bytes)", len(raw))
			}
			if left, _ := filepath.Glob(path + ".migrating"); len(left) != 0 {
				t.Fatal("sobrou arquivo temporário da migração")
			}

			// reabrir não migra de novo nem cria outro backup
			w2 := mustWAL(t, path)
			got, _ = w2.ReadAll()
			w2.Close()
			if len(got) != len(want)+2 {
				t.Fatalf("após reabrir: %d eventos", len(got))
			}
			if bk2, _ := filepath.Glob(path + ".legacy-json*"); len(bk2) != 1 {
				t.Fatalf("reabrir criou outro backup: %v", bk2)
			}
		})
	}
}

// Queda durante a migração: sobra um .migrating (incompleto ou lixo) e o JSON original
// continua sendo "o arquivo". O próximo boot migra normalmente.
func TestMigration_RecoversFromInterruptedAttempt(t *testing.T) {
	quietLogs(t)
	want := evs(1, 50)
	path := filepath.Join(t.TempDir(), "j.wal")
	os.WriteFile(path, legacyJSON(want), 0o644)
	os.WriteFile(path+".migrating", []byte("GRWL\x01\x00\x00\x00meio-escrito"), 0o644)
	// backup de uma tentativa anterior (link criado, rename não aconteceu)
	os.WriteFile(path+".legacy-json", legacyJSON(want), 0o644)

	w := mustWAL(t, path)
	got, _ := w.ReadAll()
	w.Close()
	if !equalEvents(got, want) {
		t.Fatalf("migrou %d eventos, esperava %d", len(got), len(want))
	}
	if bk, _ := filepath.Glob(path + ".legacy-json*"); len(bk) != 2 {
		t.Fatalf("o backup anterior não pode ser sobrescrito: %v", bk)
	}
}

func TestMigration_EmptyOrGarbageLegacyFileIsKeptAsBackup(t *testing.T) {
	quietLogs(t)
	path := filepath.Join(t.TempDir(), "j.wal")
	garbage := []byte("isto não é um WAL de forma alguma")
	os.WriteFile(path, garbage, 0o644)

	w := mustWAL(t, path)
	got, _ := w.ReadAll()
	w.Close()
	if len(got) != 0 {
		t.Fatalf("lixo virou %d eventos", len(got))
	}
	bk, _ := filepath.Glob(path + ".legacy-json*")
	if len(bk) != 1 {
		t.Fatalf("o arquivo original deveria ter sido preservado: %v", bk)
	}
	if b, _ := os.ReadFile(bk[0]); !bytes.Equal(b, garbage) {
		t.Fatal("backup difere")
	}
}

// ---- cabeçalho e versão ----

func TestHeader_UnsupportedVersionRefusesAndLeavesFileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v9.wal")
	content := append([]byte("GRWL\x09\x00\x00\x00"), make([]byte, recordSize)...)
	os.WriteFile(path, content, 0o644)

	_, err := NewWAL(path)
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("esperava ErrUnsupportedVersion, veio %v", err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, content) {
		t.Fatal("arquivo de versão desconhecida foi modificado")
	}
}

func TestHeader_PartialHeaderFromCrashAtCreationIsRewritten(t *testing.T) {
	h := header()
	for n := 0; n < headerSize; n++ {
		path := filepath.Join(t.TempDir(), "p.wal")
		os.WriteFile(path, h[:n], 0o644)
		w := mustWAL(t, path)
		if err := w.Append(evs(1, 3)); err != nil {
			t.Fatal(err)
		}
		got, _ := w.ReadAll()
		w.Close()
		if len(got) != 3 {
			t.Fatalf("n=%d: %d eventos", n, len(got))
		}
	}
}

// ---- corrupção no MEIO do arquivo ----

// Política (igual à do JSON): o WAL vale até o primeiro registro inválido; o resto é
// removido, mas guardado ao lado para investigação em vez de sumir.
func TestCorruption_MidFileKeepsPrefixAndSavesTheRest(t *testing.T) {
	quietLogs(t)
	path := filepath.Join(t.TempDir(), "m.wal")
	w := mustWAL(t, path)
	all := evs(1, 10)
	w.Append(all)
	w.Close()

	raw, _ := os.ReadFile(path)
	bad := headerSize + 3*recordSize + 10 // dentro do registro de índice 3
	raw[bad] ^= 0x40
	os.WriteFile(path, raw, 0o644)

	w2 := mustWAL(t, path)
	got, _ := w2.ReadAll()
	w2.Close()
	if !equalEvents(got, all[:3]) {
		t.Fatalf("esperava os 3 primeiros, veio %d", len(got))
	}
	side, _ := filepath.Glob(path + ".dropped-*")
	if len(side) != 1 {
		t.Fatalf("cauda deveria ter sido guardada: %v", side)
	}
	if b, _ := os.ReadFile(side[0]); !bytes.Equal(b, raw[headerSize+3*recordSize:]) {
		t.Fatal("cópia da cauda difere")
	}
}

// ---- ScanFile é somente leitura ----

func TestScanFile_ReadOnlyAndToleratesTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.wal")
	w := mustWAL(t, path)
	w.Append(evs(1, 4))
	w.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.Write([]byte("rasgado"))
	f.Close()
	before, _ := os.ReadFile(path)

	got, err := ScanFile(path)
	if err != nil || !equalEvents(got, evs(1, 4)) {
		t.Fatalf("ScanFile: %d eventos, err=%v", len(got), err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("ScanFile modificou o arquivo")
	}

	// também lê o formato legado, sem migrar
	leg := filepath.Join(t.TempDir(), "l.jsonl")
	os.WriteFile(leg, legacyJSON(evs(1, 3)), 0o644)
	got, err = ScanFile(leg)
	if err != nil || len(got) != 3 {
		t.Fatalf("ScanFile legado: %d, %v", len(got), err)
	}
	if bk, _ := filepath.Glob(leg + ".*"); len(bk) != 0 {
		t.Fatal("ScanFile não pode criar arquivos")
	}
}

func TestAdoptLegacyName(t *testing.T) {
	quietLogs(t)
	dir := t.TempDir()
	legacy, current := filepath.Join(dir, "a.jsonl"), filepath.Join(dir, "a.wal")

	if err := AdoptLegacyName(legacy, current); err != nil { // nada existe
		t.Fatal(err)
	}
	os.WriteFile(legacy, []byte("L"), 0o644)
	os.WriteFile(current, []byte("C"), 0o644)
	AdoptLegacyName(legacy, current) // os dois existem: não mexe em nada
	if b, _ := os.ReadFile(current); string(b) != "C" {
		t.Fatal("sobrescreveu o WAL atual")
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal("removeu o legado com WAL atual presente")
	}
	os.Remove(current)
	AdoptLegacyName(legacy, current) // só o legado: adota
	if b, _ := os.ReadFile(current); string(b) != "L" {
		t.Fatal("não adotou o legado")
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("legado deveria ter sido renomeado")
	}
}

// ---- fuzz ----

// scanBinary nunca entra em pânico; o offset é sempre alinhado; e re-codificar o
// que foi lido reproduz exatamente os bytes do prefixo íntegro.
func FuzzScanBinary(f *testing.F) {
	h := header()
	rec := make([]byte, recordSize)
	e := evs(1, 1)[0]
	putRecord(rec, &e)
	f.Add(append(append([]byte{}, h[:]...), rec...))
	f.Add(append(append([]byte{}, h[:]...), rec[:20]...))
	f.Add([]byte("GRWL"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		var got []disruptor.OrderEvent
		good, err := scanBinary(bytes.NewReader(data), func(e disruptor.OrderEvent) { got = append(got, e) })
		if err != nil && err != errBadRecord && err != errTornTail {
			if good != 0 || len(got) != 0 {
				t.Fatalf("erro de formato com progresso: good=%d eventos=%d", good, len(got))
			}
			return
		}
		if good == 0 { // vazio / cabeçalho incompleto
			if len(got) != 0 {
				t.Fatal("eventos sem cabeçalho")
			}
			return
		}
		if good < headerSize || good > int64(len(data)) || (good-headerSize)%recordSize != 0 {
			t.Fatalf("offset inválido: %d (len=%d)", good, len(data))
		}
		if int64(len(got)) != (good-headerSize)/recordSize {
			t.Fatalf("contagem inconsistente")
		}
		rebuilt := make([]byte, len(got)*recordSize)
		for i := range got {
			putRecord(rebuilt[i*recordSize:], &got[i])
		}
		if !bytes.Equal(rebuilt, data[headerSize:good]) {
			t.Fatal("re-codificar não reproduz o prefixo íntegro")
		}
	})
}
