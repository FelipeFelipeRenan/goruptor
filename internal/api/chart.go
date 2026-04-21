package api

import (
	"encoding/json"
	"log"

	"github.com/gofiber/fiber/v2"
)

type Candle struct {
	Time   int64  `json:"time"`
	Open   uint64 `json:"open"`
	High   uint64 `json:"high"`
	Low    uint64 `json:"low"`
	Close  uint64 `json:"close"`
	Volume uint64 `json:"volume"`
}

func (s *Server) handleGetCandles(c *fiber.Ctx) error {
	// A Query Blindada com Casts ::BIGINT
	query := `
		SELECT 
			EXTRACT(EPOCH FROM date_trunc('minute', created_at))::BIGINT AS time,
			(array_agg(price ORDER BY created_at ASC))[1]::BIGINT AS open,
			MAX(price)::BIGINT AS high,
			MIN(price)::BIGINT AS low,
			(array_agg(price ORDER BY created_at DESC))[1]::BIGINT AS close,
			SUM(quantity)::BIGINT AS volume
		FROM trade_history
		GROUP BY date_trunc('minute', created_at)
		ORDER BY time ASC;
	`

	// SE O SEU BATCHER ESTIVER NIL AQUI, VAI DAR PANIC (Verifique seu main.go!)
	rows, err := s.batcher.DB.Query(query) // Lembre de usar o getter apropriado pro DB
	if err != nil {
		log.Printf("❌ Erro na Query SQL: %v", err)
		return c.Status(500).SendString("Erro no banco")
	}
	defer rows.Close()

	var candles []Candle
	for rows.Next() {
		var candle Candle
		if err := rows.Scan(&candle.Time, &candle.Open, &candle.High, &candle.Low, &candle.Close, &candle.Volume); err != nil {
			log.Printf("❌ Erro no Scan do Go: %v", err)
			continue
		}
		candles = append(candles, candle)
	}

	// Se não tiver nenhum trade ainda, devolve vazio pra não quebrar o JS
	if len(candles) == 0 {
		return c.SendStatus(200)
	}
	
	// Envelopamos o Array dentro de um objeto pra blindar contra o parser do HTMX
	response := map[string]interface{}{
		"candles": candles,
	}
	jsonData, _ := json.Marshal(response)

	c.Set("HX-Trigger", `{"updateChart": `+string(jsonData)+`}`)
	return c.SendStatus(200)
}
