# Decisions

Short architecture decision records. Each one is a tradeoff already made, so it can be
defended rather than improvised.

## ADR-1: Postgres is the queue

**Decision.** Workers claim runs with `SELECT ... FOR UPDATE SKIP LOCKED` on the `runs` table.
No Redis, no NATS.

**Why.** At this scale (a handful of workers, runs that live for seconds to minutes) a table
with a partial index *is* a queue, and it lives in the same transaction domain as the event
log. A claim, a lease renewal, and an event append can never disagree about which worker owns a
run, because they all go through one row lock. A separate broker would add a second source of
truth and a whole class of "queue says X, database says Y" bugs for no throughput we need.

**Revisit when.** Claim latency or poll load matters: `LISTEN/NOTIFY` first, a broker only if
runs need to be fanned out across many hosts.

## ADR-2: Content blocks are the canonical message format

**Decision.** The loop, the reducer, and the event log speak one message format: messages made
of `text`, `tool_use`, and `tool_result` content blocks, Anthropic-shaped. Providers translate
at the wire.

**Why.** The `model_responded` payload has to be provider-independent or the reducer would
have to know which backend produced each event. Choosing the richer format (blocks, explicit
tool-use ids) means the local provider does a lossless translation to chat-completions and the
Anthropic provider (M1.5) does almost none.

## ADR-3: The local provider speaks OpenAI-compatible chat completions

**Decision.** `internal/model/local` targets `/v1/chat/completions` with `tools`, not Ollama's
native `/api/chat`.

**Why.** Ollama, llama.cpp's `llama-server`, and vLLM all expose that endpoint with tool
calling and usage counts. One client, three runtimes, and the same shape a hosted
OpenAI-compatible endpoint would have if we ever pointed at one.

**Cost.** Some Ollama-only knobs (`keep_alive`, `num_ctx`) are unreachable from this API. None
are needed yet.

## ADR-4: Model calls are at-least-once, tool calls are exactly-once

**Decision.** `model_requested` is appended *before* the call and `model_responded` after. A
crash between them leaves a dangling request; the next worker just calls the model again. Tool
calls go through the `tool_calls` ledger keyed by the `tool_requested` event's `seq`, and the
ledger update and the `tool_succeeded`/`tool_failed` event commit in one transaction.

**Why.** A model call has no side effects, so repeating it costs money but never correctness.
A tool call can have side effects, so its result must be committed exactly once. Making the
ledger row and the event atomic means a resumed worker can never find a completed result
without its event, or an event without its result, and never re-executes a call whose result
committed.

**Boundary.** A crash *during* tool execution, before the commit, re-executes the tool. That is
at-least-once for the tool's side effects. Tools with real side effects receive the
`(run_id, seq)` key in their `Invocation` and must dedupe on it themselves; the builtins in M1
are pure, and the sandboxed `python` tool in M2 runs in an ephemeral container so a repeat is
harmless.

## ADR-5: Every write is fenced by the lease

**Decision.** Every worker write (`AppendEvent`, `AppendModelResponse`, `RequestToolCall`,
`CompleteToolCall`, `FinishRun`) runs inside a transaction that locks the run row and checks
`lease_owner = <this worker>` and `finished_at IS NULL`. If the check fails the write is refused
with `ErrLeaseLost`. The heartbeat also cancels execution the moment it sees the lease gone.

**Why.** Leases handle the dead-worker case; fencing handles the *paused* worker. A worker that
stalls past its lease (GC pause, network partition, a stuck tool) and then wakes up would
otherwise append events to a run another worker already owns, corrupting the log. With fencing
its next write fails and it walks away. This is the same fencing-token argument as in
distributed locks; the lease owner string is the token.

## ADR-6: Cost is carried as integer micro-USD

**Decision.** Providers return cost as `int64` millionths of a dollar. Postgres stores
`spent_usd` as `NUMERIC(10,4)` and the update is `spent_usd + $cost::numeric / 1000000`.

**Why.** Float dollars drift; a budget check that says `0.9999999 < 1.0` after a run that
actually spent `1.0000` is a bug nobody wants to chase. Integer arithmetic in Go and exact
numeric in Postgres means the counter and the sum of `cost_micro_usd` over the log always agree.

## ADR-7: The reaper exists even though `ClaimRun` already handles expired leases

**Decision.** `ClaimRun` claims queued runs *and* running runs whose lease has lapsed. A reaper
goroutine in every worker additionally flips lapsed runs back to `queued` on a timer.

**Why.** The claim clause is the fast path: recovery latency is the lease duration, not lease
plus reaper interval. The reaper makes the hand-off *visible*: a log line per requeued run now,
a Prometheus counter in M4, and a status the API can show as `queued` rather than a `running`
run with a stale owner. Both paths are idempotent, so running the reaper in every worker is fine.

