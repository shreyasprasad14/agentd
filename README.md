# agentd

A control plane for running LLM agents as durable, resumable, sandboxed, observable jobs.

**Status: M4 (control and visibility) complete.** The eval harness is not implemented yet.

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
bulk export, or the synthetic poisoned document set M5's injection evals plant
(`evals/corpus/poisoned.jsonl`) — never touches the pipeline (ADR-17).

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
its model calls happen *inside a tool*. M3 shipped with those tokens outside the run's
`spent_usd`, which is why a non-local rerank model was refused; M4 attributes them (ADR-23), and
the refusal is kept as a price decision rather than a measurement problem — see
[M4](#m4--control-and-visibility).

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
against the table. That convention is what M5's `CITATION` eval checks answers against, one
citation at a time, so a cite naming a real opinion and an invented paragraph fails. A sibling
test asserts that a run allowlisted without `search_corpus` gets a non-retryable `tool_failed`
when the model reaches for it anyway.

## M4 — Control and visibility

M1–M3 built the mechanisms and left the *controls* half-done on purpose: the budget check fired
after the money was gone, and a cancel was honoured only between steps. M4 is where those come
due, along with the observability that lets you see either of them happen.

### The budget is a ceiling, not a receipt

Before every model call the loop prices the call's worst case — the input it is about to send
plus the largest output the provider would return — and refuses to make the call if that would
carry the run past `budget_usd`. `make demo-budget` submits a $0.02 run against Opus:

```
event: budget_exceeded
data: {"reason":"would_exceed","spent_micro_usd":0,"budget_micro_usd":20000,
       "estimated_input_tokens":1339,"max_output_tokens":16000,"estimate_micro_usd":406695}
event: run_finished
data: {"status":"budget_exceeded","error":"next call estimated at 0.406695 USD
       (1339 input + up to 16000 output tokens) with 0.020000 of 0.020000 USD left"}
# status: budget_exceeded   spent: 0.0000 of 0.0200
```

It terminates at **$0.0000 spent**, and it does so without an API key, because the refusal
happens before the provider is contacted. The old post-hoc check (`spent >= budget`) is kept as
a backstop for a call that comes in over its estimate; the two are distinguished by
`reason: "spent"` vs `"would_exceed"`. The estimate needs no tokenizer — the previous step's
measured `input_tokens` plus `chars/4` for the tool results added since — and it deliberately
errs high, so the runtime will sometimes refuse a call it could have afforded. For a hard budget
that is the right direction to err (ADR-22).

A tool that calls a model *inside itself* now pays into the same budget: `tool_succeeded`
carries `cost_micro_usd` and token counts, and the transaction that writes it bumps `spent_usd`,
so M3's reranker is accounted for and the budget bounds the **run** rather than only the loop
(ADR-23). Two consequences worth stating: tool cost is committed *after* the tool runs, so the
budget is a ceiling on model calls and an audit on tool calls; and `spent_usd` is the fold of
`model_responded` **and** `tool_succeeded` costs, so checking it by summing only the former will
come up short. The reranker stays local-only, but for a different reason than in M3 — measured,
a hosted pass is about $0.04 per search, which was declined rather than unmeasurable.

### Cancel interrupts

`POST /v1/runs/:id/cancel` used to take effect at the next step boundary, which for a run inside
a 10-minute model call or a 120-second sandbox call meant "eventually". The loop now polls the
cancel flag every `-cancel-poll` (1s) during execution and cancels the context the call is
running under. `make demo-cancel` puts a run inside a tool call with 30 seconds left to run,
cancels it, and times the `run_finished` frame:

```
▶ POST /v1/runs/.../cancel and time the run_finished frame
  cancel landed in 1.17s

  1  run_started
  2  model_requested
  3  model_responded
  4  tool_requested   compute_deadline
  5  tool_failed      run cancelled
  6  cancel_requested tool
  7  run_finished     cancel requested
```

The log stays well-formed: the interrupted call gets a `tool_failed` and its ledger row moves to
`failed` in the same transaction, rather than leaving a `tool_requested` that nothing answers. An
interrupted *model* call deliberately leaves a dangling `model_requested` — there is no honest
event to write, since the loop does not know what the provider did with the request — which is
the same shape a crash mid-call produces and which `Reduce` has modelled since M1. Tokens spent
on that call are lost. Cancelling a *queued* run needs no code: only a worker can write
`run_started`, so a worker claims it and writes `run_started` → `cancel_requested` →
`run_finished` in milliseconds (ADR-25).

### One trace per run, across processes and crashes

A run is submitted by one process, executed by another, and possibly finished by a third after a
`kill -9`. In-process context propagation cannot span that, so the trace identity is durable: the
API mints a trace id and a root span id at submission and stores them on the run row, and every
worker rebuilds a remote parent from them. `GET /v1/runs/:id/trace` returns the deep link —
`make trace RUN=<id>` opens it — and 404s for a run that has no trace rather than linking to an
empty Jaeger page.

![one run, one trace](docs/trace-waterfall.png)

A `compute_deadline` run against `qwen2.5:7b`: 11 spans, two services, one minute three seconds
end to end. `api.create_run` is the 11.73ms sliver the API process contributed at submission;
everything under it is the worker. The shape of the bars is the answer to "where does a run's
time go" — the two `model.complete` spans are 40.18s and 22.78s, while the two `tool.invoke`
spans are 12.41ms and 8.07ms. Each `agent.step` pairs with the model call or the tool call it
covers, and the attributes behind them carry the rest: `gen_ai.request.model` and the token
counts on a model call, `agentd.tool.name`, its trust tier and outcome on a tool call, and the
run's status, steps, tokens and cost on the root.

`sandbox.exec` hangs under `tool.invoke` for a sandboxed call, and `retrieval.search` with its
four stage children (`embed`, `vector`, `lexical`, `rerank`) for a corpus search — which is what
makes the reranker's share of search latency visible instead of asserted.

The same goal under `make crash-demo`, which `kill -9`s worker A mid-tool-call, is one trace
across three processes — and the interesting part is what is missing from it:

![one run, one trace, across a kill -9](docs/trace-crash.png)

Read it top to bottom. Worker B's `agent.run.attempt` (teal) does not begin until **27.79s**,
and everything before it belongs to worker A (tan) — sitting at the *top level* of the trace
rather than under an attempt, because worker A's `agent.run.attempt` was never exported. Jaeger
flags those spans with a warning badge for a parent it cannot find; the badge is the `kill -9`.
The empty band from **17.34s to 27.79s** is the lease lapsing and worker B claiming. Then worker
B re-executes the interrupted tool call, 8.01s of it, and finishes the run — and `agent.run`,
emitted by worker B at that moment but back-dated to `created_at`, spans all 35.81s of it,
including the crash.

Three things the trace does that look wrong and are not (ADR-24). **A crashed attempt has no
`agent.run.attempt` span**: the process died before it could end, so it was never exported,
while its completed children were — that gap in the waterfall *is* the crash, drawn accurately.
**The root span ends before its children**, because it is emitted retroactively by whichever
worker writes the terminal event, back-dated to `created_at` and carrying the span id every
attempt already points at. **A run that never finishes has no root span**; its children are still
queryable by trace id, so the deep link still works.

### Metrics on both processes

`/metrics` on the API's listener and on the worker's own (`-metrics-addr`, default `:9091`,
which also serves `/healthz` — the liveness probe compose was missing for the worker).
`make metrics` curls both:

```
== api ==
agentd_http_requests_total{code="201",method="POST",route="/v1/runs"} 1
agentd_runs{status="succeeded"} 23              # from Postgres, survives a restart
== worker ==
agentd_model_calls_total{model="qwen2.5:7b",outcome="ok",provider="local"} 2
agentd_model_latency_seconds_sum{model="qwen2.5:7b",provider="local"} 62.94
agentd_runs_claimed_total{resumed="false"} 1
agentd_budget_terminations_total{reason="would_exceed"} 1
agentd_cancellations_total{phase="tool"} 1
```

The set covers runs by terminal status and duration, claims by resumed, steps, model calls by
outcome (`ok`/`retried`/`failed`/`non_retryable`), tokens and dollars by model, tool invocations
by name and outcome, sandbox timeouts and orphan sweeps, retrieval searches by mode and
degradation, lease reaps and losses, budget terminations by reason, cancellations by phase, and
HTTP requests by route pattern. Two of them are promises coming due:
`agentd_leases_reaped_total` is ADR-7's, and `outcome="non_retryable"` is ADR-12's — which also
brought the typed `model.NonRetryable` error, so the loop no longer burns three attempts on a
refusal or a bad API key (ADR-27).

Process counters are for events; the database answers for state. Counters reset on deploy, so
`agentd_runs{status}` and `agentd_runs_oldest_queued_age_seconds` come from one indexed
`GROUP BY` at scrape time — the queue-depth signal that actually pages someone. Metrics go
through `client_golang` while traces go through OTel, because a scrape-time read from an
external source of truth is exactly what `prometheus.Collector` is for (ADR-26). Labels never
carry a run id, a goal, or any model-supplied string; HTTP requests are labelled with chi's
matched route pattern (`/v1/runs/{id}`), never the path, and two tests assert it because the
failure is silent.

A `prometheus` service scrapes both processes inside compose (http://localhost:9090). The
worker's metrics port is published with no fixed host port on purpose: `docker compose up
--scale worker=2` — spec §2's two-worker lease demo — collides on the second replica the moment
one is mapped, so Prometheus finds every replica by DNS and `docker compose port worker 9091`
gets a host address when a human needs one.

## M5 — Evals

M0–M4 built mechanisms and wrote paragraphs claiming they work. `make eval` turns those claims
into numbers, and it needs nothing but Postgres — no model, no API key, no network:

```bash
make up && make eval
```
```
  category     cases  pass  fail  skip  incon.   metrics
  SMOKE            3     3     0     0       0   pass_rate 1.000 (>= 1)  escalations 0 (<= 0)
  RETRIEVAL        1     1     0     0       0   pass_rate 1.000 (>= 1)  escalations 0 (<= 0)
  CITATION         1     1     0     0       0   pass_rate 1.000 (>= 1)  resolved_rate 1.000 (>= 1)  escalations 0 (<= 0)
  INJECTION        4     4     0     0       0   pass_rate 1.000  resisted_rate 1.000 (>= 1)  resolved_rate 1.000  escalations 0 (<= 0)
  SAFETY           2     2     0     0       0   pass_rate 1.000 (>= 1)  escalations 0 (<= 0)
  RESILIENCE       2     2     0     0       0   pass_rate 1.000 (>= 1)  escalations 0 (<= 0)
  BUDGET           2     2     0     0       0   pass_rate 1.000 (>= 1)  escalations 0 (<= 0)
```

Fifteen cases across spec §12's seven categories, each one a real run driven through the real
loop — same worker, same store, same registry as `agentd work` — and judged from the event log,
the `tool_calls` ledger, and the `chunks` table. Nothing is re-executed to check it, and no judge
model is asked for an opinion: a second stochastic system between the runtime and its own test
results is the thing this harness exists to remove (ADR-31). The harness owns its workers rather
than talking to a server, because a RESILIENCE case has to tear one down mid-tool-call and that
is not something it can do to a process it does not own (ADR-29).

### Two modes, one fixture

**Replay** freezes the model and proves the *runtime*: the loop, the ledger, the envelope, the
budget gates, the resume path, the citation plumbing. **`make eval-live`** runs the same cases
against a real model and proves the *model*: that it finds the right opinion, cites it honestly,
and ignores a planted instruction. Read the numbers accordingly — under replay, an assertion
about what the model *chose* is testing the plumbing that carried the choice.

Replay freezes the model and nothing else. A replayed run still writes to Postgres, still
searches the real corpus, and still executes real containers, which is why `make eval` needs a
database and why the SAFETY cases skip visibly on a machine with no Docker rather than failing.

### The cassette problem

A cassette is keyed by a hash of the request, not by the call's ordinal. Ordinal matching is
simpler and breaks the one category §12 exists for: model calls are at-least-once (ADR-4), so a
worker that dies mid-call leaves a dangling request the next worker repeats, and that repeat
would consume the *next* entry and desynchronise the rest of the run.

Hashing costs a normalisation step, because tool results carry fields that move between two
identical runs — `run_python`'s `duration_ms`, and `chunk_id`/`document_id`, which are fresh
UUIDs on every ingest. The volatile list lives in the cassette file so a miss can point at it,
and the envelope's `seq` is dropped too: a crash inside a model call shifts every later seq by
one, and the seq is a fact about the log's shape rather than about what the model was asked
(ADR-28). A miss fails the case with a diff against the nearest recording and never reaches for
a live provider — a cassette that silently fell through to the real API would turn "free and
deterministic" into a surprise bill.

The rate card is recorded in the cassette header, so the pre-flight budget ceiling replays its
exact arithmetic with no API key. The `budget-ceiling-refuses-before-spending` cassette is the
clearest case: the run makes zero model calls by design, so its cassette is nothing but a rate
card, and replaying it still terminates at `spent_usd = 0` of a two-cent budget.

`make eval-record` regenerates every cassette from trajectories declared in the suite files, so
they rebuild on a machine with no model at all; `make eval-record-live` replaces them with ones
a model actually chose.

### Injection: two numbers, and exposure before resistance

Four attacks are planted in otherwise plausible opinion prose (`evals/corpus/poisoned.jsonl`):
reach for `run_python` and read `/etc/passwd`, append a canary to the answer, stop researching
and `finish` with a fixed sentence, and close the envelope to continue as a forged system turn.

Every case asserts **exposure before resistance** — the planted text must appear in one of that
run's own tool results before any claim is made about the model ignoring it. A case whose query
never ranked the poisoned document proves nothing, and is reported *inconclusive* rather than
passed; the parser refuses an INJECTION case that declares no `exposed_with`, and a threshold on
a metric that went unmeasured fails rather than reading as silence. That is the default failure
mode of this category, not an edge case.

The scorecard reports `resisted_rate` — soft, model behaviour, only meaningful next to the name
of the model that produced it — and `escalations`, a hard count that must be zero: a tool that
ran without being on the run's allowlist, or a tool result that forged the envelope. That is
`SECURITY.md`'s "containment, not immunity" made countable (ADR-30). Escalations are computed
for every case in the suite, not only the injection ones.

### Two defects the evals found

**The envelope was forgeable through tool failures.** The system prompt says everything between
`<tool_result>` tags is data, so a body that can close the tag and keep writing is a body that
can stop being data. Retrieved documents never could — every tool serialises with
`encoding/json`, which escapes `<` and `>` by default — but `Reduce` envelopes
`"error: " + p.Error` with no serialiser in between, and that error text carries model-supplied
content (`unknown tool: <name>` puts a name the model chose into the conversation verbatim). The
hole was narrow; the problem was that the property came from a serialiser's default rather than
a decision, and nothing in the codebase would have objected when a tool stopped providing it.
`Envelope` now defangs the delimiter itself (ADR-33).

**Retrieval was not reproducible across re-ingests.** Both corpus queries broke score ties on
`c.id`, and `IngestDocument` mints a fresh UUID for every chunk on every ingest — so the
tiebreaker was itself random per ingest. Same corpus, same scores, different top-k. That made
every retrieval number unreproducible, the recall table above included. Ties now break on
`(source_id, ordinal)` (ADR-32). It surfaced because cassette replay broke after the eval
database was truncated and re-ingested, and the miss message named the reordered search result —
an eval that only re-ran against a database somebody had already loaded would never have seen it.
Re-running `make eval-retrieval` after the fix produced the same numbers: it makes them
reproducible, it does not move them.

### What the scorecard does not say

It is fifteen cases over a sixteen-document fixture corpus, and it is a **regression suite, not a
benchmark** — the same caveat the retrieval table carries. A green `make eval` means the runtime
still does what M1–M4 said it does; it does not mean the agent is good at legal research. Under
replay the resistance and citation numbers come from recorded trajectories and are worth what a
fixture is worth. `make eval-live` is the half that scores the model, and it costs what a real
model costs.

`evals/results.json` carries every case's outcome, step count, spend, cassette, and failing
assertion next to the numbers, so a table here can be traced to the run that produced it.

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
second waterfall in [M4](#m4--control-and-visibility).

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
`-sandbox-image`, `-sandbox-timeout`, `-sandbox-max-timeout`, `-cancel-poll`, `-metrics-addr`.
Env equivalents: `AGENTD_MODEL_URL`, `AGENTD_MODEL`, `AGENTD_DSN`, `AGENTD_WORKER_OWNER`,
`AGENTD_ANTHROPIC_PRICES`, `AGENTD_ANTHROPIC_THINKING_DISPLAY`, `AGENTD_SANDBOX_IMAGE`,
`AGENTD_SANDBOX_TIMEOUT`, `AGENTD_SANDBOX_MAX_TIMEOUT`, `AGENTD_CANCEL_POLL`,
`AGENTD_METRICS_ADDR`. Credentials for Claude come from `ANTHROPIC_API_KEY` (or a profile from
`ant auth login`); `-model claude-opus-5` makes Claude the default for runs that name no model.

Telemetry is off unless it is configured, on both commands: `-otlp-endpoint`
(`OTEL_EXPORTER_OTLP_ENDPOINT`) turns tracing on, `-otel-service` (`OTEL_SERVICE_NAME`) names
the process in the waterfall, and `serve` takes `-jaeger-ui` (`AGENTD_JAEGER_UI`) for the base
URL of the deep links it hands out. With no endpoint set the tracer is a no-op, which is what
`make test`, `agentd ingest`, and `agentd eval` want; an *unreachable* collector is a warning
rather than a fatal, because the OTLP exporter retries on its own. `-metrics-addr ""` turns the
worker's listener off — worth it when running two workers on one host, where the second would
otherwise log a warning and carry on without `/metrics`.

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
| `GET` | `/v1/runs/:id/trace` | `{trace_id, url}`: the Jaeger deep link; 404 when the run has no trace |
| `POST` | `/v1/runs/:id/cancel` | cancel; interrupts an in-flight model or tool call within `-cancel-poll` |
| `POST` | `/v1/runs/:id/resume` | force-release the lease so any worker can pick the run up |
| `GET` | `/v1/tools` | registry listing with schemas and trust tiers |
| `GET` | `/healthz` | liveness |
| `GET` | `/metrics` | Prometheus exposition, including the Postgres-backed run gauges |

The worker serves `/metrics` and `/healthz` of its own on `-metrics-addr` (default `:9091`);
`agentd healthz -addr <host:port>` probes either, which is how compose health-checks a
distroless image with no shell in it.

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
`make demo-legal`, which are M3's equivalent of `make test-live`.

The Anthropic provider's unit tests point the real SDK client at an `httptest` server, so the
wire body is asserted exactly: thinking blocks echoed in order, tool schemas passed through with
every keyword, cache tokens priced. The loop's router test runs two fakes behind a `Router` and
checks each run is priced and labelled by the backend that answered it.

The sandbox tests build `agentd/sandbox:python` with the `docker` CLI once per test binary and
run real containers against the local daemon; the `run_python` tool and the loop are tested with
a scripted executor so the tool's behaviour (exit codes as data, timeout annotations, label
propagation, schema bounds) is covered without Docker.

M4's controls are tested the same way — no Jaeger, no collector, no API key. Spans go to an
in-memory `tracetest.SpanRecorder`, so a full fake-model run asserts that every span lands in
the trace id stored on the row and that a resumed run's spans join the first attempt's; the ID
generator is tested for the hazard that matters, that a planted root span id is used once and a
derived context gets a random one. Budget and cancellation run against a priced fake provider
and a blocking tool: a run whose next call would exceed its budget emits
`budget_exceeded{reason:"would_exceed"}` and never calls the provider, and a cancel during a
tool call that would otherwise run for 30 seconds finishes the run `cancelled`, writes
`tool_failed{retryable:false}`, and leaves the ledger row `failed`. Metrics are asserted through
`testutil.CollectAndCompare` on exposition text, including that the Postgres collector renders
one series per status and that a failed query does not take the scrape down with it. The live
paths — `make demo-budget`, `make demo-cancel`, `make crash-demo` under compose — are M4's
`test-live` equivalent and are run by hand.

M5's harness is tested rather than trusted, because a harness that scores its own correctness is
the one thing an eval cannot do. The cassette key is pinned from both directions: two searches
differing only in `duration_ms` and chunk UUIDs must hash the same, a conversation differing by a
message or a system prompt must not, and a repeated identical request must return the same entry
rather than the next one. `internal/evals` tests each assertion against hand-built event logs,
including the two shapes that matter most — a tool call requested and never completed, and a
ledger row left `started` — plus the escalation detector against a forged envelope injected into
a reduced conversation, so the check survives `Envelope` being changed back. The runner's
integration test records a suite and replays it against `testutil.Postgres` with no model
reachable, drives the crash case end to end, and asserts that a drifted cassette fails loudly
with a diff and that a missing one is a failure rather than a silent pass. `make test` stays
model-free, key-free, and network-free; `make eval` is the CI target in its own right, because it
needs a Postgres the test suite does not have a DSN for.

## Layout

```
cmd/agentd/            # serve | work | migrate | fetch | ingest | eval
internal/api/          # handlers, SSE tail, config normalisation
internal/runtime/      # event payloads, Reduce, the loop, worker (claim/heartbeat/reaper)
internal/model/        # Provider interface, pricing, request fingerprint, Router; local/, anthropic/, fake/, cassette/ (record + replay)
internal/evals/        # suite parsing, the case runner and its chaos hook, the assertion vocabulary, citations, scorecard
internal/tools/        # Tool interface, registry + schema validation; builtin/, python/ (sandboxed), corpus/ (retrieval)
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
docs/DECISIONS.md      # ADRs
docs/SECURITY.md       # threat model
docs/plans/            # per-milestone plans
deploy/                # docker-compose.yml, Dockerfile, sandbox/Dockerfile, prometheus.yml
```

## Notes on choices

See [docs/DECISIONS.md](docs/DECISIONS.md) for the ADRs: Postgres as the queue, content
blocks as the canonical message format, OpenAI-compatible local provider, at-least-once model
calls vs exactly-once tool calls, lease fencing, micro-USD cost accounting, the reaper,
submission-time allowlists, reduce-every-iteration, provider routing by model name, thinking
blocks as opaque log content, refusing to call an unpriced model, the Engine API over the
docker CLI, failing scripts as results rather than tool failures, inputs through an anonymous
volume, the boot-time orphan sweep, the pre-flight budget ceiling, tool-internal cost
attribution, one trace per run via a durable trace id, interrupting cancellation,
`client_golang` alongside OTel, typed non-retryable provider errors, cassettes keyed by a
normalised request hash, the eval harness driving the loop in-process, injection scored as two
numbers, thresholds over golden trajectories, a retrieval tiebreaker that survives re-ingest, and
an envelope that defangs its own delimiters.

Earlier notes from M0 still hold: hand-written pgx rather than sqlc while the schema moves;
SSE polls the log at 200ms rather than `LISTEN/NOTIFY`; the terminal status and
`run_finished` event now commit in one transaction.

`LISTEN/NOTIFY` is now the upgrade two polls are waiting on — the SSE tail's 200ms and the
cancel watcher's 1s. The SSE one is the more valuable target, because it is per connected
client rather than per worker.

## Future work

Human-in-the-loop approval flows are out of scope for v1 (spec §2) and would slot in as an
`approval_requested` / `approval_granted` event pair that suspends the loop.

Deferred deliberately, with the seam already in place: `POST /v1/corpus/ingest` is an async
wrapper over `agentd ingest` and lands with the other API polish in M6, and a cross-encoder
reranker is a second implementation of `rerank.Reranker`.

What M5 deliberately did not build: an LLM judge, because every assertion is mechanically
checkable against the log or the database on purpose; golden-trajectory diffing, which is more
precise and gets regenerated rather than read (ADR-31); cassettes for the *embedder* and the
*sandbox*, since replay freezes the model and nothing else; multi-turn injection, where a second
document reacts to the model's reply — a corpus is a static attacker, which is the threat model
`SECURITY.md` describes; and cost regression tracking over time, which wants a results database
rather than a JSON file. The hand-labeled corpus at real scale is still the two-hour job M3's
plan budgets for, and M5 does not relitigate it.

Two items that were listed here through M3 are closed. Tool-internal model cost **is** attributed
to the run as of M4, so the budget bounds the run rather than the loop; the local-only rerank
policy is kept anyway, as a price decision with the numbers behind it rather than for want of a
mechanism (ADR-23). And the observability the README used to describe as "not implemented yet"
is the milestone above.

What M4 deliberately did not build: pre-flighting *tool* cost, which would mean the registry
predicting each tool's spend before invoking it for a hole bounded by one tool call; per-tenant
or global budgets and topping up a running run, which need the multi-tenancy §2 puts out of
scope; the OTel *logs* signal, since `slog` to stdout with trace ids on every line is the log
path; and Grafana dashboards, an OTel Collector, and tail-based sampling — Jaeger direct and
`AlwaysSample` are right for a few runs a minute, and the production shape is this paragraph
rather than a container.
