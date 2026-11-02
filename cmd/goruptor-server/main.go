package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/FelipeFelipeRenan/goruptor/internal/api"
	"github.com/FelipeFelipeRenan/goruptor/internal/app"
	"github.com/FelipeFelipeRenan/goruptor/internal/cloud"
	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
	"github.com/FelipeFelipeRenan/goruptor/internal/ingest"
	"github.com/FelipeFelipeRenan/goruptor/internal/matching"
	"github.com/FelipeFelipeRenan/goruptor/internal/storage"
)

// shutdownTimeout é o tempo máximo para drenar tudo no SIGINT/SIGTERM.
const shutdownTimeout = 15 * time.Second

func main() {
	fmt.Println("🚀 Iniciando a Corretora Goruptor...")

	// 1. Conecta com a AWS (MiniStack)
	awsPub, err := cloud.NewAWSPublisher()
	if err != nil {
		log.Fatalf("Falha ao conectar na AWS: %v", err)
	}

	// 2. Liga o Worker Assíncrono em background
	tradesDone := make(chan struct{})
	go func() {
		awsPub.Publish() // retorna quando awsPub.Close() esvazia o canal de trades
		close(tradesDone)
	}()
	fmt.Println("☁️  Worker AWS Conectado no SQS local.")

	// 3. Instancia o Motor passando o AWS Publisher
	book := matching.NewOrderBook(1, awsPub)
	engineHandler := matching.NewEngineHandler(book)

	// 2. INICIA O WRITE-AHEAD LOG (WAL)
	wal, err := storage.NewWAL("goruptor_journal.jsonl")
	if err != nil {
		log.Fatalf("Falha ao iniciar o WAL: %v", err)
	}

	// O WAL é o journal do anel: um estágio dedicado lê o anel em ordem de
	// sequência, grava em lote com fsync e só então libera o motor e o ACK.
	journalOpts := journalOptionsFromEnv()
	ringBuffer := disruptor.NewJournaledRingBuffer(131072, wal, journalOpts)

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

	// Porta única de entrada de ordens (HTTP e TCP): valida, deduplica e confirma.
	// Os IDs já presentes no WAL são registrados, para que um reinício não reabra
	// a porta para ordens duplicadas.
	ingress := ingest.New(ringBuffer, batcher)
	for _, evt := range pastEvents {
		ingress.IDs().Register(evt.OrderID)
	}

	// LIGAMOS O JOURNALER (só depois do replay: ele começa a gravar eventos novos)
	go ringBuffer.StartJournaler()
	// LIGAMOS O MOTOR
	go ringBuffer.StartConsumer(engineHandler)

	fmt.Println("✅ Motor LMAX Disruptor rodando. Aguardando ordens...\n--- 🔔 PREGÃO ABERTO ---")
	httpServer := api.NewServer(ingress, book, batcher.DB)
	go func() {
		if err := httpServer.Start(":3000"); err != nil {
			log.Fatalf("Erro no servidor HTTP: %v", err)
		}
	}()

	tcpServer := api.NewTCPServer(ingress)
	go func() {
		if err := tcpServer.Start(":3001"); err != nil {
			log.Fatalf("Erro no servidor TCP: %v", err)
		}
	}()

	// SIGINT (Ctrl+C) ou SIGTERM (docker stop) cancelam este contexto.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	fmt.Println("🟢 Servidores a rodar em background. Pressione Ctrl+C para sair.")
	<-sigCtx.Done()
	stopSignals() // um segundo Ctrl+C durante o desligamento encerra na hora

	fmt.Println("\n🛑 Sinal recebido! Desligando a Goruptor (drenando ordens em andamento)...")
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	components := &app.Components{
		HTTP:        httpServer,
		TCP:         tcpServer,
		Ring:        ringBuffer,
		WAL:         wal,
		Batcher:     batcher,
		CloseTrades: awsPub.Close,
		TradesDone:  tradesDone,
	}
	if err := components.Shutdown(ctx); err != nil {
		log.Printf("Desligamento terminou com erros: %v", err)
		os.Exit(1)
	}
	fmt.Println("👋 Desligamento completo: nenhuma ordem confirmada foi perdida.")
}

// journalOptionsFromEnv lê GORUPTOR_WAL_SYNC:
//
//	"always" (padrão) ou vazio -> fsync a cada lote; ACK == ordem em disco.
//	uma duração ("200ms", "1s") -> fsync no máximo a cada intervalo; ACK após
//	                               o write no SO. Mais rápido, mas uma queda de
//	                               energia pode perder até esse intervalo.
func journalOptionsFromEnv() disruptor.JournalOptions {
	v := os.Getenv("GORUPTOR_WAL_SYNC")
	if v == "" || v == "always" {
		fmt.Println("💾 WAL: fsync a cada lote (ACK só depois da ordem em disco)")
		return disruptor.JournalOptions{}
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Fatalf("GORUPTOR_WAL_SYNC inválido (%q): use \"always\" ou uma duração como \"200ms\"", v)
	}
	fmt.Printf("⚠️  WAL: fsync a cada %v (queda de energia pode perder até esse intervalo)\n", d)
	return disruptor.JournalOptions{SyncInterval: d}
}
