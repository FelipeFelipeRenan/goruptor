package storage

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"

	"github.com/FelipeFelipeRenan/goruptor/internal/disruptor"
)

type WAL struct {
	file        *os.File
	encoder     *json.Encoder
	eventStream chan disruptor.OrderEvent
	mu          sync.Mutex
}

func NewWAL(filename string) (*WAL, error) {
	file, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}

	w := &WAL{
		file:        file,
		encoder:     json.NewEncoder(file),
		eventStream: make(chan disruptor.OrderEvent, 50000),
	}

	go w.StartWriter()

	return w, nil
}

func (w *WAL) Log(event disruptor.OrderEvent) {
	w.eventStream <- event
}

func (w *WAL) StartWriter() {

	ticker := time.NewTicker(1 * time.Second)

	defer ticker.Stop()

	for {
		select {
		case event := <-w.eventStream:
			w.mu.Lock()
			if err := w.encoder.Encode(event); err != nil {
				log.Printf("❌ Erro ao gravar no WAL: %v", err)
			}
			w.mu.Unlock()

		case <-ticker.C:
			w.mu.Lock()
			if err := w.file.Sync(); err != nil {
				return
			}
			w.mu.Unlock()
		}
	}
}

func (w *WAL) ReadAll() ([]disruptor.OrderEvent, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	readFile, err := os.Open(w.file.Name())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer readFile.Close()

	var events []disruptor.OrderEvent
	decoder := json.NewDecoder(readFile)

	for decoder.More() {
		var event disruptor.OrderEvent
		if err := decoder.Decode(&event); err != nil {
			log.Printf("⚠️ Arquivo WAL corrompido no final. Ignorando resto: %v", err)
			break
		}
		events = append(events, event)
	}
	return events, nil
}
