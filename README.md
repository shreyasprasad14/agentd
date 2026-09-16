# agentd

A control plane for running LLM agents as durable, resumable, sandboxed, observable jobs.

**Status: M6 complete.** The retrieval table is measured on a real corpus; read the corpus-size
caveat under [Retrieval numbers](#retrieval-numbers) before quoting the recall columns.

An agent run is an append-only event log, and the loop is a state machine driven by it. A worker
that dies mid-run is not a lost run: a new one folds the log and continues from the last committed
step, without re-executing the tool call that was already finished. Everything else here —
sandboxing, budgets, hybrid retrieval, tracing, the eval harness — is built on that one property.

- **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** — how it works, as it stands
- **[docs/DECISIONS.md](docs/DECISIONS.md)** — 39 ADRs: the tradeoffs, already made
- **[docs/SECURITY.md](docs/SECURITY.md)** — the threat model, including what is *not* defended
- **[docs/MILESTONES.md](docs/MILESTONES.md)** — the build journal, and the defects the evals caught

## What it does

| | |
|---|---|
| **Durability** | Event-sourced runs over Postgres; leases, fencing, an idempotency ledger. `kill -9` a worker mid-tool-call and the run continues, exactly once |
| **Isolation** | Every `run_python` call in a fresh container: no network, read-only rootfs, dropped capabilities, cgroup limits |
| **Tools** | One `Tool` interface over builtins, the sandbox, and **MCP servers** (stdio or HTTP), namespaced and allowlisted per run |
| **Retrieval** | Hybrid pgvector + BM25 with reciprocal rank fusion, over real court opinions; fusion beats both inputs on MRR, and the optional LLM reranking pass is measured and reported as a loss |
| **Control** | Per-run token and dollar budgets enforced as a pre-flight ceiling; cancellation that interrupts in-flight work |
| **Observability** | One OpenTelemetry trace per run, across processes and crashes; Prometheus on both binaries; a live trajectory viewer |
| **Evals** | Cassette replay plus a live mode, scoring retrieval, citations, injection resistance, sandbox safety, crash recovery, and budgets |

## Quickstart

You need Docker, Go 1.26, and [Ollama](https://ollama.com) on the host. For Claude, export
`ANTHROPIC_API_KEY` before `make up` (or `make work`); without it local runs are unaffected and
runs that target Claude fail with an authentication error.

```bash
brew install ollama && ollama serve &   # or the desktop app
make model-pull                         # ollama pull qwen2.5:7b and mxbai-embed-large
make up                                 # builds agentd/sandbox:python, then Postgres 16 (pgvector), Jaeger, Prometheus, api, worker
make ingest-fixture                     # optional: load the 12-opinion corpus so the retrieval tools have something to find

curl -X POST localhost:8080/v1/runs \
  -H 'content-type: application/json' \
  -d '{"goal":"A motion was served on 2026-09-03. Use compute_deadline to find the date 30 weekdays later, skipping weekends, then call finish with the answer."}'
# => {"id":"...","status":"queued"}

curl -N localhost:8080/v1/runs/<id>/stream
# id: 1 / event: run_started
# id: 2 / event: model_requested
# id: 3 / event: model_responded      (tool_use: compute_deadline)
# id: 4 / event: tool_requested
# id: 5 / event: tool_succeeded       {"deadline":"2026-10-15", ...}
# id: 6 / event: model_requested
# id: 7 / event: model_responded      (tool_use: finish)
# id: 8 / event: tool_requested
# id: 9 / event: tool_succeeded
# id: 10 / event: run_finished        {"status":"succeeded","final_answer":"..."}
```

`make demo` does the submit-and-tail in one step. Resume a dropped stream from where it
left off with `-H 'Last-Event-ID: 5'`. `make trace RUN=<id>` opens that run's Jaeger waterfall
(http://localhost:16686); `make metrics` prints both processes' counters, and Prometheus is at
http://localhost:9090.

The same run against Claude, with a budget the spend counter can be seen moving against:

```bash
curl -X POST localhost:8080/v1/runs \
  -H 'content-type: application/json' \
  -d '{"goal":"...","agent_config":{"model":"anthropic/claude-opus-5"},"budget_usd":"0.50"}'
```

`make demo-anthropic` does that; `make compare` runs one goal against both providers and
prints the two trajectories side by side.

The same question with the date math done in the sandbox instead of the builtin: grant only
`run_python` and `finish`, and the model has to write the script.

```bash
curl -X POST localhost:8080/v1/runs \
  -H 'content-type: application/json' \
  -d '{"goal":"A motion was served on 2026-09-03. Write and run a Python script with the run_python tool that computes the date 30 weekdays later (skip Saturdays and Sundays) and prints it as YYYY-MM-DD. Then call finish with that date.","agent_config":{"tools":["run_python","finish"]}}'
# ...
# id: 4 / event: tool_requested       run_python {"code":"from datetime import date, timedelta\n..."}
# id: 5 / event: tool_succeeded       {"stdout":"2026-10-15\n","stderr":"","exit_code":0,"timed_out":false,...}
```

`make demo-python` does that. Watch the worker log for `sandbox run finished` lines with the
container id, exit code, and duration, or `docker ps` during a call to see the container with
its `agentd.run_id` label and no network. With `qwen2.5:7b` the recorded trajectory was: first
script imports `dateutil`, the stdlib-only sandbox returns exit 1 and the `ModuleNotFoundError`
traceback as data, the model rewrites it with `datetime` alone, gets `2026-10-15`, and calls
`finish`. That is the failing-script-as-result decision (ADR-14) doing its job.

### The crash demo

```bash
make migrate && make serve      # in one shell (or `make up` and stop the compose worker)
make crash-demo                 # in another
```

The script starts worker A with a 5-second lease, submits a run whose `agent_config` sets
`tool_delay_ms: 8000` so each tool call is slow enough to interrupt, waits for the second tool
call to start, `kill -9`s worker A, and starts worker B. The stream then shows worker B
completing the interrupted call and finishing the run, and the event log shows the first tool
call was never re-executed. It ends by printing the run's Jaeger link: both workers' spans are
in one trace, and the missing `agent.run.attempt` span is worker A's death — that trace is the
second waterfall in [M4](docs/MILESTONES.md#m4--control-and-visibility).

![crash demo](docs/crash-demo.gif)

To re-record: `brew install vhs`, then `vhs deploy/demo.tape` with Postgres and the API up.

## The viewer

`GET /` is a single-page trajectory viewer, three files compiled into the binary with `go:embed`.

It lists runs, opens one, and streams its trajectory: model turns, tool calls with their arguments
and results, tokens and cost against the budget, a cancel button, and a link to the run's Jaeger
trace.


## MCP

External tools come from [Model Context Protocol](https://modelcontextprotocol.io) servers over
stdio or streamable HTTP, through the official Go SDK. They implement the same `Tool` interface as
everything else, so the registry, schema validation, the allowlist, the envelope, the idempotency
ledger and the tracing all apply unchanged — the loop special-cases nothing.

```jsonc
// deploy/mcp.json — read by BOTH `serve` and `work`
{
  "servers": [
    {"name": "legal", "transport": "stdio", "command": "/usr/local/bin/some-mcp-server"},
    {"name": "docs",  "transport": "http",  "url": "http://127.0.0.1:9000"}
  ]
}
```

A discovered tool registers as `<server>__<tool>`, so a server can never shadow a builtin like
`finish`.  Both
processes read the same file because the API fills a run's default allowlist from its registry
while the worker dispatches through its own; they have to agree on names (ADR-34). An unreachable
server is a warning, not a fatal error.

The repo ships its own server, so none of this needs npx or the network:

```bash
go build -o /tmp/mcp-testserver ./internal/tools/mcp/testserver/cmd/mcp-testserver
```

## Endpoints

| Method | Path | |
|---|---|---|
| `POST` | `/v1/runs` | submit → `{id, status}`; body `{goal, agent_config?, max_steps?, budget_usd?}` |
| `GET` | `/v1/runs` | `{runs}`: newest first; `?status=` (validated against the enum) and `?limit=` (clamped) |
| `GET` | `/v1/runs/:id` | `{run, state}`: the row plus the reduced event log |
| `GET` | `/v1/runs/:id/events` | full event log as JSON |
| `GET` | `/v1/runs/:id/stream` | SSE tail, replays from `Last-Event-ID` |
| `GET` | `/v1/runs/:id/trace` | `{trace_id, url}`: the Jaeger deep link; 404 when the run has no trace |
| `POST` | `/v1/runs/:id/cancel` | cancel; interrupts an in-flight model or tool call within `-cancel-poll` |
| `POST` | `/v1/runs/:id/resume` | force-release the lease so any worker can pick the run up |
| `GET` | `/v1/tools` | registry listing with schemas and trust tiers |
| `GET` | `/healthz` | liveness |
| `GET` | `/metrics` | Prometheus exposition, including the Postgres-backed run gauges |
| `GET` | `/` | the trajectory viewer, embedded in the binary |

The worker serves `/metrics` and `/healthz` of its own on `-metrics-addr` (default `:9091`);
`agentd healthz -addr <host:port>` probes either, which is how compose health-checks a
distroless image with no shell in it.

`agent_config` fields: `model` (picks the provider too: `anthropic/<id>`, `local/<name>`, a
bare `claude-*` id, or anything else for the local runtime), `system_prompt`, `tools`
(allowlist; defaults to every registered tool, `run_python` included, and is fixed at
submission), `max_tokens`, `tool_delay_ms` (demo only). Unknown fields and unknown tool names
are rejected with 400. Registered tools: `compute_deadline`, `finish`, `search_corpus`, and
`fetch_document` (builtin), `run_python` (sandboxed), plus `<server>__<tool>` for every MCP
server in `-mcp-config` (external).

Corpus ingestion is an operator action, not an API call: it takes minutes and needs the
embedding runtime, so it ships as `agentd fetch` and `agentd ingest`. The API server lists and allowlists the corpus tools but cannot run them; only a worker has
the embedder and the store handle, so a search submitted to a worker without one comes back as
a retryable `tool_failed` rather than a wrong answer.

## Event log

| Type | Payload |
|---|---|
| `run_started` | goal, agent config snapshot, max_steps, budget, worker |
| `model_requested` | step, model, sha256 of the request, params |
| `model_responded` | step, provider, model, content blocks (text, tool_use, and any thinking blocks, kept verbatim), stop_reason, usage (with cache token counts), cost_micro_usd |
| `tool_requested` | tool_use_id, name, args (its seq keys the `tool_calls` ledger) |
| `tool_succeeded` | tool_use_id, name, result, duration_ms, exit_code, replayed, and any model spend the tool incurred inside itself (cost_micro_usd, input_tokens, output_tokens, cost_model) |
| `tool_failed` | tool_use_id, name, error, retryable |
| `budget_exceeded` | spent_micro_usd, budget_micro_usd, reason (`spent` or `would_exceed`), and for a pre-flight refusal the estimate that produced it (estimate_micro_usd, estimated_input_tokens, max_output_tokens) |
| `cancel_requested` | source, phase (`idle`, `model`, `tool`) |
| `run_finished` | status, final_answer, error |

`Reduce` rejects malformed logs (seq gaps, events after `run_finished`, results for tool calls
that were never requested) rather than tolerating them; a worker acting on a misread log is
worse than one that refuses. The M4 payload fields are all additive, so a log written before
them reduces unchanged.

The run's counters are the fold of the log, and since M4 that fold has two terms: `spent_usd`,
`input_tokens`, and `output_tokens` are `model_responded` **plus** `tool_succeeded` costs.
Summing only `model_responded` to check the counters will come up short by whatever the tools
spent (ADR-23).

## Retrieval numbers

`make eval-retrieval` runs every labeled query in all four modes and exits nonzero below the
thresholds in `evals/retrieval/labels.yaml`. The rows are the same code path with steps skipped,
so the `hybrid` row is literally what the tool does.

The first run reported BM25 recall@8 of **0.154**. That was not a weak baseline, it was a bug:
`websearch_to_tsquery` ANDs its terms, which is right for a search box and wrong for an agent that
sends a whole question. Same corpus, same labels, after the fix: **1.000**. That is the entire
argument for scoring retrieval separately from answer quality.

Against a real CourtListener corpus — 84 SCOTUS cases, 13,114 chunks, 40 labeled queries
(`evals/retrieval/labels-scotus.yaml`, `results-scotus.json`):

| mode | recall@8 | recall@20 | recall@50 | MRR | wall time |
|---|---|---|---|---|---|
| vector | 1.000 | 1.000 | 1.000 | 0.963 | 2.5 s |
| bm25 | 0.950 | 1.000 | 1.000 | 0.830 | 2.3 s |
| hybrid | 1.000 | 1.000 | 1.000 | **0.988** | 2.8 s |
| hybrid+rerank | 1.000 | 1.000 | 1.000 | 0.971 | 1,984 s |

**Two things in that table are worth more than the rest of it.**

**The LLM reranker makes ranking worse, and costs 713× the wall time to do it.** Hybrid's MRR is
0.988; reranking it drops it to 0.971, for 1,984 seconds against 2.8. This is a measured negative
and it is reported as one. [ADR-23](docs/DECISIONS.md) deferred a cross-encoder reranker on price;
this converts that from a price argument into a measured one, which is strictly stronger. A 7B
local model asked to re-order eight already-good hits mostly finds new ways to be wrong.

**Hybrid genuinely beats both of its components, but only on MRR.** 0.988 against vector's 0.963
and BM25's 0.830 — the fusion puts the right paragraph first more often than either input does.
Recall@8 cannot show this because it saturates, which brings us to the caveat.

**The corpus is 30× smaller than it should be, and recall@k saturates because of it.**
CourtListener's daily quota cut the pull off at 109 documents where the plan budgeted 2,500. Top-8
out of 84 documents is the top 9.5% of the corpus; the design assumed 0.3%. So `recall@8` is
1.000 for three of four modes for the same reason the 12-opinion fixture saturated, and **MRR is
the only column here that discriminates.** Reproduce with a corpus of a few thousand and the
recall columns start to mean something:

```bash
export COURTLISTENER_TOKEN=...
make fetch-corpus COURT=scotus LIMIT=2500   # ~30 min, resumable; has a daily quota
make dedupe                                 # collapse revisions of the same case (ADR-39)
make ingest CORPUS=data/corpus/scotus-dedup.jsonl   # ~11 chunks/sec; resumable by content hash
agentd eval retrieval -label "your query"   # prints top-20 hits to hand-label
make eval-retrieval
```

**Why `make dedupe` is in that sequence.** CourtListener publishes each revision of an opinion
under its own id: the 109 documents pulled covered only 84 distinct cases, and *Trump v. CASA*
appeared five times — 10.8% of the corpus by itself. Against the raw pull there is no honest
labeling, only a choice between two biases: name one id and its own revisions score as misses;
name all of them and a query gets five chances at top-8. Deduplicating removes the choice, which
is why the label file names exactly one relevant document per query. Note that content hashing
would not have caught any of this — all 109 texts differ, because a revision is a genuinely
different document.

**And the queries were written by a model, not a lawyer.** Each was drawn from its own opinion's
syllabus and never from search results, which is the anti-bias rule that matters most here. That
makes this a well-constructed benchmark, not a human-labeled gold set.

The 12-opinion fixture (`labels.yaml`, `results.json`) is still checked in and still runs in
seconds with no corpus pull. It saturates on every mode, but it catches real breakage — it is what
caught the AND-semantics bug.

Both results files carry the corpus size and the model names next to the numbers, so a table can
be traced to the run that produced it.

## Tests

```bash
make test                 # unit + testcontainers integration tests + sandbox tests; requires Docker
make test-short           # unit tests only
make test-sandbox         # the Docker executor tests and the SAFETY probes, verbose
make test-retrieval       # the pgvector-backed retrieval tests alone; Docker, no model
make test-live            # one real call to Ollama; requires the model pulled
make test-live-anthropic  # two real calls to Claude (a tool call, then the tool result
                          # with the thinking turn echoed back); needs ANTHROPIC_API_KEY
make eval                 # the agent eval scorecard, replayed from cassettes; Postgres only
make eval-retrieval       # recall@k and MRR per retrieval mode; needs Ollama
```

Nothing in `make test` calls Ollama. The retrieval tests use a deterministic hash-based fake
embedder, so a test corpus has real nearest-neighbour structure with no model anywhere: chunking,
RRF, HTML-to-text, the reranker's parser, and both tools are unit-tested, while ingest
idempotency, the two index queries, and the full search-read-cite trajectory run against real
pgvector. The real embedder and reranker are exercised by `make eval-retrieval` and
`make demo-legal`.


The Anthropic provider's unit tests point the real SDK client at an `httptest` server, so the
wire body is asserted exactly: thinking blocks echoed in order, tool schemas passed through with
every keyword, cache tokens priced. The loop's router test runs two fakes behind a `Router` and
checks each run is priced and labelled by the backend that answered it.

The sandbox tests build `agentd/sandbox:python` with the `docker` CLI once per test binary and
run real containers against the local daemon; the `run_python` tool and the loop are tested with
a scripted executor so the tool's behaviour (exit codes as data, timeout annotations, label
propagation, schema bounds) is covered without Docker.

## Layout

```
cmd/agentd/            # serve | work | migrate | fetch | ingest | eval
internal/api/          # handlers, SSE tail, config normalisation; ui/ (the embedded viewer)
internal/runtime/      # event payloads, Reduce, the loop, worker (claim/heartbeat/reaper)
internal/model/        # Provider interface, pricing, request fingerprint, Router; local/, anthropic/, fake/, cassette/ (record + replay)
internal/evals/        # suite parsing, the case runner and its chaos hook, the assertion vocabulary, citations, scorecard
internal/tools/        # Tool interface, registry + schema validation; builtin/, python/ (sandboxed), corpus/ (retrieval), mcp/ (external, + its in-repo test server)
internal/sandbox/      # Executor interface, Docker executor, limits, output capping, safety tests
internal/retrieval/    # Searcher + RRF; chunk/, embed/ (Ollama + fake), courtlistener/, rerank/, eval/
internal/store/        # Postgres access, fenced writes, ledger, corpus queries, embedded migrations
internal/telemetry/    # tracer setup, span helpers + names, the root-span ID generator, slog correlation, Prometheus instruments
internal/testutil/     # shared testcontainers Postgres fixture
evals/retrieval/       # checked-in fixture corpus, labeled queries, results.json
evals/cases/           # the agent suite: SMOKE, RETRIEVAL, CITATION, INJECTION, SAFETY, RESILIENCE, BUDGET
evals/corpus/          # poisoned.jsonl — the four planted attacks
evals/cassettes/       # one recorded trajectory per case
scripts/crash-demo.sh  # the kill -9 demo
scripts/demo-cancel.sh # cancel a run mid-tool-call and time it
scripts/compare-providers.sh  # one goal, both providers, trajectories side by side
docs/ARCHITECTURE.md   # how it works, as it stands
docs/DECISIONS.md      # ADRs
docs/SECURITY.md       # threat model
docs/MILESTONES.md     # the build journal
docs/plans/            # per-milestone plans
deploy/                # docker-compose.yml, Dockerfile, sandbox/Dockerfile, prometheus.yml
```



## Future work

Human-in-the-loop approval flows are out of scope for v1 (spec §2) and would slot in as an
`approval_requested` / `approval_granted` event pair that suspends the loop.

`POST /v1/corpus/ingest` is the one endpoint in the spec's API surface that is deliberately not
built. Ingest embeds, embedding is a model call, and the API server never calls a model — it writes
intent and tails the event log, which is the property that makes crash recovery mean anything. So
the endpoint could only ever have been a second job queue in front of `agentd ingest`, and that
queue would demonstrate nothing the runs API does not already demonstrate better over SSE: it needs
no lease fencing, because ingest is idempotent by content hash where a tool call is not. Ingest is
a CLI operation because of the invariant, not for want of a mechanism. Loading a corpus is an
operator action anyway.

A cross-encoder reranker is a second implementation of `rerank.Reranker`, deferred on price with
the numbers behind it — and now with a stronger reason to wait: the LLM reranking pass that exists
is a measured loss on the corpus we have, costing 713× hybrid's wall time to lower MRR (ADR-23).
Reranking is worth revisiting when the corpus is deep enough that recall@8 stops saturating, which
is the same condition that would make a cross-encoder worth pricing.

Measuring retrieval at real scale is the one open item with a number attached: CourtListener's
daily quota capped the corpus at 84 cases against the 2,500 planned, so the recall columns
saturate and MRR carries the result — see [Retrieval numbers](#retrieval-numbers).
