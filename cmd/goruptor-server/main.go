package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/FelipeFelipeRenan/goruptor/internal/api"
	"github.com/FelipeFelipeRenan/goruptor/internal/cloud"
	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
	"github.com/FelipeFelipeRenan/goruptor/internal/matching"
	"github.com/FelipeFelipeRenan/goruptor/internal/storage"
)

func main() {
	fmt.Println("🚀 Iniciando a Corretora Goruptor...")

	// 1. Conecta com a AWS (MiniStack)
	awsPub, err := cloud.NewAWSPublisher()
	if err != nil {
		log.Fatalf("Falha ao conectar na AWS: %v", err)
	}

	// 2. Liga o Worker Assíncrono em background
	go awsPub.Publish()
	fmt.Println("☁️  Worker AWS Conectado no SQS local.")

	// 3. Instancia o Motor passando o AWS Publisher
	book := matching.NewOrderBook(1, awsPub)
	engineHandler := matching.NewEngineHandler(book)
	ringBuffer := disruptor.NewRingBuffer(131072) // 2. INICIA O WRITE-AHEAD LOG (WAL)
	wal, err := storage.NewWAL("goruptor_journal.jsonl")
	if err != nil {
		log.Fatalf("Falha ao iniciar o WAL: %v", err)
	}

	// --- ⏪ A MÁQUINA DO TEMPO (RECOVERY) ---
	fmt.Println("⏳ Lendo Write-Ahead Log para restaurar a memória...")
	pastEvents, err := wal.ReadAll()
	if err != nil {
		log.Fatalf("Erro ao ler o WAL: %v", err)
	}

	book.SetRestoreMode(true)
	for _, evt := range pastEvents {
		// Reprocessa silenciosamente no Cérebro (não vai pro SQS de novo)
		book.Process(matching.Order{
			ID:       evt.OrderID,
			Quantity: evt.Quantity,
			Price:    evt.Price,
			Side:     matching.Side(evt.Side),
		})
	}

	book.SetRestoreMode(false)
	fmt.Printf("✅ Memória restaurada com sucesso! (%d ordens recuperadas)\n", len(pastEvents))
	rdsConnStr := "postgres://goruptor:admin123@localhost:15432/exchange?sslmode=disable"

	// 2. Instancia o Trabalhador que vai fazer o Bulk Insert de 1000 em 1000 ordens
	batcher, err := storage.NewBatcher(rdsConnStr, 1000)
	if err != nil {
		log.Fatalf("Falha ao conectar no RDS da AWS: %v", err)
	}
	// LIGAMOS O MOTOR
	go ringBuffer.StartConsumer(engineHandler)

	fmt.Println("✅ Motor LMAX Disruptor rodando. Aguardando ordens...\n--- 🔔 PREGÃO ABERTO ---")
	httpServer := api.NewServer(ringBuffer, book, wal, batcher)
	go func() {
		if err := httpServer.Start(":3000"); err != nil {
			log.Fatalf("Erro no servidor HTTP: %v", err)
		}
	}()

	tcpServer := api.NewTCPServer(ringBuffer, wal, batcher)
	go func() {
		if err := tcpServer.Start(":3001"); err != nil {
			log.Fatalf("Erro no servidor TCP: %v", err)
		}
	}()

	// --- A FORMA CORRETA E NATIVA DE BLOQUEAR A MAIN ---

	// Cria o canal para receber os sinais do OS
	quit := make(chan os.Signal, 1)

	// Notifica o canal 'quit' caso receba um SIGINT (Ctrl+C) ou SIGTERM (Docker stop)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	fmt.Println("🟢 Servidores a rodar em background. Pressione Ctrl+C para sair.")

	// A main bloqueia aqui eternamente até que um sinal entre no canal
	<-quit

	fmt.Println("\n🛑 Sinal recebido! Desligando os motores da Goruptor...")
}
