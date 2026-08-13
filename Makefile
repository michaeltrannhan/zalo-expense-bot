SHELL := /bin/sh
BIN := bin

# Host port for the local PostgreSQL container; .env may override it when
# 5432 is taken by another service (see docker-compose.yml).
POSTGRES_PORT ?= 5432
MINIO_API_PORT ?= 9000
MINIO_CONSOLE_PORT ?= 9001

-include .env

# Safe local defaults let the deterministic simulator and browser playground
# start without first creating a secrets file. A real .env still overrides
# every value, and config.Load keeps pilot/production fail-closed.
APP_ENV ?= development
POSTGRES_PASSWORD ?= postgres
DATABASE_URL ?= postgres://postgres:$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/zl_expense?sslmode=disable
ZALO_WEBHOOK_SECRET ?= dev-secret-change-me
PLAYGROUND_ADDR ?= 127.0.0.1:8090
PLAYGROUND_DATABASE_URL ?= postgres://postgres:$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/zl_expense_playground?sslmode=disable
override E2E_DATABASE_URL := postgres://postgres:$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/zl_expense_e2e?sslmode=disable
E2E_TIMEOUT ?= 10m
export

.PHONY: all build dev up down migrate test test-integration race lint vet fmt simulate playground provider-check e2e-real run-local vm-rebuild clean

all: build

up: ## Start PostgreSQL + MinIO (local S3)
	docker compose up -d postgres minio minio-init

down: ## Stop the stack
	docker compose down

migrate: up ## Apply database migrations
	@until docker compose exec -T postgres pg_isready -U postgres -d zl_expense >/dev/null 2>&1; do sleep 0.5; done
	go run ./cmd/migrate

build: ## Build all binaries
	go build -o $(BIN)/api ./cmd/api
	go build -o $(BIN)/receipt-worker ./cmd/receipt-worker
	go build -o $(BIN)/notification-worker ./cmd/notification-worker
	go build -o $(BIN)/migrate ./cmd/migrate
	go build -o $(BIN)/zalo-poll ./cmd/zalo-poll
	go build -o $(BIN)/simulate ./cmd/simulate
	go build -o $(BIN)/playground ./cmd/playground
	go build -o $(BIN)/e2e ./cmd/e2e

dev: migrate ## Run the API locally
	go run ./cmd/api

test: ## Unit tests
	go test ./...

test-integration: up ## Tests that need a real PostgreSQL (isolated zl_expense_test DB)
	@until docker compose exec -T postgres pg_isready -U postgres -d zl_expense >/dev/null 2>&1; do sleep 0.5; done
	@sh scripts/ensure-test-db.sh
	TEST_DATABASE_URL="postgres://postgres:$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/zl_expense_test?sslmode=disable" go test -p 1 -count=1 -tags integration ./...

race: up ## Race-enabled integration run (isolated zl_expense_test DB)
	@until docker compose exec -T postgres pg_isready -U postgres -d zl_expense >/dev/null 2>&1; do sleep 0.5; done
	@sh scripts/ensure-test-db.sh
	TEST_DATABASE_URL="postgres://postgres:$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/zl_expense_test?sslmode=disable" go test -p 1 -race -count=1 -tags integration ./...

vet:
	go vet ./...

fmt:
	@gofmt -l . | grep -v '^$$' && exit 1 || true

lint: fmt vet

simulate: migrate ## End-to-end local demo (no Zalo, no cloud)
	EXTRACTION_ENABLED=true OUTBOUND_ENABLED=true go run ./cmd/simulate

playground: up ## Browser chat + settings lab (local mock OCR, no Zalo/cloud)
	@until docker compose exec -T postgres pg_isready -U postgres -d zl_expense >/dev/null 2>&1; do sleep 0.5; done
	@sh scripts/ensure-playground-db.sh
	@echo "Local playground: http://$(PLAYGROUND_ADDR)"
	APP_ENV=development DATABASE_URL="$(PLAYGROUND_DATABASE_URL)" ZALO_WEBHOOK_SECRET=dev-secret-change-me \
		ZALO_BOT_TOKEN= OBJECTSTORE=local DATA_DIR=./data EXTRACTOR=mock PILOT_ALLOWLIST= \
		EXTRACTION_ENABLED=true OUTBOUND_ENABLED=true go run ./cmd/playground -addr $(PLAYGROUND_ADDR)

provider-check: build ## Validate real Zalo token + Gemini OCR using synthetic data only
	APP_ENV=development EXTRACTOR=gemini GEMINI_MODEL="$${GEMINI_MODEL:-gemini-3.6-flash}" \
		ZALO_API_BASE="$${ZALO_API_BASE:-https://bot-api.zaloplatforms.com}" \
		EXTRACTION_ENABLED=true OUTBOUND_ENABLED=true ./$(BIN)/e2e preflight

e2e-real: build up ## Supervised real Zalo -> Gemini -> confirmation E2E (resets only zl_expense_e2e)
	@until docker compose exec -T postgres pg_isready -U postgres -d postgres >/dev/null 2>&1; do sleep 0.5; done
	@echo "Resetting dedicated database: zl_expense_e2e"
	@scripts/reset-e2e-db.sh
	@E2E_DATABASE_URL="$(E2E_DATABASE_URL)" E2E_TIMEOUT="$(E2E_TIMEOUT)" scripts/run-real-e2e.sh

# Run the full stack in one terminal. API_FLAGS=-poll for real-Zalo long
# polling; OBJECTSTORE/EXTRACTOR come from .env as usual.
API_FLAGS ?=
run-local: build migrate ## Run api + both workers together (Ctrl-C stops all)
	@scripts/run-local.sh $(API_FLAGS)

vm-rebuild: ## VM: start postgres, migrate, rebuild binaries, restart systemd units
	@sh scripts/vm-rebuild.sh

clean:
	rm -rf $(BIN) data
