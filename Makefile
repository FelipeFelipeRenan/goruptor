# Goruptor - atalhos de desenvolvimento. Rode `make` para ver a lista.

BINARY_NAME := goruptor-server
MAIN_PKG    := ./cmd/goruptor-server
WAL_FILE    := goruptor_journal.jsonl
SERVER_HTTP := http://localhost:3000
SERVER_TCP  := 127.0.0.1:3001

.DEFAULT_GOAL := help

.PHONY: help setup up down infra nuke build run test bench fmt vet check loadtest clean \
		cannon-buy cannon-sell cannon-peak cannon-peak-buy cannon-peak-sell \
		cannon-dma-buy cannon-dma-sell cannon-dma-peak

help: ## Mostra esta ajuda
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-16s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------- ambiente

setup: up infra ## Sobe o MiniStack (Docker) e provisiona SQS/RDS com Terraform

up: ## Sobe os containers do MiniStack (AWS local + Postgres)
	docker-compose up -d

down: ## Derruba os containers
	docker-compose down

infra: ## Recria a infraestrutura local (SQS + RDS) com Terraform
	@echo "🧹 Limpando estado antigo do Terraform..."
	@rm -rf infra/terraform.tfstate infra/terraform.tfstate.backup infra/.terraform
	@echo "🏗️  Construindo infraestrutura limpa no MiniStack..."
	@cd infra && terraform init && terraform apply -auto-approve
	@echo "✅ Infraestrutura pronta!"

nuke: ## Derruba tudo: containers, volumes e estado do Terraform
	@echo "☢️  Removendo containers, volumes e estado do Terraform..."
	@docker-compose down -v
	@docker rm -f ministack-rds-goruptor-db 2>/dev/null || true
	@docker container prune -f
	@rm -rf infra/terraform.tfstate infra/terraform.tfstate.backup infra/.terraform
	@echo "💀 Tudo limpo. Rode 'make setup' de novo."

# ---------------------------------------------------------------- build e execução

build: ## Compila o servidor em bin/
	go build -o bin/$(BINARY_NAME) $(MAIN_PKG)

run: ## Inicia o servidor (rode da raiz do repositório: ele serve web/index.html)
	go run $(MAIN_PKG)

# ---------------------------------------------------------------- qualidade

test: ## Roda todos os testes com o race detector
	go test -race -count=1 ./...

bench: ## Benchmarks: anel, motor, WAL, batcher e TCP ponta a ponta
	go test -run xxx -bench . -benchtime 1s -benchmem ./internal/matching/ ./internal/disruptor/ ./internal/storage/ ./internal/ingest/
	go test -run xxx -bench E2E -benchtime 50000x ./internal/app/

fmt: ## Formata o código (gofmt)
	gofmt -w .

vet: ## Análise estática (go vet)
	go vet ./...

check: ## O mesmo que a CI: formatação, vet e testes
	@test -z "$$(gofmt -l .)" || (echo "arquivos sem gofmt:"; gofmt -l .; exit 1)
	go vet ./...
	go test -race -count=1 ./...

# ---------------------------------------------------------------- carga

loadtest: ## Carga no TCP binário com o cliente Go (confirmadas/s). Ex.: make loadtest N=200000 C=32
	go run ./cmd/dma-loadtest -addr $(SERVER_TCP) -n $(or $(N),100000) -c $(or $(C),16)

cannon-buy: ## [cannon] 10.000 ordens de COMPRA via HTTP
	cannon -u $(SERVER_HTTP)/api/orders -c 10000 -w 20 -X POST -H "Content-Type: application/json" --body '{"order_id": {{number}}, "price": {{number}}, "quantity": 1, "side": "BUY"}'

cannon-sell: ## [cannon] 10.000 ordens de VENDA via HTTP
	cannon -u $(SERVER_HTTP)/api/orders -c 10000 -w 20 -X POST -H "Content-Type: application/json" --body '{"order_id": {{number}}, "price": {{number}}, "quantity": 1, "side": "SELL"}'

cannon-peak-buy: ## [cannon] pico: 100.000 ordens de COMPRA via HTTP (150 workers)
	@echo "🔥 PEAK: 100.000 ordens de COMPRA (150 workers)..."
	cannon -u $(SERVER_HTTP)/api/orders -c 100000 -w 150 -X POST \
		-H "Connection: keep-alive" \
		-H "Content-Type: application/json" \
		--body '{"order_id": {{number}}, "price": {{number}}, "quantity": 1, "side": "BUY"}'

cannon-peak-sell: ## [cannon] pico: 100.000 ordens de VENDA via HTTP (150 workers)
	@echo "🔥 PEAK: 100.000 ordens de VENDA (150 workers)..."
	cannon -u $(SERVER_HTTP)/api/orders -c 100000 -w 150 -X POST \
		-H "Connection: keep-alive" \
		-H "Content-Type: application/json" \
		--body '{"order_id": {{number}}, "price": {{number}}, "quantity": 1, "side": "SELL"}'

cannon-peak: cannon-peak-buy cannon-peak-sell ## [cannon] pico de compra + venda via HTTP

# Atenção: o servidor TCP espera frames BINÁRIOS de 25 bytes (veja README). Estes
# alvos enviam o corpo em texto; só funcionam se o seu cannon converter o corpo
# para o frame binário. Para testar a porta TCP com certeza, use `make loadtest`.
cannon-dma-buy: ## [cannon] 100k ordens de COMPRA no TCP (requer corpo binário, veja nota)
	@echo "🔥 100k ordens de COMPRA (DMA :3001)..."
	cannon -u $(SERVER_TCP) --mode tcp -c 100000 -w 150 --body 'B,{{number}},{{number}},1'

cannon-dma-sell: ## [cannon] 100k ordens de VENDA no TCP (requer corpo binário, veja nota)
	@echo "📉 100k ordens de VENDA (DMA :3001)..."
	cannon -u $(SERVER_TCP) --mode tcp -c 100000 -w 150 --body 'S,{{number}},{{number}},1'

cannon-dma-peak: cannon-dma-sell cannon-dma-buy ## [cannon] pico de venda + compra no TCP

# ---------------------------------------------------------------- limpeza

clean: down ## Para o Docker e apaga o WAL e os binários
	rm -f $(WAL_FILE)
	rm -rf bin
	@echo "🧹 Ambiente limpo."
