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

	wal, err := storage.NewWAL("goruptor_jornal.jsonl")
	if err != nil {
		log.Fatalf("Falha ao iniciar o WAL: %v", err)
	}
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
	ringBuffer := disruptor.NewRingBuffer(1024)

	fmt.Println("⏳ Lendo Write-Ahead Log para restaurar memória...")
	pastEvents, _ := wal.ReadAll()
	for _, evt := range pastEvents {
		// Re-processa tudo silenciosamente (sem avisar a nuvem de novo)
		// Aqui a gente joga pro orderbook cruzar de novo e montar o estado
		book.Process(matching.Order{
			ID:       evt.OrderID,
			Quantity: evt.Quantity,
			Price:    evt.Price,
			Side:     matching.Side(evt.Side),
		})
	}
	fmt.Printf("✅ Memória restaurada com %d ordens!\n", len(pastEvents))

	// LIGAMOS O MOTOR
	go ringBuffer.StartConsumer(engineHandler)

	fmt.Println("✅ Motor LMAX Disruptor rodando. Aguardando ordens...\n--- 🔔 PREGÃO ABERTO ---")

	server := api.NewServer(ringBuffer, book, wal)

	err = server.Start(":3000")
	if err != nil {
		return
	}
}
