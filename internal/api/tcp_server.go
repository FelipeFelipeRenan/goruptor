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

// Respostas de 1 byte do protocolo TCP binário.
const (
	AckRejected byte = 0 // ordem NÃO confirmada (anel fechado ou journal falhou)
	AckOK       byte = 1 // ordem confirmada: já está no journal
)

type TCPServer struct {
	ringBuffer orderPublisher
	batcher    orderArchiver
}

func NewTCPServer(rb *disruptor.RingBuffer, batcher *storage.Batcher) *TCPServer {
	return &TCPServer{
		ringBuffer: rb,
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

	// Pré-alocamos os bytes de resposta (ACK) para evitar alocação no loop
	ackOK := []byte{AckOK}
	ackRejected := []byte{AckRejected}

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

		// 4. Despacho para o Motor + espera do journal (group commit).
		// Muitas conexões esperando ao mesmo tempo dividem um único fsync.
		if err := acceptOrder(s.ringBuffer, s.batcher, event); err != nil {
			log.Printf("❌ ordem %d não confirmada: %v", event.OrderID, err)
			conn.Write(ackRejected)
			return
		}

		// 5. ACK Binário
		// 1 único byte: AckOK só é enviado depois que a ordem está no journal.
		if _, err = conn.Write(ackOK); err != nil {
			return
		}
	}
}
