package main

import (
	"fmt"
	"log"
	"time"

	"github.com/FelipeFelipeRenan/goruptor/internal/cloud"
	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
	"github.com/FelipeFelipeRenan/goruptor/internal/matching"
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
	ringBuffer := disruptor.NewRingBuffer(1024)

	// LIGAMOS O MOTOR
	go ringBuffer.StartConsumer(engineHandler)

	fmt.Println("✅ Motor LMAX Disruptor rodando. Aguardando ordens...\n--- 🔔 PREGÃO ABERTO ---")

	// Mandando as ordens
	ringBuffer.Publish(disruptor.OrderEvent{OrderID: 1, Price: 65000, Quantity: 10, Side: disruptor.Sell})
	ringBuffer.Publish(disruptor.OrderEvent{OrderID: 2, Price: 65100, Quantity: 5, Side: disruptor.Sell})
	time.Sleep(10 * time.Millisecond)
	ringBuffer.Publish(disruptor.OrderEvent{OrderID: 3, Price: 65000, Quantity: 12, Side: disruptor.Buy})

	time.Sleep(1 * time.Second)
	fmt.Println("--- 🛑 PREGÃO ENCERRADO ---")
}
