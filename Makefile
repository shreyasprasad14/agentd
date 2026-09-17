COMPOSE := docker compose -f deploy/docker-compose.yml
DSN ?= postgres://agentd:agentd@localhost:5432/agentd?sslmode=disable
## EVAL_DSN is a database of its own. The agent eval suite embeds with a
## deterministic fake embedder, and its corpus includes planted documents that
## say "disregard your instructions" — neither belongs in the database the
## demos search. It is created on first use.
EVAL_DSN ?= postgres://agentd:agentd@localhost:5432/agentd_eval?sslmode=disable
MODEL ?= qwen2.5:7b
MODEL_URL ?= http://localhost:11434/v1
EMBED_MODEL ?= mxbai-embed-large
ANTHROPIC_MODEL ?= claude-opus-5
SANDBOX_IMAGE ?= agentd/sandbox:python
COURT ?= scotus
LIMIT ?= 2500
## REPORTER is the Caselaw Access Project reporter slug `make fetch-cap` walks:
## "us" is United States Reports. See ADR-40.
REPORTER ?= us
## CORPUS defaults to the CAP pull, which is the corpus the retrieval numbers
## are measured on. `make fetch-corpus CORPUS=data/corpus/$(COURT).jsonl` still
## pulls from CourtListener for anything more recent than 2014.
CORPUS ?= data/corpus/$(COURT)-cap.jsonl
## CORPUS_DEDUP holds one document per case; it is what the retrieval
## benchmark measures against. See `make dedupe` and ADR-39.
CORPUS_DEDUP ?= $(CORPUS:.jsonl=-dedup.jsonl)

JAEGER_UI ?= http://localhost:16686
PROMETHEUS_UI ?= http://localhost:9090

.PHONY: build test test-short test-sandbox test-retrieval test-live test-live-anthropic up down logs migrate serve work demo demo-anthropic demo-python demo-legal demo-budget demo-cancel compare crash-demo trace metrics model-pull sandbox-build fetch-cap fetch-corpus dedupe ingest ingest-fixture eval eval-record eval-live eval-record-live eval-retrieval fmt vet

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

## fetch-cap pulls opinions from the Caselaw Access Project into $(CORPUS).
## No token and no quota: the data is static files behind a CDN, one request
## per volume rather than two per opinion, which is what makes a corpus of a
## few thousand documents a five-minute job instead of a forty-day one. The
## tradeoff is recency — CAP's U.S. Reports stop at 2014. See ADR-40.
fetch-cap:
	go run ./cmd/agentd fetch-cap -reporter $(REPORTER) -court $(COURT) -limit $(LIMIT) -out $(CORPUS)

## fetch-corpus pulls opinions from CourtListener instead, which is current to
## the day but allows 125 requests a day on the free tier — roughly 60 opinions.
## Use it to top up a CAP corpus with recent cases, not to build one. Set
## COURTLISTENER_TOKEN for authenticated rate limits; re-running resumes.
fetch-corpus:
	go run ./cmd/agentd fetch -court $(COURT) -filed-after 2010-01-01 -limit $(LIMIT) -out $(CORPUS)

## dedupe collapses revisions of the same case in $(CORPUS) into $(CORPUS_DEDUP),
## keyed on docket number, keeping the longest text. CourtListener publishes each
## revision under its own id, so a raw pull holds the same case several times over;
## content hashing does not catch it, because revisions genuinely differ. Run this
## between fetch-corpus and ingest — see ADR-39.
dedupe:
	go run ./cmd/agentd dedupe -in $(CORPUS) -out $(CORPUS_DEDUP)

## ingest chunks, embeds (via Ollama), and upserts $(CORPUS_DEDUP) — the
## deduplicated corpus, not the raw pull. This target used to read $(CORPUS),
## which quietly undid `make dedupe`: ingest upserts by source_id, so loading
## the raw file after the deduplicated one leaves the displaced revisions in
## the database rather than replacing them, and the corpus the benchmark runs
## against stops matching the one its labels were written for. Run `make
## dedupe` first; ADR-39 is why the two files are separate.
ingest: $(CORPUS_DEDUP)
	go run ./cmd/agentd ingest -dsn "$(DSN)" -file $(CORPUS_DEDUP) -model-url "$(MODEL_URL)" -embed-model $(EMBED_MODEL)

$(CORPUS_DEDUP): $(CORPUS)
	go run ./cmd/agentd dedupe -in $(CORPUS) -out $(CORPUS_DEDUP)

## ingest-fixture loads the 12-opinion test fixture, enough for demo-legal
## and the smoke eval without a CourtListener pull.
ingest-fixture:
	go run ./cmd/agentd ingest -dsn "$(DSN)" -file evals/retrieval/fixture.jsonl -model-url "$(MODEL_URL)" -embed-model $(EMBED_MODEL)

