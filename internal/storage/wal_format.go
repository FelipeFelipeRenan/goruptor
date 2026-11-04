package storage

// Formato binário do WAL (versão 1).
//
//	arquivo   = cabeçalho (8 bytes) + registros (41 bytes cada)
//	cabeçalho = "GRWL" | versão (1 byte) | 3 bytes zero
//	registro  = OrderID u64 | AccountID u64 | Price u64 | Quantity u64 | AssetID u32 | Side u8 | CRC32C u32
//
// Tudo little-endian. O CRC32C cobre os 37 primeiros bytes do registro. Um
// registro rasgado por crash (escrita parcial, bloco zerado, lixo) quase nunca
// passa no CRC, e a recuperação para no primeiro registro inválido. O tamanho é
// fixo: o arquivo tem (tamanho-8)/41 registros, e o offset de qualquer um é
// conhecido sem ler os anteriores.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
)

const (
	headerSize    = 8
	recordSize    = 41
	payloadSize   = 37 // bytes cobertos pelo CRC
	formatVersion = 1
)

var (
	walMagic = [4]byte{'G', 'R', 'W', 'L'}
	le       = binary.LittleEndian
	crcTable = crc32.MakeTable(crc32.Castagnoli)

	// ErrUnknownFormat: o arquivo não é um WAL binário nem JSON legado reconhecível.
	ErrUnknownFormat = errors.New("wal: formato desconhecido")
	// ErrUnsupportedVersion: WAL binário de uma versão que este código não entende.
	ErrUnsupportedVersion = errors.New("wal: versão do formato não suportada")

	errBadRecord = errors.New("wal: registro inválido (CRC)")
	errTornTail  = errors.New("wal: registro incompleto no fim do arquivo")
)

func header() [headerSize]byte {
	return [headerSize]byte{walMagic[0], walMagic[1], walMagic[2], walMagic[3], formatVersion, 0, 0, 0}
}

// putRecord grava o registro de e em b (len(b) >= recordSize).
func putRecord(b []byte, e *disruptor.OrderEvent) {
	le.PutUint64(b[0:], e.OrderID)
	le.PutUint64(b[8:], e.AccountID)
	le.PutUint64(b[16:], e.Price)
	le.PutUint64(b[24:], e.Quantity)
	le.PutUint32(b[32:], e.AssetID)
	b[36] = byte(e.Side)
	le.PutUint32(b[payloadSize:], crc32.Checksum(b[:payloadSize], crcTable))
}

// getRecord decodifica b (len(b) >= recordSize); ok=false se o CRC não confere.
func getRecord(b []byte) (e disruptor.OrderEvent, ok bool) {
	if crc32.Checksum(b[:payloadSize], crcTable) != le.Uint32(b[payloadSize:]) {
		return e, false
	}
	return disruptor.OrderEvent{
		OrderID:   le.Uint64(b[0:]),
		AccountID: le.Uint64(b[8:]),
		Price:     le.Uint64(b[16:]),
		Quantity:  le.Uint64(b[24:]),
		AssetID:   le.Uint32(b[32:]),
		Side:      disruptor.Side(b[36]),
	}, true
}

// checkHeader valida os 8 primeiros bytes.
func checkHeader(h []byte) error {
	if len(h) < headerSize || [4]byte(h[:4]) != walMagic {
		return ErrUnknownFormat
	}
	if h[4] != formatVersion {
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, h[4])
	}
	return nil
}

// isHeaderPrefix diz se b (menor que o cabeçalho) é o começo de um cabeçalho válido:
// crash no meio da criação do arquivo.
func isHeaderPrefix(b []byte) bool {
	h := header()
	return len(b) < headerSize && string(b) == string(h[:len(b)])
}

// scanBinary lê o cabeçalho e os registros de r (que começa no byte 0 do arquivo),
// chamando sink (se não for nil) para cada evento íntegro. Devolve o offset logo
// após o último registro íntegro (>= headerSize) e o motivo da parada: nil (fim
// limpo), errTornTail (bytes sobrando < 1 registro), errBadRecord (CRC), ou um
// erro de leitura / de cabeçalho (nesses o offset é 0).
func scanBinary(r io.Reader, sink func(disruptor.OrderEvent)) (good int64, err error) {
	br := bufio.NewReaderSize(r, 1<<20)

	var h [headerSize]byte
	if n, err := io.ReadFull(br, h[:]); err != nil {
		if (err == io.EOF || err == io.ErrUnexpectedEOF) && isHeaderPrefix(h[:n]) {
			return 0, nil // vazio ou cabeçalho incompleto: nada a ler
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return 0, ErrUnknownFormat
		}
		return 0, err
	}
	if err := checkHeader(h[:]); err != nil {
		return 0, err
	}

	good = headerSize
	buf := make([]byte, recordSize*4096)
	for {
		n, rerr := io.ReadFull(br, buf)
		for off := 0; off+recordSize <= n; off += recordSize {
			ev, ok := getRecord(buf[off:])
			if !ok {
				return good, errBadRecord
			}
			if sink != nil {
				sink(ev)
			}
			good += recordSize
		}
		if rerr != nil {
			if rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
				return good, rerr
			}
			if n%recordSize != 0 {
				return good, errTornTail
			}
			return good, nil
		}
	}
}
