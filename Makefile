COMPOSE := docker compose -f deploy/docker-compose.yml
DSN ?= postgres://agentd:agentd@localhost:5432/agentd?sslmode=disable
MODEL ?= qwen2.5:7b
MODEL_URL ?= http://localhost:11434/v1
EMBED_MODEL ?= mxbai-embed-large
ANTHROPIC_MODEL ?= claude-opus-5
SANDBOX_IMAGE ?= agentd/sandbox:python
COURT ?= scotus
LIMIT ?= 2500
CORPUS ?= data/corpus/$(COURT).jsonl

.PHONY: build test test-short test-sandbox test-retrieval test-live test-live-anthropic up down logs migrate serve work demo demo-anthropic demo-python demo-legal compare crash-demo model-pull sandbox-build fetch-corpus ingest ingest-fixture eval-retrieval fmt vet

build:
	go build ./...

## test runs the full suite, including testcontainers integration tests and the
## sandbox safety tests against the local Docker daemon (needs Docker).
test:
	go test ./... -timeout 600s

## test-short skips anything that needs Docker.
test-short:
	go test ./... -short

## test-sandbox runs only the Docker executor tests and the SAFETY set (egress,
## fork bomb, filesystem, privileges, memory). Builds the sandbox image first.
test-sandbox:
	go test ./internal/sandbox -run 'TestDocker|TestSafety' -v -count=1 -timeout 300s

## sandbox-build builds the image every `run_python` tool call runs in.
sandbox-build:
	docker build -t $(SANDBOX_IMAGE) deploy/sandbox

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

up: sandbox-build
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
	go run ./cmd/agentd work -dsn "$(DSN)" -model-url "$(MODEL_URL)" -model "$(MODEL)" -sandbox-image "$(SANDBOX_IMAGE)"

## model-pull fetches the default local model and the embedding model into Ollama.
model-pull:
	ollama pull $(MODEL)
	ollama pull $(EMBED_MODEL)

## test-retrieval runs only the pgvector-backed retrieval integration tests (needs Docker, no model).
test-retrieval:
	go test ./internal/retrieval/... ./internal/store -run 'TestIngest|TestSearch|TestGetDocument' -count=1 -timeout 300s

## fetch-corpus pulls opinions from CourtListener into $(CORPUS). Set
## COURTLISTENER_TOKEN for authenticated rate limits; re-running resumes.
fetch-corpus:
	go run ./cmd/agentd fetch -court $(COURT) -filed-after 2010-01-01 -limit $(LIMIT) -out $(CORPUS)

## ingest chunks, embeds (via Ollama), and upserts the fetched corpus.
ingest:
	go run ./cmd/agentd ingest -dsn "$(DSN)" -file $(CORPUS) -model-url "$(MODEL_URL)" -embed-model $(EMBED_MODEL)

## ingest-fixture loads the 12-opinion test fixture, enough for demo-legal
## and the smoke eval without a CourtListener pull.
ingest-fixture:
	go run ./cmd/agentd ingest -dsn "$(DSN)" -file evals/retrieval/fixture.jsonl -model-url "$(MODEL_URL)" -embed-model $(EMBED_MODEL)

## eval-retrieval prints recall@k and MRR per mode over the labeled query set
## and exits nonzero below the thresholds in labels.yaml.
eval-retrieval:
	go run ./cmd/agentd eval retrieval -dsn "$(DSN)" -model-url "$(MODEL_URL)" -embed-model $(EMBED_MODEL)

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

## demo-python makes the model do the date math in the sandbox instead of the builtin.
demo-python:
	@id=$$(curl -s -X POST localhost:8080/v1/runs \
		-H 'content-type: application/json' \
		-d '{"goal":"A motion was served on 2026-09-03. Write and run a Python script with the run_python tool that computes the date 30 weekdays later (skip Saturdays and Sundays) and prints it as YYYY-MM-DD. Then call finish with that date.","agent_config":{"tools":["run_python","finish"]}}' \
		| jq -r .id); \
	echo "run $$id"; \
	curl -N "localhost:8080/v1/runs/$$id/stream"

## demo-legal asks a research question against the ingested corpus; the model
## must search, read, and cite. Run `make ingest` (or `make ingest-fixture`) first.
demo-legal:
	@id=$$(curl -s -X POST localhost:8080/v1/runs \
		-H 'content-type: application/json' \
		-d '{"goal":"What did the Court hold about warrants for historical cell phone location records? Use search_corpus to find the controlling opinion, fetch_document to read it, then call finish with a short answer citing the opinion by source_id and paragraph ordinal.","agent_config":{"tools":["search_corpus","fetch_document","finish"]}}' \
		| jq -r .id); \
	echo "run $$id"; \
	curl -N "localhost:8080/v1/runs/$$id/stream"

## compare runs the same goal against the local model and Claude and diffs the trajectories.
compare:
	ANTHROPIC_MODEL="$(ANTHROPIC_MODEL)" ./scripts/compare-providers.sh

## crash-demo kills a worker mid-tool-call and shows a second worker resume the run.
crash-demo:
	DSN="$(DSN)" MODEL_URL="$(MODEL_URL)" MODEL="$(MODEL)" ./scripts/crash-demo.sh
