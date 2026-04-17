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

	rdsConnStr := "postgres://goruptor:admin123@localhost:4566/exchange?sslmode=disable"

	batcher, err := storage.NewBatcher(rdsConnStr, 1000)
	if err != nil {
		log.Fatalf("Falha ao conectar no RDS da AWS: %v", err)
	}
	// 3. Instancia o Motor passando o AWS Publisher
	book := matching.NewOrderBook(1, awsPub)
	engineHandler := matching.NewEngineHandler(book)
	ringBuffer := disruptor.NewRingBuffer(1024)

	// LIGAMOS O MOTOR
	go ringBuffer.StartConsumer(engineHandler)

	fmt.Println("✅ Motor LMAX Disruptor rodando. Aguardando ordens...\n--- 🔔 PREGÃO ABERTO ---")

	server := api.NewServer(ringBuffer, book, batcher)

	err = server.Start(":3000")
	if err != nil {
		return
	}
}
