package storage

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
)

// ErrWALClosed é devolvido por operações em um WAL já fechado.
var ErrWALClosed = errors.New("wal: fechado")

// WAL é o Write-Ahead Log em formato binário (ver wal_format.go): cabeçalho +
// registros de 41 bytes com CRC32C.
//
// Implementa disruptor.Journal: quem o chama é o journaler do RingBuffer, de uma
// única goroutine e em ordem de sequência. Por isso o WAL não tem mais canal nem
// goroutine próprios: a ordem do arquivo é, por construção, a ordem do motor, e
// o chamador decide quando fazer fsync (group commit), em vez de um timer cego.
type WAL struct {
	mu      sync.Mutex
	path    string
	file    *os.File
	scratch []byte // buffer reutilizado por Append (um lote vira um único write)
	closed  bool
}

var _ disruptor.Journal = (*WAL)(nil)

// NewWAL abre (ou cria) o arquivo.
//
//   - arquivo novo ou vazio: grava o cabeçalho;
//   - WAL em JSON Lines (formato antigo): é MIGRADO para o binário, e o original
//     fica ao lado como <arquivo>.legacy-json (ver migrateLegacyJSON);
//   - WAL binário: confere o cabeçalho e valida todos os registros. Se a última
//     escrita foi interrompida por um crash (registro incompleto, bloco zerado,
//     CRC inválido), a cauda é removida — sem isso os próximos Append ficariam
//     depois do lixo e nunca seriam lidos de novo — e guardada num arquivo
//     <arquivo>.dropped-<hora> em vez de simplesmente sumir.
func NewWAL(filename string) (*WAL, error) {
	if err := prepareFile(filename); err != nil {
		return nil, fmt.Errorf("wal: preparando %s: %w", filename, err)
	}
	file, err := os.OpenFile(filename, os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &WAL{path: filename, file: file}, nil
}

type fileKind int

const (
	kindNew    fileKind = iota // vazio, ou cabeçalho incompleto (crash na criação)
	kindBinary                 // começa com o magic "GRWL"
	kindLegacy                 // qualquer outra coisa: tratada como JSON Lines
)

func detectKind(f *os.File) (fileKind, error) {
	var h [headerSize]byte
	n, err := f.ReadAt(h[:], 0)
	if err != nil && err != io.EOF {
		return 0, err
	}
	switch {
	case n == 0 || isHeaderPrefix(h[:n]):
		return kindNew, nil
	case n >= len(walMagic) && [4]byte(h[:4]) == walMagic:
		return kindBinary, nil
	default:
		return kindLegacy, nil
	}
}

func prepareFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	kind, err := detectKind(f)
	if err != nil {
		f.Close()
		return err
	}

	switch kind {
	case kindNew:
		defer f.Close()
		if err := f.Truncate(0); err != nil {
			return err
		}
		h := header()
		if _, err := f.WriteAt(h[:], 0); err != nil {
			return err
		}
		// O cabeçalho precisa estar em disco antes de qualquer registro.
		if err := f.Sync(); err != nil {
			return err
		}
		return syncDir(filepath.Dir(path))

	case kindLegacy:
		f.Close()
		return migrateLegacyJSON(path)

	default:
		defer f.Close()
		return repairBinary(f, path)
	}
}

// repairBinary valida o arquivo inteiro e remove a cauda inválida.
func repairBinary(f *os.File, path string) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	size := info.Size()

	good, scanErr := scanBinary(io.NewSectionReader(f, 0, size), nil)
	if scanErr != nil && scanErr != errBadRecord && scanErr != errTornTail {
		return scanErr // cabeçalho inválido, versão desconhecida, erro de leitura
	}
	if good == size {
		return nil
	}

	dropped := size - good
	sidecar := fmt.Sprintf("%s.dropped-%d", path, time.Now().Unix())
	if err := saveTail(f, good, size, sidecar); err != nil {
		log.Printf("⚠️ WAL: não consegui guardar a cauda descartada em %s: %v", sidecar, err)
	}
	log.Printf("⚠️ WAL com cauda inválida (%v): removendo %d bytes após o último registro íntegro (cópia em %s)",
		scanErr, dropped, sidecar)
	if err := f.Truncate(good); err != nil {
		return err
	}
	return f.Sync()
}

// maxSidecar limita a cópia da cauda descartada (é só para investigação).
const maxSidecar = 16 << 20

