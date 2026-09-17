# Architecture

An agent run is an append-only event log. The loop is a state machine driven by that log.

Everything else follows. If the process dies, a new worker replays the log and continues, because
the log is the only place run state lives. Nothing is held in a goroutine's memory that would be
lost with it.

```
                    ┌──────────────┐
   POST /runs  ───► │  API Server  │ ──► enqueue (Postgres)
   GET  /runs/:id/  │    (Go)      │
        stream (SSE)└──────┬───────┘
   GET  /        ───►      │ tails run_events          ┌──────────────┐
        (viewer)           │                           │ MCP servers  │
                           ▼                           │ stdio / HTTP │
                    ┌──────────────┐                   └──────▲───────┘
                    │   Postgres   │◄────── lease ──────┐     │
                    │  - runs      │                    │     │
                    │  - run_events│        ┌───────────┴─────┴────┐
                    │  - tool_calls│◄──────►│    Worker (Go)       │
                    │  - documents │        │    agent loop        │
                    │  - chunks    │        │    state machine     │
                    │    (pgvector)│        └────┬────────────┬────┘
                    └──────┬───────┘             │            │
                           │                     ▼            ▼
                           │              ┌──────────┐  ┌────────────┐
                           │              │  Model   │  │  Sandbox   │
                           │              │ Provider │  │  Executor  │
                           │              │ local /  │  │  (Docker)  │
                           │              │ anthropic│  └────────────┘
                           │              └──────────┘
                           ▼
                  ┌─────────────────┐
                  │ Jaeger (OTLP)   │  one trace per run
                  │ Prometheus      │  scraped from both processes
                  └─────────────────┘
```

**The API server never calls a model.** It writes intent and tails the event log; all execution
happens in workers.
## The event log

`run_events` is the source of truth, keyed `(run_id, seq)`, append-only. Run state is a pure fold
over it:

```go
func Reduce(events []Event) (State, error)
```

The worker calls `Reduce` on **every** iteration, not just on resume. The path a fresh worker
takes to pick up a crashed run is therefore the same path every step of every run takes — the
recovery path cannot rot, because it is the only path.

| Event | Carries |
|---|---|
| `run_started` | the agent config snapshot, including the allowlist |
| `model_requested` | step, model, messages hash, params |
| `model_responded` | content blocks, stop reason, usage |
| `tool_requested` | tool name, args, tool_use_id |
| `tool_succeeded` | result, duration, exit code, any tool-internal cost |
| `tool_failed` | error, whether retryable |
| `budget_exceeded` | spent, limit |
| `cancel_requested` | source |
| `run_finished` | terminal status, final answer |

Every event write is a transaction that also updates the run's derived counters, so the log and
the counters can never disagree.

## Durability

**Leases** answer "who owns this run". A worker claims with `SELECT ... FOR UPDATE SKIP LOCKED`,
takes a 60-second lease, and heartbeats it. A reaper returns lapsed leases to the queue, which is
how a `kill -9`'d run becomes claimable again.

**Fencing** answers "is this writer still the owner". Every write re-checks the lease inside the
same transaction, so a stalled worker that wakes up after losing its lease is refused rather than
corrupting a log another worker is now appending to.

**The idempotency ledger** answers "did this side effect already happen". Each tool call gets a
`tool_calls` row keyed by the seq of its `tool_requested` event; the result row and the
`tool_succeeded` event commit together. A crash mid-tool-call re-executes only that call. A
completed call is never touched again.

Model calls are deliberately the opposite. `model_requested` is written *before* the call, so a
crash mid-call leaves a dangling request that the next worker simply repeats. Model calls are
at-least-once and tool calls are exactly-once, because one is idempotent and the other may not be.

## Tools

One interface, four kinds of implementation, no special cases in the loop:

```go
type Tool interface {
    Name() string
    Description() string
    Schema() json.RawMessage   // validated before every Invoke
    TrustTier() TrustTier      // Builtin | Sandboxed | External
    Invoke(ctx, Invocation) (Result, error)
}
```

- **Builtin** — `finish`, `compute_deadline`, `search_corpus`, `fetch_document`. In-process.
- **Sandboxed** — `run_python`, in a fresh locked-down container per call.
- **External** — MCP servers, namespaced `<server>__<tool>`, discovered at boot.

The registry compiles each schema at registration, so a bad tool fails at boot rather than
mid-run, and validates every invocation against it before dispatch. The per-run allowlist is fixed
at submission and checked on every resolve.

