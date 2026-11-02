package api

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
	"github.com/FelipeFelipeRenan/goruptor/internal/ingest"
)

// Respostas de 1 byte do protocolo TCP binário (uma por ordem recebida).
const (
	// AckRejected: ordem NÃO confirmada por culpa do servidor (desligando ou
	// disco com falha). A conexão é encerrada em seguida.
	AckRejected byte = 0

	// AckOK: ordem confirmada, já está no journal em disco.
	AckOK byte = 1

	// AckInvalid: ordem malformada (side, preço, quantidade ou order_id inválidos).
	// A conexão continua aberta; o próximo frame é lido normalmente.
	AckInvalid byte = 2

	// AckDuplicate: o order_id já foi utilizado. A conexão continua aberta.
	AckDuplicate byte = 3
)

const frameSize = 25

type TCPServer struct {
	ingress *ingest.Ingress

	mu      sync.Mutex
	ln      net.Listener
	conns   map[net.Conn]struct{}
	closing bool
	wg      sync.WaitGroup // uma entrada por conexão ativa
}

func NewTCPServer(in *ingest.Ingress) *TCPServer {
	return &TCPServer{
		ingress: in,
		conns:   make(map[net.Conn]struct{}),
	}
}

// Start é Listen + Serve. Retorna nil depois de um Shutdown gracioso.
func (s *TCPServer) Start(addr string) error {
	if err := s.Listen(addr); err != nil {
		return err
	}
	return s.Serve()
}

// Listen abre a porta. Separado de Serve para que quem chama possa descobrir o
// endereço real (Addr) quando usa a porta 0.
func (s *TCPServer) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	log.Printf("⚡ DMA (Direct Market Access) Lane rodando puro em TCP Binário em %s...\n", ln.Addr())
	return nil
}

// Addr devolve o endereço em que o servidor escuta (nil antes de Listen).
func (s *TCPServer) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Serve aceita conexões até Shutdown. Retorna nil após um Shutdown.
func (s *TCPServer) Serve() error {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln == nil {
		return errors.New("tcp: Serve chamado antes de Listen")
	}

	backoff := time.Duration(0)
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closing := s.closing
			s.mu.Unlock()
			if closing || errors.Is(err, net.ErrClosed) {
				return nil
			}
			// Erro transitório (ex.: sem descritores de arquivo): recua em vez de girar a 100% de CPU.
			if backoff == 0 {
				backoff = 5 * time.Millisecond
			} else if backoff < time.Second {
				backoff *= 2
			}
			log.Printf("Erro TCP no Accept (nova tentativa em %v): %v\n", backoff, err)
			time.Sleep(backoff)
			continue
		}
		backoff = 0

		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			conn.Close()
			continue
		}
		s.conns[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()

		go s.handleConnection(conn)
	}
}

// Shutdown para de aceitar conexões, acorda as conexões ociosas e espera as
// ordens em andamento terminarem (ou ctx expirar; nesse caso as conexões
// restantes são derrubadas).
//
// Uma ordem só é confirmada com AckOK depois de estar em disco. Frames que o
// servidor ainda não leu quando o Shutdown começa NÃO recebem ACK; o cliente
// sabe que eles não foram aceitos e pode reenviá-los depois.
func (s *TCPServer) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	if s.ln != nil {
		s.ln.Close()
	}
	for c := range s.conns {
		// Destrava leituras bloqueadas em conexões ociosas. Uma ordem que já
		// foi lida segue seu caminho normal (o deadline só afeta Read).
		c.SetReadDeadline(time.Now())
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
		return ctx.Err()
	}
}

func (s *TCPServer) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}

func (s *TCPServer) handleConnection(conn net.Conn) {
	defer func() {
		conn.Close()
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		s.wg.Done()
	}()

	buf := make([]byte, frameSize)

	// Pré-alocamos os bytes de resposta (ACK) para evitar alocação no loop
	acks := [4][]byte{{AckRejected}, {AckOK}, {AckInvalid}, {AckDuplicate}}

	for {
		// Leitura binária bloqueante: só avança com exatamente 25 bytes.
		if _, err := io.ReadFull(conn, buf); err != nil {
			if !s.isClosing() && err != io.EOF && !errors.Is(err, net.ErrClosed) && !errors.Is(err, syscall.ECONNRESET) {
				log.Printf("❌ Fim da conexão ou erro na leitura do socket: %v\n", err)
			}
			return
		}

		// Deserialização via bit shifting.
		event := disruptor.OrderEvent{
			OrderID:  binary.BigEndian.Uint64(buf[0:8]),
			Price:    binary.BigEndian.Uint64(buf[8:16]),
			Quantity: binary.BigEndian.Uint64(buf[16:24]),
			Side:     disruptor.Side(buf[24]),
		}

		// Valida, deduplica, publica e espera o journal (group commit: muitas
		// conexões esperando ao mesmo tempo dividem um único fsync).
		var ack byte
		switch err := s.ingress.Submit(event); {
		case err == nil:
			ack = AckOK
		case errors.Is(err, ingest.ErrInvalidOrder):
			ack = AckInvalid
		case errors.Is(err, ingest.ErrDuplicateOrder):
			ack = AckDuplicate
		default:
			log.Printf("❌ ordem %d não confirmada: %v", event.OrderID, err)
			conn.Write(acks[AckRejected])
			return
		}

		if _, err := conn.Write(acks[ack]); err != nil {
			return
		}
	}
}