## ADR-8: Tool allowlists are fixed at submission

**Decision.** `POST /v1/runs` validates `agent_config` strictly (unknown fields are rejected),
fills `tools` with every registered tool when omitted, rejects unknown tool names, and stores
the result. The worker never widens it.

**Why.** Spec §10: a hijacked agent must not be able to reach for a capability it was never
granted. Snapshotting the allowlist into `run_started` also means a replayed run sees the same
tools it originally had, which the cassette evals in M5 depend on.

## ADR-9: The loop reduces from the log every iteration

**Decision.** `Loop.Execute` re-reads the run's events and calls `Reduce` at the top of every
iteration rather than folding new events into an in-memory state.

**Why.** It is the same code path a fresh worker takes on resume, so the resume path is
exercised on every step of every run rather than only in the crash test. It removes an entire
class of "in-memory state drifted from the log" bugs, one of which the M1 test suite caught in
the incremental version. One indexed query per step is free at this scale.

## ADR-10: One worker, several backends, routed by model name

**Decision.** The worker's `Provider` is a `model.Router` over the local runtime and the
Anthropic API. `agent_config.model` selects the backend: an explicit `anthropic/…` or
`local/…` prefix, a bare `claude-*` id for Anthropic, anything else for local. The router
tags each response with the backend that answered (`Response.Provider`) and prices it by the
same resolution rule.

**Why.** The alternative, a `-provider` flag per worker process, makes provider a property of
the queue rather than of the run: a Claude run would sit behind local-only workers until one
with the right flag came along, and the spec's "per-run model override" would need a second
queue. Putting the choice in the model string keeps one queue, one worker binary, and one
snapshot (`run_started.agent_config`) that says exactly what a run was configured to talk to.
The only loop change is reading the provider name from the response instead of the interface.

**Cost.** The API cannot validate the model string at submission (it has no provider
knowledge, by design); an unroutable model fails at the worker with a clear error after the
loop's bounded retries.

## ADR-11: Thinking blocks are stored verbatim and never interpreted

**Decision.** `thinking` and `redacted_thinking` are content block types on the canonical
`ContentBlock`, with `thinking`, `signature`, and `data` fields. The reducer keeps them in the
assistant turn byte-for-byte; the Anthropic provider echoes them on the next call; the local
provider and the loop ignore them.

**Why.** Current Claude models reason before answering and return signed thinking blocks. When
a reasoning turn calls a tool, the next request must carry that turn back exactly, signature
and all, or the API rejects it. The event log is the only place the turn lives after a crash,
so the blocks have to be in the log. Storing them as opaque blocks rather than a provider-
specific side channel keeps `model_responded` provider-independent (ADR-2): the reducer's
fold does not know or care which backend produced the turn. It also means thinking summaries
(with `-anthropic-thinking-display summarized`) are in the trajectory for the M6 viewer for
free.

**Boundary.** The runtime never edits history: it only appends. That is exactly the shape the
API's signature checks assume, so nothing here has to change when those checks tighten.

## ADR-12: An unpriced model cannot be called, and a refusal is an error

**Decision.** The Anthropic provider refuses to call a model with no entry in its price table
(longest-prefix match on the model id, operator-extensible with `-anthropic-prices`). A
response with `stop_reason: refusal` is returned as `*anthropic.RefusalError`, not as a
`Response`.

**Why.** Budget enforcement is only as honest as the cost it sees. Pricing an unknown model at
$0 would let a run spend without limit, which is the exact failure the budget exists to
prevent; refusing up front is loud and cheap. Likewise a refusal has no tool calls and usually
no text, so surfacing it as a normal response would finish the run "succeeded" with an empty
answer. As an error it fails the run with the policy category in `run_finished.error`.

**Cost.** The loop retries every provider error three times, so a refusal or an unknown model
costs a few seconds and, for a refusal, up to three calls before the run fails. A typed
non-retryable error is a small M4 follow-up once the loop grows a metric for it.

## ADR-13: The sandbox drives the Engine API, not the `docker` CLI

**Decision.** `internal/sandbox.Docker` creates, attaches to, starts, waits on, kills, and
removes containers through the Docker Engine API (the `moby/client` module already in the
dependency graph via testcontainers). It never shells out to `docker`.

**Why.** The worker image is distroless: there is no shell and no CLI, and adding one for the
sake of `docker run` would add tens of megabytes and a second parser between the worker and
the daemon. The API gives structured errors, the attach stream (so output is capped in memory as
it arrives rather than after a `docker logs`), the exact exit code from `wait`, and `OOMKilled`
from inspect. It also makes location irrelevant: the worker on a laptop finds the daemon through
the CLI's current context, and the worker in compose finds it through the mounted socket, with
the same code path. Every §7 flag maps one-to-one onto a `HostConfig` field; the mapping table is
in `docs/plans/m2.md`.

