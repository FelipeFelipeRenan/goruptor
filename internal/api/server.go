package api

import (
	"log"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
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
}

func NewServer(rb *disruptor.RingBuffer) *Server {
	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
	})

	server := &Server{
		app:        app,
		ringBuffer: rb,
	}

	app.Post("/api/orders", server.handleCreateOrder)
	return server
}

func (s Server) Start(port string) error {
	log.Printf("🌐 API da Goruptor rodando na porta %s...\n", port)
	err := s.app.Listen(port)
	if err != nil {
		return err
	}
	return nil
}

func (s Server) handleCreateOrder(ctx *fiber.Ctx) error {
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

	s.ringBuffer.Publish(disruptor.OrderEvent{
		OrderID:  req.OrderID,
		Price:    req.Price,
		Quantity: req.Quantity,
		Side:     side,
	})

	return ctx.Status(202).JSON(fiber.Map{
		"message":  "Ordem recebida e enviada para o motor",
		"order_id": req.OrderID,
	})
}
