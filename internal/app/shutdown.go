// Package app concentra o ciclo de vida do servidor: em que ORDEM as peças
// são desligadas. Está separado do main para poder ser testado.
package app

import (
	"context"
	"errors"
	"fmt"
	"log"
)

// Component é uma peça que sabe parar graciosamente (HTTP, TCP, anel).
type Component interface {
	Shutdown(ctx context.Context) error
}

// Components reúne as peças do servidor. Qualquer campo pode ser nil.
type Components struct {
	HTTP Component // porta de entrada HTTP
	TCP  Component // porta de entrada TCP binário
	Ring Component // anel: journaler + motor

	WAL     interface{ Close() error }
	Batcher interface {
		Close(ctx context.Context) error
	}

	// CloseTrades fecha o canal de trades do SQS; TradesDone fecha quando os
	// workers terminam de enviar o que restava.
	CloseTrades func()
	TradesDone  <-chan struct{}

	// Logf recebe o andamento (padrão: log.Printf).
	Logf func(format string, args ...any)
}

// Shutdown desliga tudo na ordem certa. Cada passo depende do anterior:
//
//  1. HTTP e TCP: param de aceitar ordens e esperam as que estão em andamento
//     terminarem (cada uma espera o disco, então recebem resposta de verdade).
//  2. Anel: drena o que já foi aceito; o journaler grava e o motor processa tudo.
//  3. WAL: flush + fsync finais e fecha o arquivo.
//  4. Batcher: grava no Postgres o lote que ainda estava na memória.
//  5. SQS: só agora, com o motor parado, fecha o canal de trades (fechar antes
//     faria o motor dar panic ao enviar um trade) e espera os envios pendentes.
//
// Um passo que falha não impede os seguintes (exceto o passo 5, que exige o
// motor parado). Os erros são devolvidos juntos.
func (c *Components) Shutdown(ctx context.Context) error {
	logf := c.Logf
	if logf == nil {
		logf = log.Printf
	}

	var errs []error
	step := func(name string, f func() error) bool {
		logf("⏳ Desligando: %s...", name)
		if err := f(); err != nil {
			logf("❌ %s: %v", name, err)
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			return false
		}
		logf("✅ %s", name)
		return true
	}
	stop := func(comp Component) func() error {
		return func() error { return comp.Shutdown(ctx) }
	}

	if c.HTTP != nil {
		step("servidor HTTP", stop(c.HTTP))
	}
	if c.TCP != nil {
		step("servidor TCP", stop(c.TCP))
	}

	engineStopped := true
	if c.Ring != nil {
		engineStopped = step("anel (journaler + motor)", stop(c.Ring))
	}
	if c.WAL != nil {
		step("WAL", c.WAL.Close)
	}
	if c.Batcher != nil {
		step("lote final do Postgres", func() error { return c.Batcher.Close(ctx) })
	}

	if c.CloseTrades != nil {
		if !engineStopped {
			logf("⚠️ O motor não parou: não fecho o canal de trades (o motor ainda pode enviar nele)")
		} else {
			step("trades do SQS", func() error {
				c.CloseTrades()
				if c.TradesDone == nil {
					return nil
				}
				select {
				case <-c.TradesDone:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		}
	}

	return errors.Join(errs...)
}