**MCP.** Servers are declared in a config file that both `serve` and `work` read, because the API
fills a run's default allowlist from its registry and the worker dispatches through its own — the
two must agree on names (ADR-34). An unreachable server is a warning, not a fatal error; the
consequence is a `tool_failed` at dispatch rather than a silently granted capability.

## The sandbox

Every `run_python` call gets a fresh container: `--network=none`, `--read-only`, `--cap-drop=ALL`,
`--security-opt=no-new-privileges`, non-root, memory- and pid-capped, with an exec-mounted tmpfs
for scratch. Wall-clock is enforced by the worker's context with a `docker kill` fallback, and
stdout/stderr are truncated to a fixed byte budget before entering the context window.

A script that fails is a *successful tool call* whose result says it failed — the model sees the
traceback as data and can correct. A tool failure is reserved for the runtime being unable to run
the tool at all.

See [SECURITY.md](SECURITY.md) for what this stops and what it does not.

## Retrieval

Ingest is parse → chunk → embed → upsert, chunked on structure rather than fixed windows, with
enough metadata to cite a paragraph. It is idempotent per `source_id` by content hash, which is
what makes an interrupted ingest resumable for free.

Collapsing several records of the *same case* is a separate step, `agentd dedupe`, which runs
between fetch and ingest and writes a new corpus file rather than editing one. CourtListener
publishes each revision of an opinion under its own id, so a raw pull holds the same case many
times over; content hashing catches none of it, because a revision is a genuinely different
document. Keeping this out of the pipeline is deliberate — the key lives in source-specific
metadata, and a pipeline that silently drops a superseded opinion gives an operator no way to see
what went missing (ADR-39).

Query is:

1. vector top-k over pgvector HNSW
2. lexical top-k over a GIN-indexed `tsvector`, ranked by Okapi BM25 against corpus statistics
   that ingest refreshes (ADR-41)
3. fused with Reciprocal Rank Fusion
4. reranked to the final k by an LLM reranker
5. returned with document ids and paragraph ordinals, so the answer can cite

Ties break on a stable key rather than on `chunks.id`, which is regenerated on every ingest — a
detail that made top-k move between two loads of the same corpus until the evals caught it
(ADR-32).

## Prompt injection

The corpus is untrusted input, so tool results enter the context inside a labeled
`<tool_result tool=… seq=…>` envelope that the system prompt declares is never an instruction, and
the envelope defangs its own delimiters so a body cannot forge the boundary (ADR-33).

Tool *definitions* are a separate surface the envelope does not cover — see ADR-35 and
SECURITY.md. Both are measured by the `INJECTION` eval category rather than asserted.

## Observability

One trace per run, rooted at `agent.run`, with `agent.step`, `model.complete`, `tool.invoke` and
`retrieval.search` beneath it. The trace id is minted at submission and stored on the run row,
because a context does not survive the queue between processes — let alone a `kill -9` between two
attempts (ADR-24). Two workers' spans therefore land in one waterfall, and a crashed attempt shows
up as the span that never closed.

Prometheus counters are registered per-process rather than globally, and HTTP metrics label the
chi *route pattern*, never the path, so a run id cannot mint a time series per run.

## Control

**Budgets** are a pre-flight ceiling, not a post-hoc audit: the loop refuses a call it cannot
afford rather than reporting an overspend afterwards (ADR-22). Tool-internal model spend — the LLM
reranker inside `search_corpus` — is attributed to the run, so the budget bounds the run rather
than the loop (ADR-23).

**Cancellation** interrupts in-flight work rather than waiting for it to finish (ADR-25).

## The HTTP surface

```
POST   /v1/runs              submit
GET    /v1/runs              list, newest first, ?status= and ?limit=
GET    /v1/runs/:id          status + reduced state
GET    /v1/runs/:id/events   the full log
GET    /v1/runs/:id/stream   SSE, replays from Last-Event-ID
POST   /v1/runs/:id/cancel   cooperative cancel
POST   /v1/runs/:id/resume   force re-lease a stuck run
GET    /v1/runs/:id/trace    Jaeger deep link
GET    /v1/tools             registry listing, with trust tiers
GET    /healthz  /metrics
GET    /                     the trajectory viewer
```

SSE replays from `Last-Event-ID`, so a dropped connection resumes without losing or repeating a
step. The viewer is the client that proves it: three embedded files, no build step, mounted as the
router's catch-all (ADR-36).