## eval-retrieval prints recall@k and MRR per mode over the labeled query set
## and exits nonzero below the thresholds in $(LABELS). It defaults to the
## real corpus; LABELS=evals/retrieval/labels.yaml scores the 12-opinion
## fixture instead, which runs anywhere in seconds and saturates.
LABELS ?= evals/retrieval/labels-scotus-cap.yaml
RESULTS ?= evals/retrieval/results-scotus-cap.json
eval-retrieval:
	go run ./cmd/agentd eval retrieval -dsn "$(DSN)" -model-url "$(MODEL_URL)" -embed-model $(EMBED_MODEL) \
		-labels $(LABELS) -results $(RESULTS)

## eval replays the checked-in cassettes through the real loop and prints the
## scorecard. No model, no API key, no network: Postgres is the only
## dependency, and the Docker-dependent SAFETY cases skip visibly without a
## daemon. This is the CI target.
eval:
	go run ./cmd/agentd eval suite -dsn "$(EVAL_DSN)"

## eval-record regenerates every cassette from the trajectories declared in
## evals/cases/*.yaml. Needs no model either — the scripts are the source and
## the cassettes are the artifact.
eval-record:
	go run ./cmd/agentd eval record -dsn "$(EVAL_DSN)"

## eval-live runs the same cases against a real model, which is the half of
## §12 that scores the model rather than the runtime. Set MODEL, or
## MODEL=anthropic/$(ANTHROPIC_MODEL) with ANTHROPIC_API_KEY set.
eval-live:
	go run ./cmd/agentd eval suite -dsn "$(EVAL_DSN)" -live -model "$(MODEL)" -model-url "$(MODEL_URL)" \
		-embedder $(EMBED_MODEL)

## eval-record-live re-records the cassettes from a real model, replacing the
## scripted trajectories with ones a model actually chose. Costs whatever the
## suite costs against MODEL.
eval-record-live:
	go run ./cmd/agentd eval record -dsn "$(EVAL_DSN)" -from model -model "$(MODEL)" -model-url "$(MODEL_URL)" \
		-embedder $(EMBED_MODEL)

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

## demo-budget submits a Claude run with a two-cent budget. The run stops
## before the call that would exceed it, so spent_usd stays under the number
## rather than being audited past it (ADR-22).
demo-budget:
	@id=$$(curl -s -X POST localhost:8080/v1/runs \
		-H 'content-type: application/json' \
		-d '{"goal":"Research the history of the Fourth Amendment in detail, then call finish with a thorough summary.","agent_config":{"model":"anthropic/$(ANTHROPIC_MODEL)"},"budget_usd":"0.02"}' \
		| jq -r .id); \
	echo "run $$id  (budget \$$0.02)"; \
	curl -sN "localhost:8080/v1/runs/$$id/stream" | grep --line-buffered -E '^(event|data)' | grep --line-buffered -E 'budget|finished' || true; \
	curl -s "localhost:8080/v1/runs/$$id" | jq -r '"status: \(.run.status)\nspent: \(.run.spent_usd) of \(.run.budget_usd)"'

## demo-cancel cancels a run that is inside a long tool call and prints how
## long the cancel took to land.
demo-cancel:
	./scripts/demo-cancel.sh

## compare runs the same goal against the local model and Claude and diffs the trajectories.
compare:
	ANTHROPIC_MODEL="$(ANTHROPIC_MODEL)" ./scripts/compare-providers.sh

## crash-demo kills a worker mid-tool-call and shows a second worker resume the run.
crash-demo:
	DSN="$(DSN)" MODEL_URL="$(MODEL_URL)" MODEL="$(MODEL)" ./scripts/crash-demo.sh

## trace prints (and on macOS opens) the Jaeger deep link for a run: make trace RUN=<id>.
trace:
	@test -n "$(RUN)" || { echo "usage: make trace RUN=<run-id>" >&2; exit 1; }
	@url=$$(curl -s "localhost:8080/v1/runs/$(RUN)/trace" | jq -r '.url // empty'); \
	if [ -z "$$url" ]; then echo "run $(RUN) has no trace (submitted before M4, or with tracing off)" >&2; exit 1; fi; \
	echo "$$url"; command -v open >/dev/null && open "$$url" || true

## metrics curls both processes' /metrics. The worker's port is published on
## an ephemeral host port so `--scale worker=2` does not collide, so its
## address is asked of compose rather than assumed.
metrics:
	@echo "== api =="; curl -s localhost:8080/metrics | grep -E '^agentd_[a-z_]+( |\{)' | grep -v ' 0$$' | head -30
	@addr=$$($(COMPOSE) port worker 9091 2>/dev/null | head -1); \
	if [ -z "$$addr" ]; then echo "\n(worker not running under compose; try: curl localhost:9091/metrics)"; exit 0; fi; \
	echo "\n== worker ($$addr) =="; curl -s "http://$$addr/metrics" | grep -E '^agentd_[a-z_]+( |\{)' | grep -v ' 0$$' | head -30
	@echo "\nPrometheus: $(PROMETHEUS_UI)   Jaeger: $(JAEGER_UI)"
