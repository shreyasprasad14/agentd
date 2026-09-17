# agentd

A control plane for running LLM agents as durable, resumable, sandboxed, observable jobs.

**Status: M6 complete.** The retrieval table is measured on a real corpus; read the corpus-size
caveat under [Retrieval numbers](#retrieval-numbers) before quoting the recall columns.

An agent run is an append-only event log, and the loop is a state machine driven by it. A worker
that dies mid-run is not a lost run: a new one folds the log and continues from the last committed
step, without re-executing the tool call that was already finished. Everything else here —
sandboxing, budgets, hybrid retrieval, tracing, the eval harness — is built on that one property.

- **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** — how it works, as it stands
- **[docs/DECISIONS.md](docs/DECISIONS.md)** — 41 ADRs: the tradeoffs, already made
- **[docs/SECURITY.md](docs/SECURITY.md)** — the threat model, including what is *not* defended
- **[docs/MILESTONES.md](docs/MILESTONES.md)** — the build journal, and the defects the evals caught

## What it does

| | |
|---|---|
| **Durability** | Event-sourced runs over Postgres; leases, fencing, an idempotency ledger. `kill -9` a worker mid-tool-call and the run continues, exactly once |
| **Isolation** | Every `run_python` call in a fresh container: no network, read-only rootfs, dropped capabilities, cgroup limits |
| **Tools** | One `Tool` interface over builtins, the sandbox, and **MCP servers** (stdio or HTTP), namespaced and allowlisted per run |
| **Retrieval** | Hybrid pgvector + BM25 with reciprocal rank fusion, over real court opinions. Measured per query type: BM25 clearly leads on case-name queries, no mode clearly leads on the others, and the optional LLM reranking pass is reported with its costs |
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
thresholds in the label file it scores. The rows are the same code path with steps skipped,
so the `hybrid` row is literally what the tool does.

The corpus is from the Caselaw Access Project: 2,484 SCOTUS merits opinions from 1988–2014,
70,736 chunks. Lexical search is Okapi BM25 computed in SQL (ADR-41). Two query sets are
measured against it.

### The 40-query benchmark (paraphrased questions)

`evals/retrieval/labels-scotus-cap.yaml`. Labels are document-level: a hit on any chunk of the
right opinion counts. These are the same 40 cases scored as the `paraphrase` category below.
Results are in `results-scotus-cap-stratified/stratified-paraphrase.json`.

| mode | recall@8 | recall@20 | recall@50 | MRR | wall time |
|---|---|---|---|---|---|
| vector | **1.000** | 1.000 | 1.000 | **0.942** | 6 s |
| bm25 | **1.000** | 1.000 | 1.000 | 0.927 | 35 s |
| hybrid | **1.000** | 1.000 | 1.000 | 0.908 | 36 s |
| hybrid+rerank | **1.000** | 1.000 | 1.000 | 0.870 | 2,513 s |

**Every mode finds every case, so this set no longer separates the modes.** Vector keeps the best
MRR, but its lead over BM25 is 0.015: about half a case out of 40. Recall saturates because each
query is written from its opinion's own statement of the question, so it carries that opinion's
distinctive language. That made the task easy regardless of corpus depth: growing the corpus 30×
did not break the saturation. The stratified set below was built to fix that.

**An earlier version of this table said fusion hurts because "BM25 degrades at depth". That was
wrong.** The lexical mode then ranked with `ts_rank_cd`, which ignores term rarity, so common
words like `v.` and `state` drowned out rare, distinctive ones. It scored 0.850 recall@8 and 0.608
MRR here, and fusion averaged that broken list into the vector ranking. Replacing only the
ranking with BM25 brought lexical to 1.000 / 0.927, and hybrid to 1.000 / 0.908 (ADR-41).

### The stratified set (query types that stress exact terms)

`evals/retrieval/labels-scotus-cap-stratified.yaml` adds 63 cases, in four categories, to the 40
paraphrases:
- **citation:** the query includes a statute or reporter citation.
- **case_name:** the query names a case that corpus opinions cite.
- **term_of_art:** the query is built around a legal term.
- **mixed:** a conceptual question that also names a citation or case.

Labels for these are **paragraph-level**: a hit must be the specific paragraph. So their recall
measures a harder task than paraphrase's, and the rows cannot be compared across that line.
**All 63 new labels are unreviewed drafts.** Full report, with document-level results, per-case
ranks and suspected label issues: [`REPORT.md`](evals/retrieval/results-scotus-cap-stratified/REPORT.md).

recall@8 / MRR:

| category | n | vector | bm25 | hybrid | hybrid+rerank |
|---|---:|---|---|---|---|
| paraphrase *(document-level)* | 40 | **1.000** / **0.942** | **1.000** / 0.927 | **1.000** / 0.908 | **1.000** / 0.870 |
| citation | 16 | 0.623 / 0.682 | **0.713** / 0.760 | 0.601 / 0.776 | **0.713** / **0.802** |
| case_name | 17 | 0.516 / 0.898 | **0.831** / 0.910 | 0.670 / **0.916** | 0.584 / 0.652 |
| term_of_art | 15 | 0.404 / 0.636 | 0.460 / 0.730 | 0.423 / **0.735** | **0.531** / 0.607 |
| mixed | 15 | 0.584 / 0.752 | 0.607 / 0.844 | **0.627** / 0.856 | **0.627** / **0.872** |

With 15–17 cases per category, a gap under about 1.5 cases' worth (lead × n) is not treated as a
difference.

- **BM25 is the clear winner on case names.** Its recall@8 beats hybrid by 0.161 (2.7 cases' worth)
  and vector by 0.315 (5.4), and it is never worse than hybrid in any case. This is the only lead
  in the table that is both large and consistent case by case.
- **Elsewhere there is no meaningful winner.** Citation and mixed are exact ties at the top, and
  term_of_art's spread is about one case.
- **Vector search is never ahead on the new categories.** It trails the best mode on MRR in all
  four, and on recall@8 in citation and case_name. On paraphrase it ties or leads.
- **Fusion dilutes BM25 where BM25 is strong.** On case_name, hybrid recall@8 (0.670) sits between
  its two inputs. RRF weights both lists by rank alone, so it cannot tell which one to trust for a
  given query (ADR-18). That is now a measured cost, no longer a hypothesis.
- **The reranker's MRR losses look like reshuffling within the right opinion.** On case_name its
  count of first-label-at-rank-1 falls from 15 to 8. But in 11 of the 13 cases where it demotes a
  label, its new top result is an unlabeled paragraph of a labeled opinion, and several of those
  read as on point. At document level it puts a relevant opinion first in 57 of 63 cases. This
  can't be scored as a loss until the draft labels are reviewed.

Reproduce:

```bash
make fetch-cap COURT=scotus LIMIT=2500   # ~85 s, no token, no quota (ADR-40)
make dedupe                              # collapse revisions of the same case (ADR-39)
make ingest                              # ~17 chunks/sec, ~70 min; resumable; refreshes BM25 statistics
agentd eval retrieval -label "your query"   # prints top-20 hits to hand-label
make eval-retrieval                      # the 40-query benchmark

go run ./cmd/evalstrat convert           # stratified labels -> chunk-ordinal label files, with mapping checks
for f in paraphrase citation case_name term_of_art mixed; do
  make eval-retrieval LABELS=evals/retrieval/generated/stratified-$f.yaml \
                      RESULTS=evals/retrieval/results-scotus-cap-stratified/stratified-$f.json
done
```

The stratified run takes about 8 hours on a laptop, nearly all of it in the reranker.

`results-scotus-cap.json` still holds the 40-query run from before the BM25 change: 0.850 / 0.608
lexical, 0.950 / 0.866 hybrid, 1.000 / 0.877 reranked. The partial stratified run on `ts_rank_cd`
is in `results-scotus-cap-stratified/baseline-ts_rank_cd/`. The previous CourtListener table is
kept in `results-scotus.json` with the labels that produced it.


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
cmd/agentd/            # serve | work | migrate | fetch | fetch-cap | dedupe | ingest | eval
internal/api/          # handlers, SSE tail, config normalisation; ui/ (the embedded viewer)
internal/runtime/      # event payloads, Reduce, the loop, worker (claim/heartbeat/reaper)
internal/model/        # Provider interface, pricing, request fingerprint, Router; local/, anthropic/, fake/, cassette/ (record + replay)
internal/evals/        # suite parsing, the case runner and its chaos hook, the assertion vocabulary, citations, scorecard
internal/tools/        # Tool interface, registry + schema validation; builtin/, python/ (sandboxed), corpus/ (retrieval), mcp/ (external, + its in-repo test server)
internal/sandbox/      # Executor interface, Docker executor, limits, output capping, safety tests
internal/retrieval/    # Searcher + RRF; chunk/, embed/ (Ollama + fake), caselaw/ + courtlistener/ (two corpus sources, one JSONL), rerank/, eval/
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

A cross-encoder reranker would be a second implementation of `rerank.Reranker`, and it is deferred
on price, with the numbers behind that. The LLM reranking pass that exists costs 60–70× hybrid's
wall time. On the stratified set it leads or ties on recall@8 in citation, term_of_art and mixed,
but trails on MRR in case_name and term_of_art. Most of that shortfall looks like reordering
paragraphs within the right opinion, which can't be scored until the draft labels are reviewed.

Open items, in the order they unblock each other:

1. **Review the 63 draft labels in the stratified set.** `REPORT.md` flags specific cases:
   paragraphs that every mode ranks above the labels, and a note that misses short-form
   citations. The per-category numbers, and whether the reranker's paragraph-level MRR loss is
   real, depend on this review.
2. **Score-aware fusion.** The mechanism behind fusion's losses is now measured, not guessed.
   With BM25 fixed (ADR-41), RRF still drags case-name recall@8 from BM25's 0.831 down to 0.670,
   because it weights both lists by rank alone (ADR-18). A fusion that knows which list to trust
   for a given query is the next experiment. The eval already reports both input lists
   separately, which is what makes that comparison possible.
3. **Tokenizer normalization.** `evals/retrieval/TOKENIZER_NOTES.md` lists the gaps: spaced
   `U. S. C.` vs compact, dropped `§`, OCR `(l)`. With BM25 they explain no individual miss, but
   the compact `u.s.c` query token never matches a labeled paragraph. This should be measured on
   its own, against the current numbers.
4. **Lexical latency.** BM25 in SQL takes about 1 s per query, 1.5× slower than `ts_rank_cd`.
   That doesn't matter under the reranker, but it is visible in `bm25` and `hybrid` modes. A native
   index is the fix if it becomes the complaint.
