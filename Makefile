COMPOSE := docker compose -f deploy/docker-compose.yml
DSN ?= postgres://agentd:agentd@localhost:5432/agentd?sslmode=disable

.PHONY: build test test-short up down logs migrate serve work demo fmt vet

build:
	go build ./...

## test runs the full suite, including testcontainers integration tests (needs Docker).
test:
	go test ./... -timeout 600s

## test-short skips anything that needs Docker.
test-short:
	go test ./... -short

fmt:
	gofmt -l -w .

vet:
	go vet ./...

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down -v

logs:
	$(COMPOSE) logs -f api worker

migrate:
	go run ./cmd/agentd migrate -dsn "$(DSN)"

serve:
	go run ./cmd/agentd serve -dsn "$(DSN)"

work:
	go run ./cmd/agentd work -dsn "$(DSN)"

## demo submits a run against a locally running API and tails its SSE stream.
demo:
	@id=$$(curl -s -X POST localhost:8080/v1/runs \
		-H 'content-type: application/json' \
		-d '{"goal":"what is the holding in the stub opinion?"}' \
		| sed -n 's/.*"id":"\([^"]*\)".*/\1/p'); \
	echo "run $$id"; \
	curl -N "localhost:8080/v1/runs/$$id/stream"
