# Variáveis
BINARY_NAME=goruptor-server
MAIN_PATH=cmd/goruptor-server/main.go
WAL_FILE=goruptor_journal.jsonl

.PHONY: all setup up down infra run test clean cannon-sell cannon-buy help

help: ## Exibe esta ajuda
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

setup: up infra ## Sobe o Docker e provisiona o Terraform (Tudo pronto para rodar)

up: ## Sobe os containers do MiniStack (LocalStack + Postgres)
	docker-compose up -d

down: ## Derruba todos os containers
	docker-compose down

infra: ## Aplica o Terraform para criar SQS e RDS no MiniStack
	cd infra && terraform init && terraform apply -auto-approve

run: ## Inicia a Corretora Go
	go run $(MAIN_PATH)

test: ## Roda os testes unitários de lógica financeira
	go test -v ./internal/matching/...

clean: down ## Limpa o ambiente, para o docker e deleta o arquivo WAL
	rm -f $(WAL_FILE)
	@echo "🧹 Ambiente limpo e WAL removido."

cannon-sell: ## Dispara 5000 ordens de VENDA usando o seu Cannon (Rust)
	cannon -u http://localhost:3000/api/orders -c 5000 -w 20 -X POST -H "Content-Type: application/json" --body '{"order_id": {{number}}, "price": {{number}}, "quantity": 1, "side": "SELL"}'

cannon-buy: ## Dispara 5000 ordens de COMPRA usando o seu Cannon (Rust)
	cannon -u http://localhost:3000/api/orders -c 5000 -w 20 -X POST -H "Content-Type: application/json" --body '{"order_id": {{number}}, "price": {{number}}, "quantity": 1, "side": "BUY"}'