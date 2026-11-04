package storage

// Leitura do formato LEGADO do WAL (JSON Lines). Só é usada para migrar um WAL
// antigo para o formato binário (ver wal.go, migrateLegacyJSON) e para ScanFile.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"math"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
)

// scan lê eventos até o fim ou até o primeiro registro inválido.
// Devolve os eventos, o offset logo após o último íntegro e o erro que parou a leitura (nil = EOF limpo).
//
// Caminho rápido: o WAL é escrito pelo próprio encoder, então quase toda linha
// tem o formato canônico e é decodificada à mão (sem reflection nem alocação por
// evento). Na primeira linha fora do padrão (cauda cortada, lixo, espaços, JSON
// reformatado), a leitura continua dali com o json.Decoder, que decide o que é
// íntegro. O resultado é idêntico ao de scanSlow (ver wal_scan_test.go).
func scan(r io.Reader) (events []disruptor.OrderEvent, goodOffset int64, err error) {
	br := bufio.NewReaderSize(r, 256<<10)
	var pos int64
	for {
		line, rerr := br.ReadSlice('\n')
		if rerr == nil {
			if ev, ok := parseCanonical(line[:len(line)-1]); ok {
				events = append(events, ev)
				goodOffset = pos + int64(len(line)) - 1 // logo após o '}', como o Decoder
				pos += int64(len(line))
				continue
			}
		} else if len(line) == 0 && rerr == io.EOF {
			return events, goodOffset, nil
		}
		// Fora do padrão (ou erro de leitura): devolve a linha ao fluxo e segue no Decoder.
		rest := io.MultiReader(bytes.NewReader(append([]byte(nil), line...)), br)
		return scanSlow(rest, events, pos, goodOffset)
	}
}

// scanSlow é a leitura genérica com json.Decoder. base é o offset do início de r
// no arquivo; events/good são o que já foi lido antes.
func scanSlow(r io.Reader, events []disruptor.OrderEvent, base, good int64) ([]disruptor.OrderEvent, int64, error) {
	dec := json.NewDecoder(r)
	for dec.More() {
		var ev disruptor.OrderEvent
		if err := dec.Decode(&ev); err != nil {
			return events, good, err
		}
		events = append(events, ev)
		good = base + dec.InputOffset()
	}
	return events, good, nil
}

var canonicalKeys = [6]string{
	`{"OrderID":`, `,"AccountID":`, `,"Price":`, `,"Quantity":`, `,"AssetID":`, `,"Side":`,
}

// parseCanonical decodifica exatamente o que json.Encoder produz para OrderEvent
// (sem o '\n'). Qualquer desvio devolve ok=false e o chamador usa o json.Decoder.
func parseCanonical(b []byte) (ev disruptor.OrderEvent, ok bool) {
	var vals [6]uint64
	for i, key := range canonicalKeys {
		if len(b) < len(key) || string(b[:len(key)]) != key {
			return ev, false
		}
		b = b[len(key):]
		var v uint64
		n := 0
		for n < len(b) && b[n] >= '0' && b[n] <= '9' {
			d := uint64(b[n] - '0')
			if v > (math.MaxUint64-d)/10 {
				return ev, false // estouro de uint64
			}
			v = v*10 + d
			n++
		}
		if n == 0 || (n > 1 && b[0] == '0') { // sem dígitos ou zero à esquerda (JSON inválido)
			return ev, false
		}
		vals[i] = v
		b = b[n:]
	}
	if len(b) != 1 || b[0] != '}' || vals[4] > math.MaxUint32 || vals[5] > math.MaxUint8 {
		return ev, false
	}
	return disruptor.OrderEvent{
		OrderID: vals[0], AccountID: vals[1], Price: vals[2], Quantity: vals[3],
		AssetID: uint32(vals[4]), Side: disruptor.Side(vals[5]),
	}, true
}
