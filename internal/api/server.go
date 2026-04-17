package api

import (
	"log"
	"sync"
	"time"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
	"github.com/FelipeFelipeRenan/goruptor/internal/matching"
	"github.com/FelipeFelipeRenan/goruptor/internal/storage"
	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
)

type OrderRequest struct {
	OrderID  uint64 `json:"order_id"`
	Price    uint64 `json:"price"`
	Quantity uint64 `json:"quantity"`
	Side     string `json:"side"`
}

type Server struct {
	app        *fiber.App
	ringBuffer *disruptor.RingBuffer
	orderBook  *matching.OrderBook
	batcher    *storage.Batcher
	wal        *storage.WAL

	clients map[*websocket.Conn]bool
	mu      sync.Mutex
}

func NewServer(rb *disruptor.RingBuffer, ob *matching.OrderBook, wal *storage.WAL, batcher *storage.Batcher) *Server {
	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
	})

	server := &Server{
		app:        app,
		ringBuffer: rb,
		orderBook:  ob,
		batcher:    batcher,
		wal:        wal,
		clients:    make(map[*websocket.Conn]bool),
	}

	app.Post("/api/orders", server.handleCreateOrder)

	app.Use("/ws", func(ctx *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(ctx) {
			ctx.Locals("allowed", true)
			return ctx.Next()
		}
		return fiber.ErrUpgradeRequired
	})

	app.Get("/ws", websocket.New(server.handleWS))

	go server.broadcastTicker()
	return server
}

func (s *Server) Start(port string) error {
	log.Printf("🌐 API da Goruptor rodando na porta %s...\n", port)
	err := s.app.Listen(port)
	if err != nil {
		return err
	}
	return nil
}

func (s *Server) handleCreateOrder(ctx *fiber.Ctx) error {
	var req OrderRequest
	if err := ctx.BodyParser(&req); err != nil {
		return ctx.Status(400).JSON(fiber.Map{"error": "JSON inválido"})
	}

	var side disruptor.Side

	if req.Side == "SELL" {
		side = disruptor.Sell
	} else {
		side = disruptor.Buy
	}

	event := disruptor.OrderEvent{
		OrderID: req.OrderID, Price: req.Price, Quantity: req.Quantity, Side: side,
	}
	s.wal.Log(event)

	s.ringBuffer.Publish(event)

	return ctx.Status(202).JSON(fiber.Map{
		"message":  "Ordem recebida e enviada para o motor",
		"order_id": req.OrderID,
	})
}

func (s *Server) handleWS(ctx *websocket.Conn) {
	s.mu.Lock()
	s.clients[ctx] = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.clients, ctx)
		s.mu.Unlock()
		err := ctx.Close()
		if err != nil {
			return
		}
	}()

	for {
		if _, _, err := ctx.ReadMessage(); err != nil {
			break
		}
	}
}

func (s *Server) broadcastTicker() {
	ticker := time.NewTicker(200 * time.Millisecond)
	for range ticker.C {
		snap := s.orderBook.GetSnapshot()

		s.mu.Lock()
		for client := range s.clients {
			if err := client.WriteJSON(snap); err != nil {
				err := client.Close()
				if err != nil {
					return
				}
				delete(s.clients, client)
			}
		}
		s.mu.Unlock()
	}
}
