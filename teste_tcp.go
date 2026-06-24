package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"time"
)

func main() {
	log.Println("🎯 Carregando o Sniper DMA (Direct Market Access)...")

	conn, err := net.Dial("tcp", "localhost:3001")
	if err != nil {
		log.Fatalf("❌ Falha ao conectar na porta 3001: %v", err)
	}
	defer conn.Close()

	packet := make([]byte, 25)
	binary.BigEndian.PutUint64(packet[8:16], 50000) // Price
	binary.BigEndian.PutUint64(packet[16:24], 1)    // Quantity
	packet[24] = 0                                  // 0 = BUY, 1 = SELL

	const numOrders = 100000
	log.Printf("🔥 Disparando %d ordens binárias sem cabeçalhos HTTP...", numOrders)

	start := time.Now()

	for i := uint64(1); i <= numOrders; i++ {
		binary.BigEndian.PutUint64(packet[0:8], i) // Atualiza só o OrderID
		
		_, err := conn.Write(packet)
		if err != nil {
			log.Fatalf("Erro ao enviar no disparo %d: %v", i, err)
		}
	}

	elapsed := time.Since(start)
	rps := float64(numOrders) / elapsed.Seconds()

	fmt.Printf("✅ Ataque concluído no OS em %v!\n", elapsed)
	fmt.Printf("🚀 Velocidade de injeção local: %.2f RPS\n", rps)

	// A CURA: Mantemos a conexão TCP aberta à força para a corretora conseguir ler tudo!
	fmt.Println("⏳ Aguardando a Corretora drenar o buffer de rede...")
	time.Sleep(3 * time.Second)
	fmt.Println("👋 Sniper desligado. Conexão fechada com segurança.")
}