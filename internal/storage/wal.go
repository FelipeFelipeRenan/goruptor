package storage

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
)

// ErrWALClosed é devolvido por operações em um WAL já fechado.
var ErrWALClosed = errors.New("wal: fechado")

// WAL é o Write-Ahead Log em JSON Lines (um evento por linha).
//
// Implementa disruptor.Journal: quem o chama é o journaler do RingBuffer, de uma
// única goroutine e em ordem de sequência. Por isso o WAL não tem mais canal nem
// goroutine próprios: a ordem do arquivo é, por construção, a ordem do motor, e
// o chamador decide quando fazer fsync (group commit), em vez de um timer cego.
type WAL struct {
	mu     sync.Mutex
	path   string
	file   *os.File
	bw     *bufio.Writer
	enc    *json.Encoder
	closed bool
}

var _ disruptor.Journal = (*WAL)(nil)

// NewWAL abre (ou cria) o arquivo. Se a última escrita foi interrompida por um
// crash e deixou uma linha truncada, a cauda inválida é removida: sem isso, os
// próximos Append ficariam colados no lixo e o arquivo inteiro ficaria ilegível
// a partir daquele ponto.
func NewWAL(filename string) (*WAL, error) {
	file, err := os.OpenFile(filename, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}

	if err := repairTail(file); err != nil {
		file.Close()
		return nil, fmt.Errorf("wal: reparando cauda de %s: %w", filename, err)
	}

	bw := bufio.NewWriterSize(file, 64*1024)
	return &WAL{
		path: filename,
		file: file,
		bw:   bw,
		enc:  json.NewEncoder(bw), // Encode já termina cada valor com '\n'
	}, nil
}

// repairTail remove bytes inválidos depois do último evento íntegro e garante
// que o arquivo termine em '\n'.
func repairTail(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size == 0 {
		return nil
	}

	_, good, _ := scan(io.NewSectionReader(f, 0, size))
	switch {
	case good == 0:
		// nenhum evento íntegro: arquivo inteiro é lixo (ou só espaços)
		log.Printf("⚠️ WAL sem eventos íntegros; descartando %d bytes", size)
		return f.Truncate(0)
	case size == good+1:
		return nil // saudável: termina em '\n'
	case size == good:
		_, err = f.Write([]byte{'\n'}) // falta só o '\n' final
		return err
	default:
		log.Printf("⚠️ WAL com cauda corrompida: removendo %d bytes após o último evento íntegro", size-good)
		if err := f.Truncate(good); err != nil {
			return err
		}
		_, err = f.Write([]byte{'\n'})
		return err
	}
}

// scan lê eventos até o fim ou até o primeiro registro inválido.
// Devolve os eventos, o offset logo após o último íntegro e o erro que parou a leitura (nil = EOF limpo).
func scan(r io.Reader) (events []disruptor.OrderEvent, goodOffset int64, err error) {
	dec := json.NewDecoder(r)
	for dec.More() {
		var ev disruptor.OrderEvent
		if err = dec.Decode(&ev); err != nil {
			return events, goodOffset, err
		}
		events = append(events, ev)
		goodOffset = dec.InputOffset()
	}
	return events, goodOffset, nil
}

// Append grava os eventos, na ordem, e os entrega ao SO antes de retornar
// (sobrevive a crash do processo). Para sobreviver a queda de energia use Sync.
func (w *WAL) Append(events []disruptor.OrderEvent) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrWALClosed
	}
	for i := range events {
		if err := w.enc.Encode(&events[i]); err != nil {
			return err
		}
	}
	return w.bw.Flush()
}

// Sync força o conteúdo para o armazenamento estável (fsync).
// O lock é sempre liberado, inclusive em caso de erro.
func (w *WAL) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrWALClosed
	}
	return w.file.Sync()
}

// Close faz flush, fsync final e fecha o arquivo. É idempotente.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	flushErr := w.bw.Flush()
	syncErr := w.file.Sync()
	closeErr := w.file.Close()
	return errors.Join(flushErr, syncErr, closeErr)
}

// ReadAll devolve todos os eventos íntegros do arquivo, em ordem.
// Uma cauda inválida (crash no meio de uma escrita) é ignorada com um aviso.
func (w *WAL) ReadAll() ([]disruptor.OrderEvent, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	readFile, err := os.Open(w.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer readFile.Close()

	events, _, scanErr := scan(bufio.NewReader(readFile))
	if scanErr != nil {
		log.Printf("⚠️ Arquivo WAL corrompido no final. Ignorando resto: %v", scanErr)
	}
	return events, nil
}
