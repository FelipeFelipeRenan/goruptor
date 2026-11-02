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
// Também mede a latência de cada ordem (do envio ao ACK) e imprime min, mean e
// percentis. Sem -window as ordens saem em rajada e a latência inclui a fila no
// próprio cliente; com -window W, cada conexão mantém no máximo W ordens em voo e
// a latência passa a refletir o servidor (use para comparar versões).
//
// Uso:
//
//	go run ./cmd/dma-loadtest -n 100000 -c 16
//	go run ./cmd/dma-loadtest -n 100000 -c 16 -window 32
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
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	frameSize = 25
	ackOK     = 1
)

type result struct {
	confirmed, rejected uint64
	lat                 []time.Duration // latência de cada ACK recebido (envio -> ACK)
	err                 error
}

// latStats resume as latências em milissegundos.
type latStats struct {
	n                             int
	min, mean, p50, p95, p99, max float64
}

// t0 e nowNs: relógio MONOTÔNICO. O relógio de parede (UnixNano) pode andar para
// trás (ajuste de hora, WSL/VM) e produzia latências negativas.
var t0 = time.Now()

func nowNs() int64 { return int64(time.Since(t0)) }

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func summarize(lat []time.Duration) latStats {
	if len(lat) == 0 {
		return latStats{}
	}
	sorted := append([]time.Duration(nil), lat...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum time.Duration
	for _, d := range sorted {
		sum += d
	}
	pct := func(p float64) float64 { // percentil por posto mais próximo (ceil)
		idx := int(p*float64(len(sorted))+0.999999999) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return ms(sorted[idx])
	}
	return latStats{
		n: len(sorted), min: ms(sorted[0]), mean: ms(sum) / float64(len(sorted)),
		p50: pct(0.50), p95: pct(0.95), p99: pct(0.99), max: ms(sorted[len(sorted)-1]),
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:3001", "endereço do servidor TCP")
	total := flag.Int("n", 100_000, "total de ordens a enviar")
	conns := flag.Int("c", 1, "conexões simultâneas")
	idBase := flag.Uint64("id-base", uint64(time.Now().Unix())*1_000_000,
		"primeiro OrderID (padrão: derivado do relógio, para não repetir IDs entre execuções)")
	timeout := flag.Duration("timeout", 2*time.Minute, "tempo máximo da execução")
	window := flag.Int("window", 0, "máximo de ordens em voo por conexão (0 = sem limite, rajada)")
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
			results[c] = runConn(*addr, first, n, *timeout, *window)
		}(c, next, n)
		next += uint64(n)
	}
	wg.Wait()
	elapsed := time.Since(start)

	var confirmed, rejected uint64
	var allLat []time.Duration
	failed := false
	for c, r := range results {
		confirmed += r.confirmed
		rejected += r.rejected
		allLat = append(allLat, r.lat...)
		if r.err != nil {
			failed = true
			log.Printf("❌ conexão %d: %v", c, r.err)
		}
	}

	fmt.Printf("✅ confirmadas: %d | recusadas: %d | tempo: %v\n", confirmed, rejected, elapsed.Round(time.Millisecond))
	fmt.Printf("🚀 %.0f ordens confirmadas/s\n", float64(confirmed)/elapsed.Seconds())
	if st := summarize(allLat); st.n > 0 {
		note := "sem -window: inclui a fila no cliente"
		if *window > 0 {
			note = fmt.Sprintf("window=%d por conexão", *window)
		}
		fmt.Printf("⏱️ latência (ms): min=%.2f mean=%.2f p50=%.2f p95=%.2f p99=%.2f max=%.2f  (%s)\n",
			st.min, st.mean, st.p50, st.p95, st.p99, st.max, note)
	}
	if failed || rejected > 0 || confirmed != uint64(*total) {
		os.Exit(1)
	}
}

// runConn envia n ordens em pipeline e conta os ACKs recebidos.
func runConn(addr string, firstID uint64, n int, timeout time.Duration, window int) result {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return result{err: err}
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	var res result
	res.lat = make([]time.Duration, 0, n)
	sendNs := make([]int64, n) // instante de envio de cada ordem (atômico: escritor e leitor)
	var sem chan struct{}
	if window > 0 {
		sem = make(chan struct{}, window)
	}
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
			res.lat = append(res.lat, time.Duration(nowNs()-atomic.LoadInt64(&sendNs[i])))
			if sem != nil {
				<-sem
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
		if sem != nil {
			select {
			case sem <- struct{}{}:
			case <-done: // leitor terminou (erro): não adianta esperar vaga
				conn.Close()
				if res.err == nil {
					res.err = fmt.Errorf("conexão encerrada antes de enviar a ordem %d/%d", i, n)
				}
				return res
			}
		}
		binary.BigEndian.PutUint64(frame[0:8], firstID+uint64(i))
		binary.BigEndian.PutUint64(frame[8:16], 1000+uint64(i%64))
		binary.BigEndian.PutUint64(frame[16:24], 1)
		frame[24] = byte(i % 2)
		atomic.StoreInt64(&sendNs[i], nowNs())
		_, werr := bw.Write(frame)
		if werr == nil && sem != nil {
			werr = bw.Flush() // com janela, cada ordem sai na hora (senão a latência mediria o buffer)
		}
		if werr != nil {
			conn.Close() // destrava o leitor
			<-done
			if res.err == nil {
				res.err = fmt.Errorf("escrevendo ordem %d/%d: %w", i, n, werr)
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
