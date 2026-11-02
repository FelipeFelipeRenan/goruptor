package app_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/FelipeFelipeRenan/goruptor/internal/api"
	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
)

// Benchmarks de PONTA A PONTA: clientes TCP reais -> Ingress -> anel -> journaler
// -> WAL em disco -> motor, com Batcher e trades. Só o Postgres e o SQS são
// substituídos por destinos em memória.
//
// Dois modos de journal, para separar o custo de SOFTWARE do custo do DISCO:
//
//	fsync   padrão de produção: ACK só depois do fsync (depende muito do disco)
//	nosync  fsync quase nunca (SyncInterval alto): mede o custo da pilha de software
//
// Rodar: go test -run xxx -bench E2E -benchtime 100000x ./internal/app/

var journalModes = []struct {
	name string
	opts disruptor.JournalOptions
}{
	{"fsync", disruptor.JournalOptions{}},
	{"nosync", disruptor.JournalOptions{SyncInterval: time.Hour}},
}

func stopStack(b *testing.B, s *stack) {
	b.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.comps.Shutdown(ctx); err != nil {
		b.Fatalf("shutdown: %v", err)
	}
}

// BenchmarkE2E_TCP_LockStep: N clientes, cada um com UMA ordem em voo (escreve,
// espera o ACK, escreve a próxima), como o cannon com N workers.
func BenchmarkE2E_TCP_LockStep(b *testing.B) {
	for _, mode := range journalModes {
		for _, clients := range []int{16, 128} {
			b.Run(fmt.Sprintf("%s/clients=%d", mode.name, clients), func(b *testing.B) {
				s := newStackWith(b, mode.opts)
				addr := s.tcp.Addr().String()

				per := b.N / clients
				if per == 0 {
					per = 1
				}
				total := per * clients

				var wg sync.WaitGroup
				b.ResetTimer()
				start := time.Now()
				for c := 0; c < clients; c++ {
					wg.Add(1)
					go func(c int) {
						defer wg.Done()
						conn, err := net.Dial("tcp", addr)
						if err != nil {
							b.Error(err)
							return
						}
						defer conn.Close()
						var ack [1]byte
						for i := 0; i < per; i++ {
							id := uint64(c)*10_000_000 + uint64(i) + 1
							if _, err := conn.Write(frame(id, 1000+id%64, 1, byte(id&1))); err != nil {
								b.Error(err)
								return
							}
							if _, err := io.ReadFull(conn, ack[:]); err != nil || ack[0] != api.AckOK {
								b.Errorf("ACK: %v %v", ack[0], err)
								return
							}
						}
					}(c)
				}
				wg.Wait()
				elapsed := time.Since(start)
				b.StopTimer()
				b.ReportMetric(float64(total)/elapsed.Seconds(), "orders/s")
				stopStack(b, s)
			})
		}
	}
}

// BenchmarkE2E_TCP_Pipelined: poucos clientes que enviam MUITAS ordens em
// pipeline e leem os ACKs em paralelo (como o cmd/dma-loadtest).
func BenchmarkE2E_TCP_Pipelined(b *testing.B) {
	for _, mode := range journalModes {
		b.Run(mode.name+"/clients=8", func(b *testing.B) {
			const clients = 8
			s := newStackWith(b, mode.opts)
			addr := s.tcp.Addr().String()

			per := b.N / clients
			if per == 0 {
				per = 1
			}
			total := per * clients

			var wg sync.WaitGroup
			b.ResetTimer()
			start := time.Now()
			for c := 0; c < clients; c++ {
				wg.Add(1)
				go func(c int) {
					defer wg.Done()
					conn, err := net.Dial("tcp", addr)
					if err != nil {
						b.Error(err)
						return
					}
					defer conn.Close()

					done := make(chan struct{})
					go func() { // leitor de ACKs
						defer close(done)
						br := bufio.NewReader(conn)
						for i := 0; i < per; i++ {
							a, err := br.ReadByte()
							if err != nil || a != api.AckOK {
								b.Errorf("ACK %d: %v %v", i, a, err)
								return
							}
						}
					}()

					bw := bufio.NewWriterSize(conn, 64*1024)
					for i := 0; i < per; i++ {
						id := uint64(c)*10_000_000 + uint64(i) + 1
						bw.Write(frame(id, 1000+id%64, 1, byte(id&1)))
					}
					bw.Flush()
					<-done
				}(c)
			}
			wg.Wait()
			elapsed := time.Since(start)
			b.StopTimer()
			b.ReportMetric(float64(total)/elapsed.Seconds(), "orders/s")
			stopStack(b, s)
		})
	}
}
