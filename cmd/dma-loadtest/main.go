// Comando dma-loadtest: cliente de carga do Direct Market Access (TCP binário,
// porta 3001 do goruptor-server).
//
// Cada ordem é um frame de 25 bytes (big-endian):
//
//	[0:8]   OrderID  uint64
//	[8:16]  Price    uint64
//	[16:24] Quantity uint64
//	[24]    Side     uint8   (0 = BUY, 1 = SELL)
//
// O servidor responde 1 byte por ordem: 1 = confirmada (já está no WAL em disco),
// 2 = inválida, 3 = order_id repetido, 0 = não confirmada. Este cliente envia em
// pipeline, lê os ACKs em paralelo e mede ordens CONFIRMADAS por segundo (não
// apenas a velocidade de escrita no socket).
//
// Uso:
//
//	go run ./cmd/dma-loadtest -n 100000 -c 16
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

const (
	frameSize = 25
	ackOK     = 1
)

type result struct {
	confirmed, rejected uint64
	err                 error
}

func main() {
	addr := flag.String("addr", "127.0.0.1:3001", "endereço do servidor TCP")
	total := flag.Int("n", 100_000, "total de ordens a enviar")
	conns := flag.Int("c", 1, "conexões simultâneas")
	idBase := flag.Uint64("id-base", uint64(time.Now().Unix())*1_000_000,
		"primeiro OrderID (padrão: derivado do relógio, para não repetir IDs entre execuções)")
	timeout := flag.Duration("timeout", 2*time.Minute, "tempo máximo da execução")
	flag.Parse()

	if *total <= 0 || *conns <= 0 || *conns > *total {
		log.Fatal("use -n > 0, -c > 0 e -c <= -n")
	}

	log.Printf("🎯 %d ordens em %d conexão(ões) para %s", *total, *conns, *addr)

	results := make([]result, *conns)
	var wg sync.WaitGroup
	start := time.Now()

	next := *idBase
	for c := 0; c < *conns; c++ {
		n := *total / *conns
		if c < *total%*conns {
			n++
		}
		wg.Add(1)
		go func(c int, first uint64, n int) {
			defer wg.Done()
			results[c] = runConn(*addr, first, n, *timeout)
		}(c, next, n)
		next += uint64(n)
	}
	wg.Wait()
	elapsed := time.Since(start)

	var confirmed, rejected uint64
	failed := false
	for c, r := range results {
		confirmed += r.confirmed
		rejected += r.rejected
		if r.err != nil {
			failed = true
			log.Printf("❌ conexão %d: %v", c, r.err)
		}
	}

	fmt.Printf("✅ confirmadas: %d | recusadas: %d | tempo: %v\n", confirmed, rejected, elapsed.Round(time.Millisecond))
	fmt.Printf("🚀 %.0f ordens confirmadas/s\n", float64(confirmed)/elapsed.Seconds())
	if failed || rejected > 0 || confirmed != uint64(*total) {
		os.Exit(1)
	}
}

// runConn envia n ordens em pipeline e conta os ACKs recebidos.
func runConn(addr string, firstID uint64, n int, timeout time.Duration) result {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return result{err: err}
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	var res result
	done := make(chan struct{})

	// Leitor: um byte de ACK por ordem enviada.
	go func() {
		defer close(done)
		br := bufio.NewReader(conn)
		for i := 0; i < n; i++ {
			b, err := br.ReadByte()
			if err != nil {
				if err != io.EOF {
					res.err = fmt.Errorf("lendo ACK %d/%d: %w", i, n, err)
				} else {
					res.err = fmt.Errorf("servidor fechou a conexão após %d/%d ACKs", i, n)
				}
				return
			}
			if b == ackOK {
				res.confirmed++
			} else {
				res.rejected++
			}
		}
	}()

	// Escritor: frames em pipeline. Preços numa faixa estreita e lados
	// alternados, para o motor realmente casar ordens.
	bw := bufio.NewWriterSize(conn, 64*1024)
	frame := make([]byte, frameSize)
	for i := 0; i < n; i++ {
		binary.BigEndian.PutUint64(frame[0:8], firstID+uint64(i))
		binary.BigEndian.PutUint64(frame[8:16], 1000+uint64(i%64))
		binary.BigEndian.PutUint64(frame[16:24], 1)
		frame[24] = byte(i % 2)
		if _, err := bw.Write(frame); err != nil {
			conn.Close() // destrava o leitor
			<-done
			if res.err == nil {
				res.err = fmt.Errorf("escrevendo ordem %d/%d: %w", i, n, err)
			}
			return res
		}
	}
	if err := bw.Flush(); err != nil {
		conn.Close()
		<-done
		if res.err == nil {
			res.err = err
		}
		return res
	}

	<-done
	return res
}
