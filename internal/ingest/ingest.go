// Package ingest é a única porta de entrada de ordens do servidor, usada tanto
// pelo HTTP quanto pelo TCP binário.
//
// Uma ordem passa, nesta sequência, por:
//
//  1. Validação (Validate): rejeita lixo antes de ele chegar ao motor.
//  2. Deduplicação de order_id (IDSet): a mesma ordem nunca é executada duas vezes.
//  3. Publish no anel (lock-free). O journaler grava a ordem NA ORDEM DA SEQUÊNCIA.
//  4. WaitDurable: espera a ordem estar em disco. Só depois disso o cliente recebe o ACK.
//  5. Archive: só ordens confirmadas vão para o Postgres.
package ingest

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
)

// Limites aceitos pelo servidor. Existem para que a soma de quantidades de um
// nível de preço (uint64) nunca estoure: com MaxQuantity = 1e9 seriam
// necessárias ~1,8e10 ordens no mesmo nível para dar a volta.
const (
	MaxPrice    uint64 = 1_000_000_000_000_000 // 1e15
	MaxQuantity uint64 = 1_000_000_000         // 1e9
)

var (
	// ErrInvalidOrder: a ordem é malformada. Erro do CLIENTE; reenviar igual não adianta.
	ErrInvalidOrder = errors.New("ordem inválida")

	// ErrDuplicateOrder: o order_id já foi usado. Erro do CLIENTE.
	ErrDuplicateOrder = errors.New("order_id já utilizado")

	// ErrUnavailable: o servidor NÃO confirmou a ordem (desligando, ou o disco falhou).
	// Erro do SERVIDOR. O cliente não deve assumir que a ordem existe.
	ErrUnavailable = errors.New("ordem não confirmada")
)

// ParseSide converte "BUY"/"SELL" (sem diferenciar maiúsculas) em um lado.
// Qualquer outro valor é rejeitado: antes, tudo que não fosse "SELL" virava compra.
func ParseSide(s string) (disruptor.Side, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "BUY":
		return disruptor.Buy, nil
	case "SELL":
		return disruptor.Sell, nil
	}
	return 0, fmt.Errorf("%w: side deve ser \"BUY\" ou \"SELL\"", ErrInvalidOrder)
}

// Validate confere os invariantes de uma ordem.
func Validate(e disruptor.OrderEvent) error {
	switch {
	case e.OrderID == 0:
		return fmt.Errorf("%w: order_id deve ser maior que zero", ErrInvalidOrder)
	case e.Side != disruptor.Buy && e.Side != disruptor.Sell:
		return fmt.Errorf("%w: side inválido (%d)", ErrInvalidOrder, e.Side)
	case e.Price == 0 || e.Price > MaxPrice:
		return fmt.Errorf("%w: price deve estar entre 1 e %d", ErrInvalidOrder, MaxPrice)
	case e.Quantity == 0 || e.Quantity > MaxQuantity:
		return fmt.Errorf("%w: quantity deve estar entre 1 e %d", ErrInvalidOrder, MaxQuantity)
	}
	return nil
}

// Publisher é o que a entrada precisa do RingBuffer.
type Publisher interface {
	Publish(event disruptor.OrderEvent) (uint64, error)
	WaitDurable(seq uint64) error
}

// Archiver recebe as ordens já confirmadas (ex.: o Batcher do Postgres).
type Archiver interface {
	Push(event disruptor.OrderEvent) error
}

// Ingress valida, deduplica e confirma ordens.
type Ingress struct {
	pub Publisher
	arc Archiver
	ids *IDSet
}

// New cria a entrada de ordens.
func New(pub Publisher, arc Archiver) *Ingress {
	return &Ingress{pub: pub, arc: arc, ids: NewIDSet()}
}

// IDs expõe o conjunto de order_ids já vistos. No boot, o recovery deve
// registrar aqui os IDs reprocessados do WAL, para que um reinício não
// reabra a porta para ordens duplicadas.
func (in *Ingress) IDs() *IDSet { return in.ids }

// Submit processa uma ordem. Retorna nil SOMENTE quando a ordem está confirmada
// em disco. Os erros se classificam com errors.Is:
//
//	ErrInvalidOrder, ErrDuplicateOrder -> culpa do cliente
//	ErrUnavailable                     -> servidor não confirmou a ordem
func (in *Ingress) Submit(event disruptor.OrderEvent) error {
	if err := Validate(event); err != nil {
		return err
	}
	if !in.ids.Register(event.OrderID) {
		return fmt.Errorf("%w: %d", ErrDuplicateOrder, event.OrderID)
	}

	seq, err := in.pub.Publish(event)
	if err != nil {
		// Nada foi para o anel: liberar o ID permite ao cliente tentar de novo
		// (por exemplo, depois do reinício do servidor).
		in.ids.Forget(event.OrderID)
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}

	if err := in.pub.WaitDurable(seq); err != nil {
		// A ordem JÁ está no anel e pode ou não ter chegado ao disco: resultado
		// ambíguo. O ID continua registrado de propósito, para ela não ser
		// executada duas vezes caso o cliente reenvie. Depois de um reinício, o
		// replay do WAL decide a verdade.
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}

	// A ordem já está durável: uma falha de arquivamento não a desfaz.
	if err := in.arc.Push(event); err != nil {
		log.Printf("⚠️ ordem %d confirmada mas não arquivada no Postgres: %v", event.OrderID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// IDSet
// ---------------------------------------------------------------------------

const idShards = 64

// IDSet é um conjunto concorrente de order_ids, dividido em shards para que
// centenas de conexões não disputem o mesmo lock.
//
// Custo: o conjunto cresce com o número de ordens aceitas (~40-50 bytes por
// ordem, ou seja ~50 MB por milhão de ordens). Para um laboratório está ótimo;
// em produção troque por uma janela de tempo ou um filtro de Bloom.
type IDSet struct {
	shards [idShards]struct {
		mu sync.Mutex
		m  map[uint64]struct{}
	}
}

// NewIDSet cria um conjunto vazio.
func NewIDSet() *IDSet {
	s := &IDSet{}
	for i := range s.shards {
		s.shards[i].m = make(map[uint64]struct{})
	}
	return s
}

// shard espalha os IDs (que costumam ser sequenciais) entre os shards.
func (s *IDSet) shard(id uint64) int {
	return int((id * 0x9E3779B97F4A7C15) >> 58) // 6 bits = 64 shards
}

// Register marca o ID como usado. Retorna false se ele já existia.
func (s *IDSet) Register(id uint64) bool {
	sh := &s.shards[s.shard(id)]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if _, dup := sh.m[id]; dup {
		return false
	}
	sh.m[id] = struct{}{}
	return true
}

// Forget libera um ID registrado (usado quando a ordem nunca entrou no anel).
func (s *IDSet) Forget(id uint64) {
	sh := &s.shards[s.shard(id)]
	sh.mu.Lock()
	delete(sh.m, id)
	sh.mu.Unlock()
}

// Len devolve quantos IDs estão registrados.
func (s *IDSet) Len() int {
	n := 0
	for i := range s.shards {
		s.shards[i].mu.Lock()
		n += len(s.shards[i].m)
		s.shards[i].mu.Unlock()
	}
	return n
}
