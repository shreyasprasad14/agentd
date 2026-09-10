# agentd

A control plane for running LLM agents as durable, resumable, sandboxed, observable jobs.

**Status: M3 (retrieval) complete.** Tracing and the eval harness are not implemented yet.

## M1 — Real loop + durability

An agent run is an append-only event log, and the loop is a state machine driven by it. The
worker loads a run's events, folds them with a pure `Reduce(events) State`, and acts on the
result: drain any open tool calls, then check cancel, step limit, and budget, then call the
model, then append what happened. It does this from the log on *every* iteration, so the path
a fresh worker takes to resume a crashed run is the same path every step of every run takes.

The model sits behind a `ModelProvider` interface with tool calling and token/cost accounting
built in. M1 ships the local provider, an OpenAI-compatible client that works against Ollama,
llama.cpp, or vLLM, with cost pinned at zero. The Anthropic provider (M1.5) plugs into the same
seam. Tools go through a registry that validates arguments against each tool's JSON Schema and
enforces the per-run allowlist fixed at submission. Two builtins ship: `compute_deadline` (date
math with weekend skipping) and `finish`, the explicit terminal tool. Tool results reach the
model inside a labeled `<tool_result>` envelope that the system prompt declares to be data,
never instructions.

Durability is three mechanisms. **Leases**: a worker claims a run with `FOR UPDATE SKIP
LOCKED`, takes a 60-second lease, and heartbeats it; a reaper returns lapsed leases to the
queue. **Fencing**: every write checks that the writer still holds the lease inside the same
transaction, so a stalled worker that wakes up after losing its lease is refused rather than
corrupting the log. **The idempotency ledger**: each tool call gets a `tool_calls` row keyed by
the seq of its `tool_requested` event, and the result row and the `tool_succeeded` event commit
together. A crash mid-tool-call re-executes only that call; a completed call is never touched
again. Model calls are the opposite, deliberately: `model_requested` is written before the
call, so a crash mid-call leaves a dangling request that the next worker simply repeats.