func saveTail(f *os.File, from, to int64, dst string) error {
	if to-from > maxSidecar {
		to = from + maxSidecar
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.NewSectionReader(f, from, to-from)); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// migrateLegacyJSON converte um WAL em JSON Lines para o formato binário.
//
// Ordem das operações (cada passo é seguro se o processo cair logo depois):
//  1. escreve o binário completo em <path>.migrating e faz fsync;
//  2. cria o backup <path>.legacy-json como link físico do original (ou cópia, se
//     o sistema de arquivos não suportar links);
//  3. rename(<path>.migrating, <path>), atômico: o original só deixa de ser
//     "o arquivo" depois de o binário estar completo e o backup existir.
//
// Uma queda entre os passos deixa o JSON original intacto e a migração é refeita
// no próximo boot. Uma cauda inválida no JSON é ignorada, como sempre foi.
func migrateLegacyJSON(path string) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	events, _, scanErr := scan(src)
	src.Close()
	if scanErr != nil {
		log.Printf("⚠️ WAL legado com cauda inválida; ignorando o resto na migração: %v", scanErr)
	}

	tmp := path + ".migrating"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(out, 1<<20)
	h := header()
	bw.Write(h[:])
	rec := make([]byte, recordSize)
	for i := range events {
		putRecord(rec, &events[i])
		bw.Write(rec)
	}
	if err := bw.Flush(); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}

	backup := uniqueName(path + ".legacy-json")
	if err := os.Link(path, backup); err != nil {
		if err := copyFile(path, backup); err != nil {
			return fmt.Errorf("guardando o backup do WAL legado: %w", err)
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return err
	}
	log.Printf("🔁 WAL migrado de JSON para binário: %d eventos (original preservado em %s)", len(events), backup)
	return nil
}

func uniqueName(base string) string {
	if _, err := os.Lstat(base); os.IsNotExist(err) {
		return base
	}
	return fmt.Sprintf("%s.%d", base, time.Now().UnixNano())
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// syncDir faz fsync do diretório, para criações e renames sobreviverem a uma queda de energia.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}

// AdoptLegacyName renomeia o WAL do nome antigo (ex.: goruptor_journal.jsonl)
// para o atual, se só o antigo existir. O conteúdo em JSON é migrado por NewWAL.
func AdoptLegacyName(legacy, current string) error {
	if _, err := os.Lstat(current); err == nil {
		return nil // já existe um WAL no nome novo
	}
	if _, err := os.Lstat(legacy); err != nil {
		return nil // nada a adotar
	}
	log.Printf("🔁 WAL antigo encontrado (%s): adotando como %s", legacy, current)
	return os.Rename(legacy, current)
}

// Append grava os eventos, na ordem, e os entrega ao SO antes de retornar
// (sobrevive a crash do processo). Para sobreviver a queda de energia use Sync.
// O lote vira um único write.
func (w *WAL) Append(events []disruptor.OrderEvent) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrWALClosed
	}
	need := len(events) * recordSize
	if cap(w.scratch) < need {
		w.scratch = make([]byte, need)
	}
	b := w.scratch[:need]
	for i := range events {
		putRecord(b[i*recordSize:], &events[i])
	}
	_, err := w.file.Write(b)
	return err
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

// Close faz fsync final e fecha o arquivo. É idempotente.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	syncErr := w.file.Sync()
	closeErr := w.file.Close()
	return errors.Join(syncErr, closeErr)
}

// ReadAll devolve todos os eventos íntegros do arquivo, em ordem.
// Uma cauda inválida (crash no meio de uma escrita) é ignorada com um aviso.
func (w *WAL) ReadAll() ([]disruptor.OrderEvent, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	f, err := os.Open(w.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var events []disruptor.OrderEvent
	if info, err := f.Stat(); err == nil && info.Size() > headerSize {
		events = make([]disruptor.OrderEvent, 0, (info.Size()-headerSize)/recordSize)
	}
	_, scanErr := scanBinary(f, func(e disruptor.OrderEvent) { events = append(events, e) })
	if scanErr != nil {
		if scanErr == errBadRecord || scanErr == errTornTail {
			log.Printf("⚠️ Arquivo WAL corrompido no final. Ignorando resto: %v", scanErr)
		} else {
			return nil, scanErr
		}
	}
	return events, nil
}

// ScanFile lê um WAL SOMENTE para leitura (não repara nem migra nada), aceitando
// o formato binário e o JSON legado. Serve para inspeção e para testes que leem o
// arquivo enquanto o servidor o escreve; uma cauda incompleta é ignorada.
func ScanFile(path string) ([]disruptor.OrderEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var h [headerSize]byte
	n, _ := f.ReadAt(h[:], 0)
	if n >= len(walMagic) && [4]byte(h[:4]) == walMagic {
		var events []disruptor.OrderEvent
		_, err := scanBinary(f, func(e disruptor.OrderEvent) { events = append(events, e) })
		if err != nil && err != errBadRecord && err != errTornTail {
			return nil, err
		}
		return events, nil
	}
	events, _, _ := scan(f)
	return events, nil
}
