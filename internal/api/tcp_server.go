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

	reader := bufio.NewReader(conn)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF && !strings.Contains(err.Error(), "connection reset") {
				log.Printf("❌ Erro de leitura crítica no socket: %v\n", err)
			}
			return // Encerra a goroutine graciosamente quando o cliente desconectar
		}

		// 1. Sanetização radical da string: remove espaços, quebras reais e literais (\\n)
		line = strings.TrimSpace(line)
		line = strings.ReplaceAll(line, "\\n", "")

		if line == "" {
			continue
		}

		parts := strings.Split(line, ",")
		if len(parts) != 4 {
			log.Printf("⚠️ Pacote malformado rejeitado: %q\n", line)
			continue
		}

		// 2. Mapeamento seguro do Side
		side := disruptor.Buy
		if parts[0] == "S" {
			side = disruptor.Sell
		}

		// 3. Parsing com tratamento de erro explícito (Fim do '_' oculto)
		orderID, errID := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
		price, errPrice := strconv.ParseUint(strings.TrimSpace(parts[2]), 10, 64)
		quantity, errQty := strconv.ParseUint(strings.TrimSpace(parts[3]), 10, 64)

		// Se qualquer um falhar, o pacote é descartado com aviso no log
		if errID != nil || errPrice != nil || errQty != nil {
			log.Printf("⚠️ Erro ao converter dados numéricos na linha: %q (ID_err: %v, Price_err: %v, Qty_err: %v)\n", 
				line, errID, errPrice, errQty)
			continue
		}

		// 4. Montagem e despacho para o ecossistema LMAX
		event := disruptor.OrderEvent{
			OrderID:  orderID,
			Price:    price,
			Quantity: quantity,
			Side:     side,
		}

		s.wal.Log(event)
		s.ringBuffer.Publish(event)
		s.batcher.Push(event)

		// Confirmação para o Cannon computar o sucesso
		_, err = conn.Write([]byte("1\n"))
		if err != nil {
			return
		}
	}
}