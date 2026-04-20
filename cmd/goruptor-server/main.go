package main

import (
	"fmt"
	"log"

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
	ringBuffer := disruptor.NewRingBuffer(1024) // 2. INICIA O WRITE-AHEAD LOG (WAL)
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
	rdsConnStr := "postgres://goruptor:admin123@localhost:4510/exchange?sslmode=disable"

	// 2. Instancia o Trabalhador que vai fazer o Bulk Insert de 1000 em 1000 ordens
	batcher, err := storage.NewBatcher(rdsConnStr, 1000)
	if err != nil {
		log.Fatalf("Falha ao conectar no RDS da AWS: %v", err)
	}
	// LIGAMOS O MOTOR
	go ringBuffer.StartConsumer(engineHandler)

	fmt.Println("✅ Motor LMAX Disruptor rodando. Aguardando ordens...\n--- 🔔 PREGÃO ABERTO ---")

	server := api.NewServer(ringBuffer, book, wal, batcher)

	err = server.Start(":3000")
	if err != nil {
		return
	}
}
