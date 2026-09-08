COMPOSE := docker compose -f deploy/docker-compose.yml
DSN ?= postgres://agentd:agentd@localhost:5432/agentd?sslmode=disable
MODEL ?= qwen2.5:7b
MODEL_URL ?= http://localhost:11434/v1
ANTHROPIC_MODEL ?= claude-opus-5

.PHONY: build test test-short test-live test-live-anthropic up down logs migrate serve work demo demo-anthropic compare crash-demo model-pull fmt vet

build:
	go build ./...

## test runs the full suite, including testcontainers integration tests (needs Docker).
test:
	go test ./... -timeout 600s

## test-short skips anything that needs Docker.
test-short:
	go test ./... -short

## test-live runs the opt-in test against a real local model (needs Ollama + the model pulled).
test-live:
	AGENTD_LIVE_MODEL=1 AGENTD_MODEL_URL=$(MODEL_URL) AGENTD_MODEL=$(MODEL) AGENTD_HTTP_DEBUG=1 go test ./internal/model/local -run TestLive -v -count=1

## test-live-anthropic makes two real Messages API calls (needs ANTHROPIC_API_KEY; costs a fraction of a cent).
test-live-anthropic:
	AGENTD_LIVE_ANTHROPIC=1 AGENTD_ANTHROPIC_MODEL=$(ANTHROPIC_MODEL) AGENTD_HTTP_DEBUG=1 go test ./internal/model/anthropic -run TestLive -v -count=1

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
	go run ./cmd/agentd work -dsn "$(DSN)" -model-url "$(MODEL_URL)" -model "$(MODEL)"

## model-pull fetches the default local model into Ollama.
model-pull:
	ollama pull $(MODEL)

## demo submits a run against a locally running API and tails its SSE stream.
demo:
	@id=$$(curl -s -X POST localhost:8080/v1/runs \
		-H 'content-type: application/json' \
		-d '{"goal":"A motion was served on 2026-09-03. Use compute_deadline to find the date 30 weekdays later, skipping weekends, then call finish with the answer."}' \
		| jq -r .id); \
	echo "run $$id"; \
	curl -N "localhost:8080/v1/runs/$$id/stream"

## demo-anthropic is `demo` against Claude with a real budget, so the spend counter moves.
demo-anthropic:
	@id=$$(curl -s -X POST localhost:8080/v1/runs \
		-H 'content-type: application/json' \
		-d '{"goal":"A motion was served on 2026-09-03. Use compute_deadline to find the date 30 weekdays later, skipping weekends, then call finish with the answer.","agent_config":{"model":"anthropic/$(ANTHROPIC_MODEL)"},"budget_usd":"0.50"}' \
		| jq -r .id); \
	echo "run $$id"; \
	curl -N "localhost:8080/v1/runs/$$id/stream"

## compare runs the same goal against the local model and Claude and diffs the trajectories.
compare:
	ANTHROPIC_MODEL="$(ANTHROPIC_MODEL)" ./scripts/compare-providers.sh

## crash-demo kills a worker mid-tool-call and shows a second worker resume the run.
crash-demo:
	DSN="$(DSN)" MODEL_URL="$(MODEL_URL)" MODEL="$(MODEL)" ./scripts/crash-demo.sh