The integration suite runs against real Postgres via testcontainers. The headline test starts
a worker, lets it complete one tool call and begin a second, tears the worker down with no
cleanup (a `kill -9` from Postgres's point of view), starts a second worker, and asserts that
the run finishes, that the first tool ran exactly once, that the second ran twice, that no model
call was repeated, and that the second worker's model request contained the full rebuilt
conversation. A sibling test crashes mid-model-call instead. `scripts/crash-demo.sh` does the
same thing by hand against a real local model.

## M1.5 — Second model provider

Claude plugs into the same `ModelProvider` seam through the official Anthropic Go SDK, in
`internal/model/anthropic`. Because the canonical message format was already content blocks
(ADR-2), the translation is close to one-to-one; the loop, the reducer, and the event schema did
not change. What a hosted API adds over a local one is what M1.5 is really about:

- **Real money through the budget path.** The provider carries a price table (USD per million
  tokens, cache reads and writes included) and prices every response in micro-USD, so
  `spent_usd` moves and the `budget_exceeded` termination is exercised for real. A model with
  no price entry is refused before the call is made, rather than silently billed at $0 and
  allowed past its budget. Extra models are added with `-anthropic-prices`.
- **Thinking blocks round-trip through the log.** Current Claude models reason before they
  answer and return signed `thinking` blocks that must be sent back unchanged when a reasoning
  turn calls a tool. Those blocks are two new content block types stored verbatim in
  `model_responded`; the reducer passes them through and the provider echoes them. `-anthropic-
  thinking-display summarized` records readable summaries of the reasoning in the event log.
- **Refusals are errors, not answers.** A `stop_reason: refusal` fails the run with the policy
  category rather than finishing it with an empty final answer.

One worker serves both backends. A `model.Router` picks the provider from the model name:
`anthropic/claude-opus-5` or `local/qwen2.5:7b` explicitly, a bare `claude-*` name implicitly,
anything else goes to the local runtime. So `agent_config.model` on a run is the per-run
provider override the spec asked for, and `model_responded.provider` records which backend
actually answered. `make compare` submits one goal to both and diffs the trajectories.

## M2 — Sandbox

Every call to a sandboxed tool runs in a fresh container that is created, run, and destroyed
for that one call. The executor in `internal/sandbox` drives the Docker Engine API directly (no
`docker` CLI in the worker image) and applies the spec §7 flag set field for field: no network,
512 MiB with swap disabled, one CPU, 128 pids, a read-only root filesystem with a 64 MiB tmpfs
at `/tmp`, every capability dropped, `no-new-privileges`, uid 65534. Three things sit on top of
the flags because a flag cannot do them:

- **The worker owns the clock.** `ContainerWait` runs under a context deadline (30 s by default,
  120 s at most, per call). On expiry the worker sends `SIGKILL`, records exit code 137 and
  `timed_out: true`, and removes the container. If the worker itself is shutting down, the same
  kill-and-remove happens and the tool is re-executed by the next worker.
- **Output is capped as it streams.** stdout and stderr are demultiplexed off the attach stream
  into fixed 16 KiB buffers that count and drop the rest, so a script printing gigabytes costs
  the worker 32 KiB and the model sees `stdout_truncated: true`. The daemon's log driver is off
  for these containers, so the flood never reaches the host disk either.
- **Leftovers are swept.** Containers carry `agentd.run_id` and `agentd.seq` labels. A worker
  that is `kill -9`'d mid-call leaves a container with no supervisor; every worker removes
  labelled containers older than the maximum timeout at boot, and leaves younger ones (possibly a
  live sibling's) alone.

Inputs reach the container through an anonymous volume at `/work/in`, filled with
`CopyToContainer` before start and deleted with the container. The daemon refuses copies into a
read-only rootfs, and a bind mount would need a host path the worker cannot supply once it runs
in compose. The volume inherits the image's root-owned `0555` directory, so the sandboxed
process gets `EACCES` if it tries to rewrite its own script.

The `run_python` tool is the first sandboxed tool: `{code, stdin?, files?, timeout_seconds?}`
→ `{stdout, stderr, exit_code, timed_out, oom_killed, stdout_truncated, stderr_truncated,
duration_ms}`. A script that exits nonzero, is OOM-killed, or times out is a *successful* tool
call whose result says so: the model needs the traceback to fix the code, and a step limit
already bounds how many times it may try. Only a failure of the sandbox itself (daemon
unreachable, image missing) is a `tool_failed`. The exit code is recorded on the
`tool_succeeded` event, so evals can read it from the log. (The spec calls the tool `python`.
Ollama 0.33 reserves that name for its own builtin tool and silently drops any tool call to it,
whichever model is serving; renaming it was cheaper than arguing with the parser.)

Building this surfaced one loop fix: a model response with no text and no tool call, which is
exactly what an OpenAI-compatible server returns after dropping a tool call it could not parse,
used to finish the run as `succeeded` with an empty answer. It is now retried like a transport
error and fails the run with a clear message if it persists.

The safety tests in `internal/sandbox/safety_test.go` are the spec §12 `SAFETY` category run
against the real daemon, one hostile script per row:

| Probe | Observed |
|---|---|
| TCP to 1.1.1.1:80, DNS lookup, interface scan | `ENETUNREACH`, resolution failure, only `lo` up and no routes |
| Fork bomb | `fork()` refused with `EAGAIN` at the pids cap; container exits in ~200 ms, not at the deadline |
| Writes to `/`, `/usr`, `/etc`, `/work/in`; 100 MiB into `/tmp` | `EROFS`, `EROFS`, `EROFS`, `EACCES`; `ENOSPC` after 64 MiB |
| uid, capability sets, `NoNewPrivs`, `su root` | 65534; effective, permitted, and bounding sets all zero; 1; authentication failure |
| Allocate 2 GiB against a 512 MiB cap | OOM-killed, exit 137, `oom_killed: true`, nothing printed |
| `sleep 60` with a 2 s limit | killed at 2 s, exit 137, partial stdout kept, container gone |

`make test-sandbox` runs them on their own. The threat model, what each control stops, and what
the sandbox does *not* stop are in [docs/SECURITY.md](docs/SECURITY.md).

### Why Docker, and what a real deployment would use

Namespaces and cgroups are a policy boundary, not a hardware one: the sandboxed process shares
the host kernel, and a kernel exploit escapes everything above. The four realistic options trade
that gap against cost. **Docker as configured here** costs nothing extra, starts in ~100 ms, runs
any Linux payload, and relies on the default seccomp profile plus the flags above to keep the
kernel surface small. **gVisor** (`runsc`) interposes a user-space kernel, so most syscalls never
reach the host; it costs syscall-heavy workloads real throughput, but a script doing date
arithmetic and text processing will not notice, and it is a one-field change
(`HostConfig.Runtime`) with the same image and flags. **Firecracker** gives a true VM boundary
per call with cold starts in the low hundreds of milliseconds, at the cost of running a VMM
fleet and losing the Docker image and API conveniences. **WASM** has the strongest isolation
model of the four but the narrowest runtime; CPython under WASI is real but its standard library
coverage is not yet something to build a product on.

For a legal-tech deployment handling privileged client material, the choice is gVisor as the
default runtime, on a dedicated or rootless daemon so the socket the worker holds is not the
host's. The workload does not pay gVisor's tax, the operational model is unchanged, and it
closes the one gap that the flag set cannot. Firecracker becomes the answer when tenants share
hosts and the isolation boundary has to survive a kernel bug by construction rather than by
interposition. The path is written down in order in `docs/SECURITY.md`.

## M3 — Retrieval

A legal research question is answered from a real corpus of court opinions, with citations that
resolve to real rows, and the retrieval quality is measured rather than asserted.

The corpus arrives in two steps that stay separate on purpose. `agentd fetch` pulls opinions
from the CourtListener REST API into a JSONL file (one lead opinion per decision, HTML converted
to text, resumable, `Retry-After` honoured); `agentd ingest` chunks, embeds, and upserts that
file. Ingest reads *only* that format, so swapping the corpus source — a different court, the
bulk export, or a synthetic poisoned document set for M5's injection evals — never touches the
pipeline (ADR-17).

Chunking is structure first, size second. Paragraphs are the unit; a short line that is
numbered, all caps, or title case is a *section heading* and becomes a label carried by every
chunk beneath it rather than a chunk of its own, so a hit says `II. Analysis` without a second
lookup. Paragraphs merge to ~1,200 characters (about 300 tokens, inside `mxbai-embed-large`'s
512-token window with the query prefix), hard-capped at 1,800 with sentence-aligned splitting,
and each chunk carries the previous chunk's last sentence as overlap so a holding that straddles
a boundary is retrievable from either side (ADR-20). Ingest is idempotent per document in one
transaction, keyed on the content hash *and* the embedding model, so an interrupted corpus load
resumes and a model change re-embeds by itself rather than silently mixing vector spaces
(ADR-21).

Search is two indexed queries run concurrently — HNSW top 50 by cosine distance, GIN top 50 by
`ts_rank_cd` — fused with Reciprocal Rank Fusion in Go, then reranked. Fusion is Go rather than
one clever CTE because the eval needs the two lists separately to report per-mode numbers, and
because RRF needs no score normalisation between cosine distance and a text rank (ADR-18). The
reranker scores candidates pointwise through the existing `model.Provider`, which reuses the
local model already running instead of standing up a cross-encoder service (ADR-19). It degrades
rather than fails: a provider error or an unparseable batch returns the fused order and reports
`mode: "hybrid"`, because a reranker outage should cost recall, not the run.

Two things about that reranker worth saying out loud. It is **the slow step**: with `qwen2.5:7b`
on a laptop it is roughly 30 seconds per query against 50 candidates, against 0.3 seconds for
everything before it, which is why `-rerank-candidates` is a flag and `-rerank=false` exists. And
its model calls happen *inside a tool*, so their tokens are **not** counted in the run's
`spent_usd`. With a local model that is $0 and true; pointing it at Claude would spend real money
outside the budget, so a non-local rerank model is refused until M4 attributes tool-internal cost
to the run.

Two tools reach it, both builtin tier and read-only: `search_corpus` (query, `k`, optional court
and date filters) and `fetch_document` (by id or `source_id`, optionally a range of ordinals).
Results are bounded twice — chunks are ≤ 1,800 characters by construction, and each tool caps its
serialised result with a `truncated` flag — so a document cannot crowd the context window the way
an uncapped `stdout` could. Both tell the model to cite by `source_id` and paragraph `ordinal`,
and the default system prompt now says the same and that opinion text is quoted data, never
instructions. `docs/SECURITY.md` has the boundary in full.

### What the eval measured, and the bug it caught

`make eval-retrieval` runs every labeled query in all four modes and exits nonzero below the
thresholds in `evals/retrieval/labels.yaml`. The four rows are the same code path with steps
skipped, so the `hybrid` row is literally what the tool does.

The first run said BM25 recall@8 was **0.154**. That was not a weak baseline, it was a bug:
`websearch_to_tsquery` ANDs its terms, which is right for a search box and wrong for an agent,
because a model sends a whole question and requiring all ten lexemes to appear in one
1,200-character chunk matches essentially nothing. The lexical query now normalises the question
through the same dictionary that built the index and ORs the lexemes, leaving `ts_rank_cd` — a
cover-density rank, so proximity and phrase order already score higher — to discriminate. Same
corpus, same labels, recall@8 went **0.154 → 1.000**. That is the entire reason the milestone
has an eval instead of a paragraph asserting the search works.

Against the checked-in 12-opinion fixture, 13 labeled queries, `mxbai-embed-large` embeddings and
`qwen2.5:7b` reranking (`make ingest-fixture && make eval-retrieval`):

| mode | recall@8 | recall@20 | recall@50 | MRR | wall time |
|---|---|---|---|---|---|
| vector | 1.000 | 1.000 | 1.000 | 1.000 | 0.3 s |
| bm25 | 1.000 | 1.000 | 1.000 | 0.923 | 0.01 s |
| hybrid | 1.000 | 1.000 | 1.000 | 1.000 | 0.3 s |
| hybrid+rerank | 1.000 | 1.000 | 1.000 | 1.000 | 388 s |

Read that table for what it is: twelve well-known opinions with one query written per case is a
**smoke test**, not a benchmark. Every mode saturates because with twelve documents the right
answer is rarely outside the top eight of anything, and the only number that moves is BM25's MRR
— one query where the lexically-best chunk is not the one labeled. It is checked in because it
runs in seconds with no corpus pull and it does catch real breakage (it caught the AND-semantics
bug), but the numbers that discriminate between vector, BM25, and hybrid need a corpus with
thousands of near-neighbours, where the labeling is the two-hour job the plan budgets for:

```bash
make fetch-corpus COURT=scotus LIMIT=2500   # needs COURTLISTENER_TOKEN; resumable
make ingest
agentd eval retrieval -label "your query"   # prints top-20 hits to hand-label
make eval-retrieval
```

`evals/retrieval/results.json` carries every run's numbers next to the corpus size and the
embedding and rerank model names, so a table can be traced to the run that produced it.

### The retrieval demo

`make ingest-fixture` then `make demo-legal` submits a research question with only
`search_corpus`, `fetch_document`, and `finish` granted, so the model has to find the case,
read it, and cite it:

The recorded trajectory with `qwen2.5:7b`, verbatim:

```
# id:  3 / event: model_responded  (tool_use: search_corpus)
# id:  5 / event: tool_succeeded   search_corpus  mode=hybrid hits=3 top=clop-0002
# id:  7 / event: model_responded  (tool_use: fetch_document)
# id:  9 / event: tool_succeeded   fetch_document doc=clop-0002 chunks=1
# id: 11 / event: model_responded  (tool_use: finish)
# id: 14 / event: run_finished     "In Carpenter v. United States (clop-0002 ¶1), the Supreme Court
#                                   held that the Government's acquisition of historical cell-site
#                                   location records is a Fourth Amendment search..."
```

`clop-0002 ¶1` is a row in `chunks`, and its text is the sentence the answer paraphrases. The
loop integration test asserts that whole shape against pgvector with a fake model and a fake
embedder — search, then fetch, then finish, with the cited `(source_id, ordinal)` resolved
against the table — which is the convention M5's `CITATION` eval will check answers against. A
sibling test asserts that a run allowlisted without `search_corpus` gets a non-retryable
`tool_failed` when the model reaches for it anyway.

## Quickstart

You need Docker, Go 1.26, and [Ollama](https://ollama.com) on the host. For Claude, export
`ANTHROPIC_API_KEY` before `make up` (or `make work`); without it local runs are unaffected and
runs that target Claude fail with an authentication error.

```bash
brew install ollama && ollama serve &   # or the desktop app
make model-pull                         # ollama pull qwen2.5:7b and mxbai-embed-large
make up                                 # builds agentd/sandbox:python, then Postgres 16 (pgvector), Jaeger, api, worker
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
left off with `-H 'Last-Event-ID: 5'`.

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
call was never re-executed.

![crash demo](docs/crash-demo.gif)

To re-record: `brew install vhs`, then `vhs deploy/demo.tape` with Postgres and the API up.

## Running without compose

```bash
make migrate     # apply migrations
make serve       # API on :8080
make work        # worker, in a second shell; talks to Ollama on localhost:11434
```

Both `serve` and `work` apply migrations at startup (advisory-locked, so racing them is safe)
and retry the initial connection for 30s. Worker flags: `-model-url`, `-model`, `-lease`,
`-reaper-interval`, `-owner`, `-anthropic-prices`, `-anthropic-thinking-display`,
`-sandbox-image`, `-sandbox-timeout`, `-sandbox-max-timeout`. Env equivalents: `AGENTD_MODEL_URL`,
`AGENTD_MODEL`, `AGENTD_DSN`, `AGENTD_WORKER_OWNER`, `AGENTD_ANTHROPIC_PRICES`,
`AGENTD_ANTHROPIC_THINKING_DISPLAY`, `AGENTD_SANDBOX_IMAGE`, `AGENTD_SANDBOX_TIMEOUT`,
`AGENTD_SANDBOX_MAX_TIMEOUT`. Credentials for Claude come from `ANTHROPIC_API_KEY` (or a
profile from `ant auth login`); `-model claude-opus-5` makes Claude the default for runs that
name no model.

The worker finds Docker the way the CLI does: `DOCKER_HOST`, then the current `docker context`
(so Docker Desktop's per-user socket on macOS works without configuration), then
`/var/run/docker.sock`. In compose the socket is mounted into the worker container. If the
daemon or the sandbox image is missing at boot the worker logs a warning and keeps running;
`run_python` calls then fail as retryable `tool_failed` events until it is fixed.

## Endpoints

| Method | Path | |
|---|---|---|
| `POST` | `/v1/runs` | submit → `{id, status}`; body `{goal, agent_config?, max_steps?, budget_usd?}` |
| `GET` | `/v1/runs/:id` | `{run, state}`: the row plus the reduced event log |
| `GET` | `/v1/runs/:id/events` | full event log as JSON |
| `GET` | `/v1/runs/:id/stream` | SSE tail, replays from `Last-Event-ID` |
| `POST` | `/v1/runs/:id/cancel` | cooperative cancel; the worker finishes the run as `cancelled` |
| `POST` | `/v1/runs/:id/resume` | force-release the lease so any worker can pick the run up |
| `GET` | `/v1/tools` | registry listing with schemas and trust tiers |
| `GET` | `/healthz` | liveness |

`agent_config` fields: `model` (picks the provider too: `anthropic/<id>`, `local/<name>`, a
bare `claude-*` id, or anything else for the local runtime), `system_prompt`, `tools`
(allowlist; defaults to every registered tool, `run_python` included, and is fixed at
submission), `max_tokens`, `tool_delay_ms` (demo only). Unknown fields and unknown tool names
are rejected with 400. Registered tools: `compute_deadline`, `finish`, `search_corpus`, and
`fetch_document` (builtin), `run_python` (sandboxed).

Corpus ingestion is an operator action, not an API call: it takes minutes and needs the
embedding runtime, so it ships as `agentd fetch` and `agentd ingest`. `POST /v1/corpus/ingest`
(spec §13) is a thin async wrapper over the same code and lands with the other API polish in
M6. The API server lists and allowlists the corpus tools but cannot run them; only a worker has
the embedder and the store handle, so a search submitted to a worker without one comes back as
a retryable `tool_failed` rather than a wrong answer.

## Event log

| Type | Payload |
|---|---|
| `run_started` | goal, agent config snapshot, max_steps, budget, worker |
| `model_requested` | step, model, sha256 of the request, params |
| `model_responded` | step, provider, model, content blocks (text, tool_use, and any thinking blocks, kept verbatim), stop_reason, usage (with cache token counts), cost_micro_usd |
| `tool_requested` | tool_use_id, name, args (its seq keys the `tool_calls` ledger) |
| `tool_succeeded` | tool_use_id, name, result, duration_ms, exit_code, replayed |
| `tool_failed` | tool_use_id, name, error, retryable |
| `budget_exceeded` | spent_micro_usd, budget_micro_usd |
| `cancel_requested` | source |
| `run_finished` | status, final_answer, error |

`Reduce` rejects malformed logs (seq gaps, events after `run_finished`, results for tool calls
that were never requested) rather than tolerating them; a worker acting on a misread log is
worse than one that refuses.

## Tests

```bash
make test                 # unit + testcontainers integration tests + sandbox tests; requires Docker
make test-short           # unit tests only
make test-sandbox         # the Docker executor tests and the SAFETY probes, verbose
make test-retrieval       # the pgvector-backed retrieval tests alone; Docker, no model
make test-live            # one real call to Ollama; requires the model pulled
make test-live-anthropic  # two real calls to Claude (a tool call, then the tool result
                          # with the thinking turn echoed back); needs ANTHROPIC_API_KEY
```

Nothing in `make test` calls Ollama. The retrieval tests use a deterministic hash-based fake
embedder, so a test corpus has real nearest-neighbour structure with no model anywhere: chunking,
RRF, HTML-to-text, the reranker's parser, and both tools are unit-tested, while ingest
idempotency, the two index queries, and the full search-read-cite trajectory run against real
pgvector. The real embedder and reranker are exercised by `make eval-retrieval` and
`make demo-legal`, which are M3's equivalent of `make test-live`.

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
internal/api/          # handlers, SSE tail, config normalisation
internal/runtime/      # event payloads, Reduce, the loop, worker (claim/heartbeat/reaper)
internal/model/        # Provider interface, pricing, Router; local/ (OpenAI-compatible), anthropic/ (Claude), fake/ (tests)
internal/tools/        # Tool interface, registry + schema validation; builtin/, python/ (sandboxed), corpus/ (retrieval)
internal/sandbox/      # Executor interface, Docker executor, limits, output capping, safety tests
internal/retrieval/    # Searcher + RRF; chunk/, embed/ (Ollama + fake), courtlistener/, rerank/, eval/
internal/store/        # Postgres access, fenced writes, ledger, corpus queries, embedded migrations
internal/testutil/     # shared testcontainers Postgres fixture
evals/retrieval/       # checked-in fixture corpus, labeled queries, results.json
scripts/crash-demo.sh  # the kill -9 demo
scripts/compare-providers.sh  # one goal, both providers, trajectories side by side
docs/DECISIONS.md      # ADRs
docs/SECURITY.md       # threat model
docs/plans/            # per-milestone plans
deploy/                # docker-compose.yml, Dockerfile, sandbox/Dockerfile
```

## Notes on choices

See [docs/DECISIONS.md](docs/DECISIONS.md) for the ADRs: Postgres as the queue, content
blocks as the canonical message format, OpenAI-compatible local provider, at-least-once model
calls vs exactly-once tool calls, lease fencing, micro-USD cost accounting, the reaper,
submission-time allowlists, reduce-every-iteration, provider routing by model name, thinking
blocks as opaque log content, refusing to call an unpriced model, the Engine API over the
docker CLI, failing scripts as results rather than tool failures, inputs through an anonymous
volume, and the boot-time orphan sweep.

Earlier notes from M0 still hold: hand-written pgx rather than sqlc while the schema moves;
SSE polls the log at 200ms rather than `LISTEN/NOTIFY`; the terminal status and
`run_finished` event now commit in one transaction.

## Future work

Human-in-the-loop approval flows are out of scope for v1 (spec §2) and would slot in as an
`approval_requested` / `approval_granted` event pair that suspends the loop.

Deferred deliberately, with the seam already in place: `POST /v1/corpus/ingest` is an async
wrapper over `agentd ingest` and lands with the other API polish in M6; attributing a
reranker's model calls to the run's `spent_usd` needs a `cost_micro_usd` on `tool_succeeded`
and lands with M4's cost work, which is why M3 refuses a hosted rerank model rather than
spending outside a budget; a cross-encoder reranker is a second implementation of
`rerank.Reranker`; and planted-document injection tests are M5, which the JSONL ingest format
was shaped to make a one-line edit.
