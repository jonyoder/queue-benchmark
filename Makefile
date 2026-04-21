.PHONY: help up down psql build test bench report fmt lint clean

POSTGRES_URL ?= postgres://benchmark:benchmark@localhost:5433/benchmark?sslmode=disable

help:
	@echo "Targets:"
	@echo "  up       — start ephemeral Postgres via docker-compose"
	@echo "  down     — stop and remove Postgres + its data volume"
	@echo "  psql     — connect to the benchmark Postgres"
	@echo "  build    — build ./cmd/bench"
	@echo "  test     — run Go tests"
	@echo "  bench    — run a scenario: make bench LIB=river SCENARIO=steady"
	@echo "  report   — analyze collected JSONL data"
	@echo "  fmt      — gofmt + goimports"
	@echo "  clean    — remove build artifacts and results"

up:
	docker compose up -d
	@echo "Waiting for Postgres..."
	@until docker compose exec -T postgres pg_isready -U benchmark -d benchmark >/dev/null 2>&1; do sleep 1; done
	@echo "Postgres ready at $(POSTGRES_URL)"

down:
	docker compose down -v

psql:
	docker compose exec postgres psql -U benchmark -d benchmark

build:
	go build -o bin/bench ./cmd/bench

test:
	go test ./...

bench: build
	@if [ -z "$(LIB)" ] || [ -z "$(SCENARIO)" ]; then echo "Usage: make bench LIB=<river|platlib> SCENARIO=<name>"; exit 1; fi
	./bin/bench run --lib=$(LIB) --scenario=$(SCENARIO) --postgres-url='$(POSTGRES_URL)'

report:
	./bin/bench report --results-dir=./results

fmt:
	gofmt -w .
	go mod tidy

clean:
	rm -rf bin/ results/
