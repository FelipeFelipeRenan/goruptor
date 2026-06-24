package api

import (
	"encoding/binary"
	"io"
	"log"
	"net"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
	"github.com/FelipeFelipeRenan/goruptor/internal/storage"
)

const packetSize = 25

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
	log.Printf("⚡ DMA (Direct Market Access) Lane rodando puro em TCP na porta %s...\n", port)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Erro TCP: %v\n", err)
			continue
		}
		// Cada cliente que conecta (ex: workers do seu Cannon) ganha uma goroutine ultra-leve
		go s.handleConnection(conn)
	}
}

func (s *TCPServer) handleConnection(conn net.Conn) {
    defer conn.Close()
    
    // Buffer local para evitar alocação por cada pacote
    buf := make([]byte, 25)

    for {
        // io.ReadFull é bloqueante. Ele só retorna se ler 25 bytes ou der erro.
        _, err := io.ReadFull(conn, buf)
        if err != nil {
            if err != io.EOF {
                log.Printf("❌ Erro na leitura do socket DMA: %v", err)
            }
            return // Sai da goroutine quando a conexão for fechada pelo Sniper
        }

        // Conversão zero-copy (na stack)
        event := disruptor.OrderEvent{
            OrderID:  binary.BigEndian.Uint64(buf[0:8]),
            Price:    binary.BigEndian.Uint64(buf[8:16]),
            Quantity: binary.BigEndian.Uint64(buf[16:24]),
            Side:     disruptor.Side(buf[24]),
        }

        // Envia para o motor
        s.wal.Log(event)
        s.ringBuffer.Publish(event)
        s.batcher.Push(event)
    }
}