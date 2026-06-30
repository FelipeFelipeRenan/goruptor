package api

import (
	"bufio"
	"io"
	"log"
	"net"
	"strconv"
	"strings"

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

	log.Println("👀 Nova conexão TCP recebida do Cannon!") // LOG 1

	// 1. Instancia o leitor com buffer para lidar com as quebras de linha
	reader := bufio.NewReader(conn)

	for {
		// 2. Lê a string até encontrar o delimitador (Enter/Quebra de linha)
		line, err := reader.ReadString('\n')
		log.Printf("📦 Lemos da rede: %q (Erro: %v)\n", line, err) // LOG 2
		if err != nil {
			if err != io.EOF {
				log.Printf("Erro na leitura TCP: %v\n", err)
			}
			return // Sai quando o Cannon fechar a conexão
		}

		// 3. Limpa espaços e quebras de linha e divide pela vírgula
		line = strings.TrimSpace(line)
		parts := strings.Split(line, ",")

		// Prevenção de pânico se o Cannon enviar uma linha em branco ou cortada
		if len(parts) != 4 {
			continue
		}

		// 4. Mapeia o lado da ordem (B = Buy, S = Sell)
		side := disruptor.Buy
		if parts[0] == "S" {
			side = disruptor.Sell
		}

		// 5. Converte as strings para uint64 (Base 10, 64 bits)
		orderID, _ := strconv.ParseUint(parts[1], 10, 64)
		price, _ := strconv.ParseUint(parts[2], 10, 64)
		quantity, _ := strconv.ParseUint(parts[3], 10, 64)

		// 6. Monta o evento e joga pro motor
		event := disruptor.OrderEvent{
			OrderID:  orderID,
			Price:    price,
			Quantity: quantity,
			Side:     side,
		}

		s.wal.Log(event)
		s.ringBuffer.Publish(event)
		s.batcher.Push(event)

		_, err = conn.Write([]byte("1\n"))
		if err != nil {
			return // Se o Cannon fechou a porta na nossa cara, morremos graciosamente
		}
	}
}
