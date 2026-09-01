# =============================================================================
# Stripe Payment & Subscription Gateway
# =============================================================================
.DEFAULT_GOAL := help
SHELL := /bin/bash

MODULE      := $(shell head -1 go.mod | awk '{print $$2}')
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)

COMPOSE     := docker compose
MIGRATIONS  := ./migrations

# Load .env for host-side goose/psql targets.
ifneq (,$(wildcard .env))
include .env
export
endif

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

# --- Stack -------------------------------------------------------------------
.PHONY: up
up: ## Start postgres, run migrations, start the API
	$(COMPOSE) up -d --build

.PHONY: down
down: ## Stop the stack (keeps the data volume)
	$(COMPOSE) down --remove-orphans

.PHONY: nuke
nuke: ## Stop the stack and DELETE the database volume
	$(COMPOSE) down -v --remove-orphans

.PHONY: logs
logs: ## Tail all service logs
	$(COMPOSE) logs -f --tail=100

.PHONY: db
db: ## Open a psql shell on the running database
	$(COMPOSE) exec -e PGPASSWORD=$(POSTGRES_PASSWORD) postgres \
		psql -U $(POSTGRES_USER) -d $(POSTGRES_DB)

.PHONY: stripe
stripe: ## Forward Stripe test events to the local webhook endpoint
	$(COMPOSE) --profile dev up stripe-cli

# --- Migrations --------------------------------------------------------------
.PHONY: migrate-up
migrate-up: ## Apply all pending migrations
	$(COMPOSE) run --rm migrate up

.PHONY: migrate-down
migrate-down: ## Roll back exactly one migration
	$(COMPOSE) run --rm migrate down

.PHONY: migrate-status
migrate-status: ## Show migration status
	$(COMPOSE) run --rm migrate status

.PHONY: migrate-redo
migrate-redo: ## Roll back and re-apply the latest migration (tests the .down)
	$(COMPOSE) run --rm migrate redo

.PHONY: verify-schema
verify-schema: ## Assert every constraint/trigger/idempotency guarantee (rolls back)
	$(COMPOSE) exec -T -e PGPASSWORD=$(POSTGRES_PASSWORD) postgres \
		psql -U $(POSTGRES_USER) -d $(POSTGRES_DB) -q \
		< scripts/verify-schema.sql

.PHONY: migrate-create
migrate-create: ## Scaffold a migration: make migrate-create name=add_invoices
	@test -n "$(name)" || (echo "usage: make migrate-create name=<snake_case>" && exit 1)
	goose -dir $(MIGRATIONS) create $(name) sql

# --- Go ----------------------------------------------------------------------
.PHONY: build
build: ## Build the API binary into ./bin
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/api ./cmd/api

.PHONY: run
run: ## Run the API against the local database
	go run ./cmd/api

.PHONY: test
test: ## Run unit tests with the race detector
	go test -race -count=1 ./...

.PHONY: test-integration
test-integration: ## Run integration tests against a live PostgreSQL (make up)
	go test -race -count=1 -tags=integration ./test/integration/...

.PHONY: cover
cover: ## Generate and open a coverage report
	go test -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -html=coverage.out

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run ./...

.PHONY: tidy
tidy: ## go mod tidy + verify
	go mod tidy && go mod verify

.PHONY: fmt
fmt: ## Format and simplify
	gofmt -s -w . && go vet ./...

.PHONY: audit
audit: tidy fmt lint test ## Full pre-commit gate