**Cost.** The daemon socket is root-equivalent on the host. The worker is trusted infrastructure
and the payload never sees the socket, but a compromised worker process would own the machine.
`docs/SECURITY.md` says so and names the mitigations (rootless daemon, a socket proxy that
allows only the container endpoints, or a dedicated daemon for sandboxes).

**Revisit when.** A stronger boundary is needed: gVisor is `HostConfig.Runtime = "runsc"` with
the same image and flags, which is the main reason to prefer the API over hand-built `docker
run` strings.

## ADR-14: A failing script is a result, not a tool failure

**Decision.** `sandbox.Executor.Run` returns an error only when the sandbox itself failed
(daemon unreachable, image missing, create or start refused). A nonzero exit, a timeout, or an
OOM kill is a successful `Run` whose `Output` says what happened, and the `run_python` tool passes
that through as `{stdout, stderr, exit_code, timed_out, oom_killed, ...}`. The loop records it as
`tool_succeeded` with the exit code; only sandbox failures become `tool_failed{retryable:true}`.

**Why.** The model needs the traceback to fix the script, and it needs to know the difference
between "your code is wrong" (data, try again) and "the platform is broken" (an error the runtime
owns). Folding both into `is_error` would either hide the traceback or teach the model to retry
an outage. The step limit still bounds how many times a model can fail at the same script.

**Boundary.** `exit_code` in `tool_succeeded` is the sandbox's; the M5 SAFETY and SMOKE evals
read it from the log without re-running anything.

## ADR-15: Inputs enter the sandbox through an anonymous volume

**Decision.** The script and any input files are packed as a root-owned, mode 0444 tar and
copied with `CopyToContainer` into an anonymous volume mounted at `/work/in` on the
created-but-not-started container. Neither a bind mount nor a copy into the rootfs is used.

**Why.** The daemon refuses `docker cp` into a read-only rootfs (`container rootfs is marked
read-only`), and dropping `--read-only` to allow it would be the wrong trade. A bind mount needs a
path that exists on the Docker *host*, which the worker cannot provide once it runs in compose and
talks to the daemon over the socket. An anonymous volume is created by the daemon wherever the
daemon is, accepts the copy, inherits the image's root-owned `0555` directory so uid 65534 gets
`EACCES` on write (the safety test asserts this), and is deleted with the container
(`RemoveVolumes`). The read-only property comes from ownership rather than a mount flag, which is
weaker in principle and equivalent for an unprivileged process with no capabilities.

**Cost.** One volume create and remove per call, a few milliseconds on Docker Desktop.

## ADR-16: Every worker sweeps orphaned sandbox containers at boot, by age

**Decision.** Each sandbox container carries `agentd.sandbox`, `agentd.run_id`, `agentd.seq`,
and `agentd.tool` labels. When a worker starts it force-removes any `agentd.sandbox` container
older than the maximum sandbox timeout plus thirty seconds, and leaves younger ones alone.

**Why.** The wall-clock timeout lives in the worker, so a worker that is `kill -9`'d mid-call
leaves a container with no supervisor; the next worker re-executes the tool (ADR-4) and the
orphan keeps its CPU and memory until something notices. Age is the safe test: a container older
than the longest any call may run cannot belong to a live worker, and a younger one might belong
to a healthy sibling. Sweeping on boot rather than on a timer keeps it out of the hot path and
covers the case that matters (a restart after a crash) without a second reaper.

## ADR-17: The corpus comes from the CourtListener REST API, not the bulk export

**Decision.** `agentd fetch` pulls opinions through the REST v4 API (`/search/` filtered by
court and filing date, then `/opinions/{id}/` for the text), writing one JSON object per line.
`agentd ingest` reads only that JSONL.

**Why.** The bulk export is a multi-gigabyte opinions file plus separate clusters, dockets, and
courts files that have to be joined locally just to filter by jurisdiction. The API filters
server-side, pages by cursor, and a few thousand opinions is a few thousand requests, well
inside the authenticated rate limit. For a corpus the spec sizes at "one jurisdiction, a few
thousand opinions", the join is all cost and no benefit.

**Boundary.** The fetch/ingest split is where the corpus source is swappable: a different court,
the bulk CSVs, or a synthetic poisoned document set for the M5 `INJECTION` evals is a different
producer of the same JSONL, with nothing downstream changing. Planting a hostile document is a
one-line edit to a file.

