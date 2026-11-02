package api

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
	"github.com/FelipeFelipeRenan/goruptor/internal/ingest"
	"github.com/FelipeFelipeRenan/goruptor/internal/matching"
	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/pprof"
)

type OrderRequest struct {
	OrderID  uint64 `json:"order_id"`
	Price    uint64 `json:"price"`
	Quantity uint64 `json:"quantity"`
	Side     string `json:"side"`
}

type Server struct {
	app       *fiber.App
	ingress   *ingest.Ingress
	orderBook *matching.OrderBook
	db        *sql.DB // usado só pelos candles

	clients map[*websocket.Conn]bool
	mu      sync.Mutex

	stop     chan struct{}
	stopOnce sync.Once
}

func NewServer(in *ingest.Ingress, ob *matching.OrderBook, db *sql.DB) *Server {
	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
	})

	app.Use(pprof.New())

	server := &Server{
		app:       app,
		ingress:   in,
		orderBook: ob,
		db:        db,
		clients:   make(map[*websocket.Conn]bool),
		stop:      make(chan struct{}),
	}

	app.Get("/", func(c *fiber.Ctx) error {
		return c.SendFile("./web/index.html")
	})

	app.Post("/api/orders", server.handleCreateOrder)

	app.Get("/api/candles", server.handleGetCandles)

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

// Start sobe o servidor HTTP. Retorna nil depois de um Shutdown gracioso.
func (s *Server) Start(port string) error {
	log.Printf("🌐 API da Goruptor rodando na porta %s...\n", port)
	return s.app.Listen(port)
}

// Shutdown para de aceitar conexões, encerra os WebSockets e espera as
// requisições em andamento terminarem (ou ctx expirar). As ordens que já
// estavam esperando o journal recebem a resposta normalmente.
func (s *Server) Shutdown(ctx context.Context) error {
	s.stopOnce.Do(func() { close(s.stop) })

	s.mu.Lock()
	for c := range s.clients {
		c.Close()
		delete(s.clients, c)
	}
	s.mu.Unlock()

	return s.app.ShutdownWithContext(ctx)
}

// httpError traduz o erro de Submit em status HTTP e mensagem para o cliente.
//
//	400  ordem inválida                 (erro do cliente; a mensagem diz o que corrigir)
//	409  order_id já utilizado          (erro do cliente)
//	503  ordem NÃO confirmada           (erro do servidor: desligando ou disco com falha)
func httpError(err error) (status int, message string) {
	switch {
	case errors.Is(err, ingest.ErrInvalidOrder):
		return 400, err.Error()
	case errors.Is(err, ingest.ErrDuplicateOrder):
		return 409, err.Error()
	default:
		return 503, "ordem não confirmada, tente novamente"
	}
}

func (s *Server) handleCreateOrder(ctx *fiber.Ctx) error {
	var req OrderRequest
	if err := ctx.BodyParser(&req); err != nil {
		return ctx.Status(400).JSON(fiber.Map{"error": "JSON inválido"})
	}

	side, err := ingest.ParseSide(req.Side)
	if err != nil {
		status, msg := httpError(err)
		return ctx.Status(status).JSON(fiber.Map{"error": msg})
	}

	// Valida, deduplica e só responde depois que a ordem está no journal (em disco).
	err = s.ingress.Submit(disruptor.OrderEvent{
		OrderID: req.OrderID, Price: req.Price, Quantity: req.Quantity, Side: side,
	})
	if err != nil {
		status, msg := httpError(err)
		if status >= 500 {
			log.Printf("❌ ordem %d não confirmada: %v", req.OrderID, err)
		}
		return ctx.Status(status).JSON(fiber.Map{"error": msg})
	}

	return ctx.Status(202).JSON(fiber.Map{
		"message":  "Ordem confirmada e enviada para o motor",
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
		ctx.Close()
	}()

	for {
		if _, _, err := ctx.ReadMessage(); err != nil {
			break
		}
	}
}

func (s *Server) broadcastTicker() {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
		}

		snap := s.orderBook.GetSnapshot()

		s.mu.Lock()
		for client := range s.clients {
			if err := client.WriteJSON(snap); err != nil {
				// Antes, uma falha em Close() encerrava o broadcaster para sempre.
				client.Close()
				delete(s.clients, client)
			}
		}
		s.mu.Unlock()
	}
}
