package api

import (
	"encoding/binary"
	"io"
	"log"
	"net"
	"strings"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
	"github.com/FelipeFelipeRenan/goruptor/internal/storage"
)

type TCPServer struct {
	ringBuffer *disruptor.RingBuffer
	wal        *storage.WAL
	batcher    *storage.Batcher
}

func NewTCPServer(rb *disruptor.RingBuffer, wal *storage.WAL, batcher *storage.Batcher) *TCPServer {
	return &TCPServer{
		ringBuffer: rb,
		wal:        wal,
		batcher:    batcher,
	}
}

func (s *TCPServer) Start(port string) error {
	listener, err := net.Listen("tcp", port)
	if err != nil {
		return err
	}
	log.Printf("⚡ DMA (Direct Market Access) Lane rodando puro em TCP Binário na porta %s...\n", port)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Erro TCP: %v\n", err)
			continue
		}
		go s.handleConnection(conn)
	}
}

func (s *TCPServer) handleConnection(conn net.Conn) {
	defer conn.Close()


	buf := make([]byte, 25)
	
	// Pré-alocamos o byte de resposta (ACK) para evitar alocação no loop
	ack := []byte{1}

	for {
		// 2. Leitura Binária Bloqueante
		// Só avança quando tiver pescado exatamente 25 bytes da placa de rede.
		_, err := io.ReadFull(conn, buf)
		if err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "connection reset") {
				log.Printf("❌ Fim da conexão ou erro na leitura do socket: %v\n", err)
			}
			return
		}

		// 3. Deserialização via Bit Shifting (Extremamente rápido)
		event := disruptor.OrderEvent{
			OrderID:  binary.BigEndian.Uint64(buf[0:8]),
			Price:    binary.BigEndian.Uint64(buf[8:16]),
			Quantity: binary.BigEndian.Uint64(buf[16:24]),
			Side:     disruptor.Side(buf[24]),
		}

		// 4. Despacho Lock-Free para o Motor
		s.wal.Log(event)
		s.ringBuffer.Publish(event)
		s.batcher.Push(event)

		// 5. ACK Binário
		// Enviamos 1 único byte de volta para o Cannon confirmar o sucesso
		_, err = conn.Write(ack)
		if err != nil {
			return
		}
	}
}