**Cost.** Two requests per opinion and a free token in `COURTLISTENER_TOKEN`. Past a few thousand
documents the bulk path is the answer; the client honours `Retry-After` and resumes from an
existing output file, so an interrupted pull is restartable rather than restarted.

## ADR-18: Fusion happens in Go, not in SQL

**Decision.** `SearchVector` (HNSW, top 50) and `SearchLexical` (GIN + `websearch_to_tsquery`,
top 50) are two plain indexed queries that run concurrently, and Reciprocal Rank Fusion over
their results is twenty lines of Go.

**Why.** The eval needs the two lists *separately* to report vector-only and BM25-only rows, so
a single CTE that fuses internally would have to be written twice: once for the tool and once
for the eval. Two queries and a pure function means "hybrid" in the README table is literally
the same code the `search_corpus` tool runs, with the fusion step measurable in isolation and
testable without a database. RRF also needs no score normalisation between cosine distance and
`ts_rank_cd`, which is the thing that makes a SQL fusion query fiddly.

**Detail.** `k = 60`, ties broken by vector rank then chunk id so results are deterministic.
Queries set `hnsw.ef_search = 100` for the transaction: the index default of 40 starves a
top-50 candidate list.

## ADR-19: The reranker is an LLM through the existing provider seam

**Decision.** `rerank.Reranker` is the interface; the M3 implementation scores candidates
pointwise (0–10, batches of ten, temperature 0, JSON out) through the same `model.Provider` the
loop uses. No cross-encoder, no second inference server.

**Why.** There is no Go cross-encoder runtime, and llama.cpp's `/v1/rerank` means standing up
and operating a second server for one step of one tool. Pointwise scoring reuses the local model
that is already running, costs nothing, and sits behind an interface a cross-encoder can replace
without touching the searcher.

**Degradation is the important part.** Any provider error, or a batch whose output does not
parse, scores that batch zero and is logged; the fused RRF order is returned and the result says
`mode: "hybrid"` rather than `hybrid+rerank`. A reranker outage costs recall, never the run. The
eval refuses to *silently* accept this: a `hybrid+rerank` row that degraded is an error, because
a table row labelled rerank that actually measured hybrid is worse than no row.

**Cost, stated plainly.** The reranker's model calls happen inside a tool, so their tokens are
**not** counted in the run's `spent_usd`. With a local model that is $0 and honest. Pointing it
at Claude would spend real money outside the budget, so M3 refuses a non-local rerank model;
attributing tool-internal model cost to the run (a `cost_micro_usd` on `tool_succeeded`) is an
M4 item with the rest of the cost work.

## ADR-20: Chunk structure first, size second, with one-sentence overlap

**Decision.** Text is split into paragraphs; a short line that is numbered, all caps, or title
case is a *section heading*, carried as a label on every chunk beneath it rather than becoming a
chunk of its own. Paragraphs merge until 1,200 characters, hard-capped at 1,800 with sentence-
aligned splitting for oversize paragraphs, and each chunk is prefixed with the previous chunk's
last sentence (≤ 200 characters).

**Why.** Retrieval returns a *citable unit*, and in an opinion that unit is a passage under a
heading, not a fixed-width window. Carrying `II. Analysis` on every chunk under it means a hit
tells the model where in the opinion it is without a second fetch. The sizes are set by the
embedding model: 1,200 characters is about 300 tokens, comfortably inside
`mxbai-embed-large`'s 512-token window with the query prefix attached.

**Why overlap is one sentence and not more.** A holding that straddles a chunk boundary is
otherwise retrievable from neither side. One sentence is enough to carry it; more inflates the
index and, worse, makes duplicate hits for the same passage compete in the fused ranking.

**Boundary.** `Chunk(text)` is pure, and the tests assert the invariants that make ordinals
citable: contiguous `0..n-1`, `char_start`/`char_end` covering the input, nothing over the cap.

## ADR-21: Ingest is idempotent per document, in one transaction

**Decision.** Each document is upserted, its chunks deleted, and its new chunks inserted inside a
single transaction. A document is skipped when its `content_sha256`, chunk count, *and* the
embedding model recorded on its chunks all match; `-force` overrides.

**Why.** Ingesting a real corpus takes minutes and will be interrupted. Per-document atomicity
means a crash leaves every committed document whole and every unstarted one absent, so
re-running finishes the rest instead of starting over or, worse, leaving a document with half
its chunks embedded and silently under-retrievable.

**Why the embedding model is part of the key.** The stored vector is only meaningful next to
the model that produced it; a corpus half-embedded by `mxbai-embed-large` and half by `bge-m3`
returns nonsense rankings with no error anywhere. Recording `embedding_model` per chunk row
makes "which model produced this vector" answerable, and makes switching models a re-ingest that
the tool performs by itself rather than an operator remembering to pass `-force`.
