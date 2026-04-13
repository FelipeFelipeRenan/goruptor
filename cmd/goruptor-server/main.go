package main

import (
	"fmt"
	"time"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
	"github.com/FelipeFelipeRenan/goruptor/internal/matching"
)

func main() {
	fmt.Println("🚀 Iniciando a Corretora Pulse...")

	// 1. Instanciamos o Motor de Matching para o par BTC/USD (Asset 1)
	book := matching.NewOrderBook(1)

	// 2. Instanciamos o Adaptador do Motor
	engineHandler := matching.NewEngineHandler(book)

	// 3. Instanciamos o Chassi Lock-Free (Tambor de 1024 posições)
	ringBuffer := disruptor.NewRingBuffer(1024)

	// 4. LIGAMOS O MOTOR (O Consumidor começa a girar em background)
	go ringBuffer.StartConsumer(engineHandler)

	fmt.Println("✅ Motor LMAX Disruptor rodando. Aguardando ordens...")
	time.Sleep(1 * time.Second) // Pausa dramática

	// 5. SIMULANDO O GATEWAY DE REDE (O Produtor)
	// Vamos jogar aquelas mesmas ordens do nosso teste, mas agora
	// passando pelo funil de alta performance!

	fmt.Println("\n--- 🔔 PREGÃO ABERTO ---")

	// Vendedor 1 chega
	ringBuffer.Publish(disruptor.OrderEvent{
		OrderID: 1, AccountID: 100, Price: 65000, Quantity: 10, AssetID: 1, Side: disruptor.Sell,
	})

	// Vendedor 2 chega (mais caro)
	ringBuffer.Publish(disruptor.OrderEvent{
		OrderID: 2, AccountID: 101, Price: 65100, Quantity: 5, AssetID: 1, Side: disruptor.Sell,
	})

	time.Sleep(10 * time.Millisecond) // Dá tempo pro console imprimir na ordem certa

	// O Predador ataca! (Você comprando)
	ringBuffer.Publish(disruptor.OrderEvent{
		OrderID: 3, AccountID: 999, Price: 65000, Quantity: 12, AssetID: 1, Side: disruptor.Buy,
	})

	// Seguramos o programa aberto por 1 segundo para vermos os prints
	time.Sleep(1 * time.Second)
	fmt.Println("--- 🛑 PREGÃO ENCERRADO ---")
}
