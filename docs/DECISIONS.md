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
plus reaper interval. The reaper makes the hand-off *visible*: a log line per requeued run,
`agentd_leases_reaped_total` since M4, and a status the API can show as `queued` rather than a
`running` run with a stale owner. Both paths are idempotent, so running the reaper in every
worker is fine.

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

**Cost, and how it was paid down.** The loop used to retry every provider error three times, so
a refusal or an unknown model cost a few seconds and, for a refusal, up to three calls before
the run failed. M4 typed these errors (`model.NonRetryable`, ADR-27) and the loop now returns on
the first one, counting it as `agentd_model_calls_total{outcome="non_retryable"}` — the metric
this ADR was waiting for.

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

**Cost, stated plainly — amended by ADR-23.** The reranker's model calls happen inside a tool.
As of M4 their tokens *are* counted in the run's `spent_usd`: `tool_succeeded` carries
`cost_micro_usd` and the token counts, and the transaction that writes it bumps the run's
counters, so the budget bounds the run rather than only the loop. The local-only `-rerank-model`
restriction is kept all the same, and its justification changes: not "we cannot measure this"
but "we measured it and declined the spend" — roughly $0.04 per search at Sonnet-class rates,
about 20% on top of a six-step research run. ADR-23 has the numbers and what they buy.

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

## ADR-22: The budget is a pre-flight ceiling, not a post-hoc audit

**Decision.** Before each model call the loop prices the call's *worst case* — the input tokens
it is about to send plus the maximum output the provider would allow — and refuses to make the
call when `spent + estimate > budget`. The old check (`spent >= budget`, after the fact) stays
as a backstop. `budget_exceeded` carries a `reason` of `would_exceed` or `spent`, and the
estimate that produced it.

**Why.** The post-hoc check can only notice money that is already gone. One call with a large
context and a 16k `max_tokens` can overshoot a $0.05 budget by more than the budget itself, and
"the run stopped once it had spent 7× its limit" is not a limit. `make demo-budget` is the
proof: a $0.02 budget against Opus terminates at `spent_usd = 0.0000` with an estimate of
$0.406695 in the event, without an API key, because the refusal happens before the provider is
ever contacted.

**What it gives up.** The runtime will sometimes refuse a call it could have afforded, because
it prices the worst case and most calls do not produce their maximum output. For a *hard*
budget that is the right direction to err — the alternative failure mode is a bill. The
estimate also ignores prompt-cache reads, which are cheaper, so it errs high there too.

**The estimate needs no tokenizer.** The previous step's `input_tokens` is a real number the
provider measured; the only thing added since is the tool results now in the message list,
whose size is known exactly. So the estimate is `LastInputTokens + newChars/4`, and on step one
a plain `chars/4` over the system prompt, goal, and tool schemas. Shipping a tokenizer per
provider to sharpen a number that is deliberately pessimistic would be the wrong trade.

**Tool spend is audited, not pre-flighted.** A tool that calls a model inside itself (ADR-23)
commits its cost after the call, so a single tool call can overshoot. Pre-flighting it would
mean the registry predicting each tool's cost before invoking it, which is not worth building
for a hole bounded by one tool call's spend. The budget is a *ceiling* on model calls and an
*audit* on tool calls, and the README says so in those words.

## ADR-23: Tool-internal model cost is attributed to the run

**Decision.** `tools.Result` carries a `Cost` struct (micro-USD, input and output tokens, the
model's name). `tool_succeeded` carries those fields, and the transaction that writes the event
bumps `spent_usd` and the token counters — the same guarantee, in the same shape, that
`model_responded` has had since M1. `Reduce` folds tool cost into `SpentMicroUSD`, so the budget
gates on it.

**Why.** M3's reranker makes model calls *inside* `search_corpus`, so its tokens never reached
`spent_usd`. That is a soundness hole rather than a cost question: the budget stopped bounding
the *run* and started bounding only the *loop*. M3 plugged it with policy — refuse any rerank
model that routes to a paid provider — which is sound while tool-internal spend is exactly $0,
and silently unsound the moment any future tool calls a paid model. M6's MCP adapter will expose
arbitrary third-party tools; some of them will.

**The policy is kept anyway, and its justification changes.** `-rerank-model` still refuses
anything that routes to a paid provider. Until now the reason was "we cannot measure this".
Measured, a hosted reranker costs roughly 11,000 input and 500 output tokens per search — about
$0.04 at Sonnet-class rates, or ~20% on top of a six-step research run, so three searches is
roughly +65%. That is affordable, and it is still declined: runs stay cheap by construction and
the restriction needs no per-run reasoning. What is unclaimed is the latency win — the local
reranker is 10–30s per search, the slowest step in a research run, where a hosted model with
`Parallel: 4` would finish in seconds. Revisit when search latency is the complaint.

**Amended 2026-09-16: the reranking step itself is now a measured loss, not just a priced one.**
On the 84-case SCOTUS corpus (`evals/retrieval/results-scotus.json`), `hybrid+rerank` takes 1,984
seconds against hybrid's 2.8 — 713× — and *lowers* MRR, 0.988 → 0.971. So the question this ADR
was arguing about, local versus hosted, is downstream of a better one: on this corpus the rerank
pass should not run at all. A 7B model re-ordering eight already-good hits mostly finds new ways
to be wrong. That does not retire the mode — a corpus deep enough for recall@8 to stop saturating
is exactly where reranking could start to pay, and this one is quota-capped at 30× under target —
but it does mean **no claim that reranking improves retrieval is currently supported by
measurement, and the README says so.**

**Consequence worth knowing.** `spent_usd` is now the fold of `model_responded` *and*
`tool_succeeded` costs. Anyone checking the counters by summing only `model_responded` will get
a mismatch — the invariant "the counters equal the fold of the log" still holds, but the fold
has two terms. This amends ADR-19's cost paragraph, whose open item is now closed.

## ADR-24: One trace per run, via a durable trace id

**Decision.** `POST /v1/runs` mints a trace id and a root span id and stores both on the run
row. Every worker that claims the run rebuilds a remote parent from them, so its spans land in
that trace whichever process, and whichever attempt, produced them. The worker that writes the
terminal event emits the `agent.run` root span retroactively: back-dated to `created_at`, ended
now, carrying the stored span id that every attempt already points at. `GET /v1/runs/:id/trace`
returns the deep link, and 404s for a run that has no trace rather than inventing one.

**Why not span links.** The idiomatic OTel answer for work that crosses a queue is a span
*link* between separate traces. Spec §11 asks for one trace per run and §13 for a single deep
link, and two traces joined by a link delivers neither: the crash demo would be two waterfalls,
and `/trace` would have to pick one. In-process context propagation cannot span a queue, let
alone a `kill -9`, so the identity has to be durable — and a column is the only durable place
it can live.

**Why the root span is emitted last.** A span is exported when it *ends*, so a root held open
for the length of a run is lost to exactly the crash it exists to illustrate. Emitting it at
submission instead would end it before the run began, carrying neither the real duration nor
the final status. Setting a chosen span id needs a custom `sdk/trace.IDGenerator` that reads an
id planted on the context — a documented extension point, about thirty lines.

**The hazard, stated so it is not rediscovered.** The `IDGenerator` is a provider-wide hook
consulted for *every* span. If the context carrying the planted id leaked past the single
`tracer.Start` it was made for, several spans would share one span id and the trace would be
corrupt. The planted context is used for one `Start` and never passed downward; a unit test
asserts that a second `Start` on a derived context gets a random id.

**Three things the trace does that look wrong and are not.** A crashed attempt has no
`agent.run.attempt` span, because the process died before it ended — its completed children
appear with a missing parent, and that gap *is* the crash, drawn accurately. The root span ends
before its children do, because it is emitted last and timestamped from `created_at`. A run
that never finishes has no root span at all; its children are still queryable by trace id, so
the deep link still works.

## ADR-25: Cancellation interrupts in-flight work

**Decision.** `Loop.Execute` derives its own context and runs a watcher that polls
`runs.cancel_requested` every `-cancel-poll` (1s). When the flag is set the watcher closes a
channel and cancels the context, so the in-flight `provider.Complete` or `tool.Invoke` returns
immediately. The channel is closed *before* the cancel, so the unwind can tell a cancel from a
shutdown from a lost lease — three causes that all reach the loop as `context.Canceled`.

**Why a poll, and why 1s.** The heartbeat already writes to that exact row every `lease/3`
(~20s), so `UPDATE … RETURNING cancel_requested` would give cancel detection for no additional
query at all — but it would cap cancel latency at ~20s. The dedicated poll buys sub-second
latency for one indexed single-row read per second per *running* run, and `claimAndExecute` is
sequential, so that is 1 qps per worker. Cancel latency is a number `make demo-cancel` prints:
measured, 0.7–1.2s against a tool call that had 30 more seconds to run. `LISTEN/NOTIFY` is the
real upgrade and would also retire the SSE tail's 200ms poll — the more valuable target, since
that one is per connected client rather than per worker.

**The load-bearing part is not the interval.** It is making the three reasons explicit. Before,
the worker inferred them from `ctx.Err()` and from `finish` happening to fail with
`ErrLeaseLost`, which made a cancelled run and a shutting-down worker indistinguishable.
`Execute` now returns an `Outcome`, and `agentd_cancellations_total{phase}` says where the
interrupt landed — a cancel counted in `idle` would mean the run really stopped at a step
boundary, which is the behaviour this ADR replaced.

**Keeping the log well-formed.** An interrupted tool call is answered with `tool_failed`
(`"run cancelled"`, not retryable), written under a context the interrupt cannot reach, and its
ledger row moves to `failed` in the same transaction. Both ways a call can be interrupted go
through one function: during the tool's own work, and during the artificial `tool_delay_ms`
between the request and the call — which is the wider window, and the one the demos land in. An
interrupted *model* call deliberately leaves a dangling `model_requested`: there is no honest
event to write, because the loop does not know what the provider did with the request. That is
the same shape a crash mid-call produces, which `Reduce` has modelled as `ModelInFlight` since
M1 — and the tokens that call spent are lost, exactly as they are for an empty response.

**Cancelling a queued run needs no code.** The event log requires `run_started` first, and only
a worker can resolve the agent-config snapshot that goes in it. So a queued cancel is claimed by
a worker, which writes `run_started` → `cancel_requested` → `run_finished` in milliseconds. The
API could not shortcut this without writing an event it has no snapshot for.

## ADR-26: `client_golang` for metrics, OpenTelemetry for traces

**Decision.** Traces go through the OTel SDK; metrics go through `prometheus/client_golang`,
with a registry per process — no package-level instruments — injected into the loop, the worker,
the sandbox, the searcher, and the API. The API additionally registers a `prometheus.Collector`
that answers `agentd_runs{status}` and `agentd_runs_oldest_queued_age_seconds` from Postgres on
each scrape.

**Why two libraries.** The unusual half of this problem is that Postgres-backed gauge — a
scrape-time read from an external source of truth — and `prometheus.Collector` is the
abstraction built for exactly that. The event half is commodity in either library. Two
supporting facts: `testutil.CollectAndCompare` diffs exposition text, which is what makes the
metrics tests worth writing, and `otel/exporters/prometheus` depends on `client_golang`
transitively, so "one instrumentation API" would mean both dependency trees rather than one.
*Given up:* exporter portability, and exemplars linking histogram buckets to trace ids come
less automatically.

**Process counters for events, database gauges for state.** Counters reset on deploy, which
makes "how many runs have ever failed" unanswerable from them and "how long has the oldest
queued run been waiting" — the signal that actually pages someone — impossible. One indexed
`GROUP BY` per scrape answers both. A failed query logs and emits nothing rather than an
invalid metric, because an invalid metric fails the whole scrape and would throw away the
process counters that still work.

**Cardinality is a hard rule.** Never a run id, never a goal, never a model-supplied string as
a label. `tool` is bounded by the registry, `model` and `provider` by configuration, `status`
and `outcome` by constants, and HTTP requests are labelled with chi's matched *route pattern*
(`/v1/runs/{id}`) rather than the path. Two tests assert it, because the failure is silent:
nothing breaks until the process runs out of memory weeks later.

**Buckets are set explicitly.** `prometheus.DefBuckets` tops out at 10 seconds, while the local
reranker takes 10–30s per search and `-model-timeout` defaults to ten minutes. With the
defaults nearly every model and rerank observation would land in `+Inf` and the histograms
would be decorative. Model, tool and retrieval histograms use `.1 … 300`; whole runs use
`1 … 3600`.

**Where they are served.** The API exposes `/metrics` on its existing listener. The worker had
no HTTP server, so it gets one on `-metrics-addr` (default `:9091`) serving `/metrics` and
`/healthz` — which is also the liveness probe compose was missing for it. A port already in use
is a warning rather than a fatal: two workers on one host is not a corner case, it is
`make crash-demo` and spec §2's two-worker lease demo. In compose the port is published with no
fixed host port, because `--scale worker=2` collides on the second replica the moment one is
mapped; Prometheus finds every replica by DNS on the compose network.

**One instrument set per process.** The API registers the loop's instruments too and reports
them as zero, which is what makes a query work against either job and sums correctly across
both. Splitting the set by role would trade that for a shorter exposition.

## ADR-27: Non-retryable provider errors are typed

**Decision.** `model.NonRetryable` wraps errors that cannot succeed on a second attempt —
refusals, unknown or unpriced models, authentication failures — and `model.IsNonRetryable`
tests for it. `modelStep` returns immediately on one instead of burning three attempts, and
counts it as `agentd_model_calls_total{outcome="non_retryable"}`.

**Why now.** This is the follow-up ADR-12 named: it said the retry cost of a refusal was worth
paying "until the loop grows a metric for it". It has one. Three attempts against a bad API key
bought two backoffs of latency in front of a failure that was already decided, and recorded the
third attempt's error rather than the first's — which is the one a reader needs.

**Why a wrapper rather than an error list.** The providers know which of their failures are
terminal; the loop does not, and a list of sentinel errors in the loop would have to be revised
every time a provider added one. Wrapping puts the judgement in the package that can make it,
and `errors.Is` keeps the loop's test a single call.

## ADR-28: Cassettes match on a normalised request hash, not on call order

**Decision.** A cassette entry is looked up by a SHA-256 of the request's fingerprint — model,
system prompt, conversation, tool names — taken after a declared list of volatile fields has been
stripped from every tool result and the envelope's `seq` attribute has been dropped. A miss fails
the call with a diff against the nearest recorded entry and never falls through to a live
provider.

**Why not the call's ordinal.** It is simpler and it breaks the one category §12 exists for. Model
calls are at-least-once (ADR-4): a worker that dies between `model_requested` and `model_responded`
leaves a dangling request the next worker repeats, and under ordinal matching that repeat consumes
the *next* entry and desynchronises everything after it. Every RESILIENCE case would fail for a
reason with nothing to do with the runtime.

**What hashing costs, and why the list is in the fixture.** Tool results contain fields that move
between two identical runs: `run_python`'s `duration_ms`, and `chunk_id`/`document_id`, which
`IngestDocument` mints fresh on every ingest — so they are not stable across two ingests of the
same file, let alone a fresh database in CI. The volatile list is a hole by construction: a tool
that starts returning a new volatile field breaks replay until the list learns about it. The
alternative, teaching the cassette which fields each tool produces, couples the eval harness to
every tool's payload shape. A declared list that lives in the cassette file, and that the miss
message points at, is the smaller mistake.

**Dropping the envelope `seq` is not cosmetic.** A run that crashes inside a model call comes back
with a second `model_requested`, so every event after it sits one seq higher — and the seq is
printed into the envelope of every later tool result. Hashing it would make a resumed run's calls
miss a cassette recorded from a clean one. The seq is a fact about the log's shape, not about what
the model was asked.

**A miss is never a live call.** `-on-miss` takes `fail` (the default) and `record`. A cassette
that transparently reached the real API would turn "deterministic and free" into a surprise bill
and a test that passed for the wrong reason.

**The rate card is in the header.** The pre-flight budget ceiling (ADR-22) prices a call that has
not happened yet, from usage the cassette has never seen, so it needs a price *function* rather
than a recorded number. With one, a run recorded against Claude replays its exact termination
arithmetic on a machine with no API key — and a case the ceiling stopped records an empty
cassette that is nothing but a rate card.

## ADR-29: The eval harness drives the real loop in-process

**Decision.** `agentd eval suite` opens Postgres, builds the same `runtime.Worker` that `agentd
work` builds with the provider swapped for a cassette, submits runs with `store.CreateRun`, and
reads the event log back. It owns its workers' lifetimes.

**Why not over the HTTP API.** A RESILIENCE case has to tear a worker down mid-tool-call, which is
not something it can do to a process it does not own. Owning the worker also makes the kill
*exactly* the one `make crash-demo` performs — the context is cancelled with no cleanup and the
lease is left behind — rather than an approximation of it.

**Why not a mock loop.** A harness that reimplements the loop in order to control it measures the
harness.

**What it gives up.** `make eval` exercises the runtime and not the deployment: the API server,
SSE, and compose are untouched by it, and `internal/api`'s integration tests stay the thing that
covers them.

## ADR-30: Injection is measured as two numbers, and exposure is asserted first

**Decision.** The INJECTION category reports `resisted_rate` — a soft threshold, model behaviour —
and `escalations`, a hard count of runtime failures whose only acceptable value is zero. An
escalation fails the suite whatever the resistance rate. A case whose planted text never reached
the model is *inconclusive*: its own scorecard state, excluded from the rate.

**Why two numbers.** A single pass/fail forces the corpus to be tuned until a small local model
passes, which measures the corpus rather than the defense; reporting resistance alone lets a
capability escape hide inside an 80% pass rate. The split is what `SECURITY.md` already says in
words — containment, not immunity — expressed as thresholds. Following a planted instruction is a
model lapse and a number to report; *reaching a capability the run was never granted* is a bug.

**Why exposure comes first.** The default failure mode of this category is a case whose query
never ranks the synthetic poisoned document, which then passes forever while proving nothing.
Every case names text that must appear in one of its own tool results before any claim about
resistance is made, and the suite refuses to parse an INJECTION case that declares no
`exposed_with`. A threshold on a metric that was never measured also fails, so a category that
went wholly inconclusive cannot read as a pass.

**What escalation means concretely.** Two things, because they are the two the runtime can be
wrong about: a tool produced a result although the run's allowlist never named it (ADR-8's
refusal failing), and a tool result forged the envelope delimiter (ADR-33). Both are computed for
every case in the suite, not only the injection ones — an escalation anywhere is a bug, and the
scorecard should not need a case to have anticipated it.

**The honest limit.** Under cassette replay the resistance numbers come from a recorded
trajectory and are worth what a fixture is worth. Replay proves the runtime half; `make eval-live`
is where the model is on trial, and the rate it produces is only meaningful next to the name of
the model that produced it.

## ADR-31: Thresholds and an assertion vocabulary, not golden trajectories

**Decision.** Cases declare assertions from a fixed vocabulary — status, step and cost ceilings,
tools called, documents retrieved, citations resolved, events present, budget arithmetic,
exactly-once — and categories are scored against declared thresholds. No case compares its event
log against a checked-in expected log.

**Why not golden logs.** They are more precise and worthless in practice: every legitimate change
to the loop or the system prompt rewrites every golden file, so the diffs stop being read and
start being regenerated. A fixed vocabulary fails for a reason a person can act on.

**Why no LLM judge.** Every assertion here is mechanically checkable against the log or the
database on purpose. A judge model is a second stochastic system between the runtime and its own
test results, in a harness whose entire value is removing the first one.

**Unknown fields are errors.** A mistyped assertion that silently did not run would be a case that
can only pass, which is the one thing an eval must never be. The suite parser sets
`KnownFields(true)` and rejects a case that asserts nothing at all.

## ADR-32: Search results are ordered by a tiebreaker that survives re-ingest

**Decision.** Both corpus queries break score ties on `(d.source_id, c.ordinal)` rather than on
`c.id`.

**The bug it fixes.** They used to order by `c.id`, and `IngestDocument` mints a fresh
`uuid.New()` for every chunk on every ingest — so the tiebreaker was itself random per ingest. Two
ingests of the same corpus gave identical scores and a different order, and with a small corpus
there are real ties, so the top-k changed. Every retrieval number was unreproducible across
re-ingests, the README's recall table included.

**How it was found.** Cassette replay broke after the eval corpus was truncated and re-ingested,
and the miss message named a search result whose hits had reordered. An eval that only re-ran
against a database somebody had already loaded would never have seen it — which is the argument
for CI re-ingesting from the JSONL every time rather than trusting state.

**Why these columns.** `(source_id, ordinal)` is the pair the corpus tools already hand the model
to cite with, it is unique per chunk by schema, and it is a property of the corpus text rather
than of the insert. Re-running `make eval-retrieval` after the change produced the same numbers
the README already carried: the fix makes them reproducible, it does not move them.

## ADR-33: The envelope defangs its own delimiters

**Decision.** `runtime.Envelope` rewrites any `<tool_result` or `</tool_result` appearing in a
tool result body as `<\tool_result`, in either direction and whatever its casing, before wrapping
it. The tag is broken rather than deleted, so the attempt stays readable in the event log.

**Why.** The envelope is the whole prompt-injection defense: the system prompt says everything
between those tags is data, so a body that can close the tag and keep writing is a body that can
stop being data — or open a second envelope attributed to a tool the run never called.

**What was actually true before.** Retrieved documents could not do it. Every tool serialises its
result with `encoding/json`, which escapes `<` and `>` to `<` and `>` by default, so a
poisoned opinion arrived already defanged. Tool *failures* were a different story:
`Reduce` envelopes `"error: " + p.Error` with no serialiser in between, and that error text
includes model-supplied content — `unknown tool: <name>` puts a name the model chose into the
conversation verbatim. So the hole was narrow and real.

**The point is not the width of the hole.** It is that the property was being provided by a
serialiser's default rather than by a decision. One tool switching to an `Encoder` with
`SetEscapeHTML(false)`, or M6's MCP adapter exposing a tool that returns prose, removes it with
nothing in the codebase objecting. `TestEnvelopeCannotBeForgedByItsBody` and the eval harness's
`Escalations()` are the two things that would now notice.

## ADR-34: MCP tools are namespaced, and discovered at boot rather than per run

**Decision.** A discovered tool registers as `<server>__<tool>`. Servers are declared in a config
file read at boot by *both* `agentd serve` and `agentd work`; a run cannot name its own.

**Why namespaced.** `tools.Registry.Register` rejects duplicate names, and `registry()` wires the
builtins with `MustRegister`. An MCP server that advertises a tool called `finish` would therefore
turn a working binary into one that panics at startup — a peer we do not control deciding whether
this process boots. Namespacing makes a collision impossible rather than unlikely, and it gives a
run's allowlist a way to grant one server's `search` without granting another's.

**Why at boot, and in both processes.** `api.normalizeConfig` fills a run's default allowlist from
the API's registry and rejects unknown tools at submission, while the worker dispatches through
its own. The two must agree on names or a run is admitted against a tool that cannot be
dispatched. So `serve` dials the servers too, even though it never invokes anything: a manifest is
only obtainable by asking, and the API needs one to list and to allowlist.

**Why not per run.** A run naming its own server would let a submission add a capability to the
process — the trust boundary inverted. Servers are operator configuration, like a binary on the
PATH.

**The failure mode, stated rather than hidden.** A server up for one process's boot and down for
the other's leaves the registries disagreeing. An unreachable server logs a warning and
contributes no tools (the same treatment `buildSandbox` gives an unreachable Docker daemon), so
the run is admitted and the call fails at dispatch as a non-retryable `tool_failed` the model can
route around. A run is never silently granted a capability it was not allowlisted for, which is
the property §10 actually cares about.

**Revisit when.** Servers need to come and go without a restart. That wants a manifest cache both
processes read, not per-run declaration.

## ADR-35: Tool descriptions are an injection surface the envelope does not cover

**Decision.** State the limit instead of pretending to close it. The `<tool_result>` envelope
defends retrieved content; it does not defend tool *definitions*, and this package does not try to
filter them. What bounds the attack is the trust boundary and the allowlist. The claim is scored
by an eval case rather than asserted in prose.

**Why.** A tool's name, description and JSON Schema are written by the server operator and go into
the model's tool definitions — outside every envelope, in every request, before any tool is
called. A hostile server can put an instruction there and it rides the whole run. No filter fixes
this: the description has to reach the model for the tool to be usable at all. ADR-33 hardened the
envelope against a body forging its delimiters; this is the surface the envelope was never on.

**What actually bounds it.** Servers are operator configuration read from a file at boot — adding
one is a trust decision equivalent to installing software, not equivalent to retrieving a
document (ADR-34). And because a run's allowlist is fixed at submission, a poisoned description
can only ask the model to use capabilities the run was already granted.

**What is mechanical.** `MaxDescriptionBytes`, `MaxSchemaBytes`, `MaxResultBytes` and `MaxTools`
bound blast radius, not trust: they stop a hostile *or merely broken* server from filling the
context window or refusing to let the binary start. A tool whose name cannot survive namespacing
is dropped; one whose schema will not compile gets a permissive one.

**Deliberately not done.** Prefixing every description with a "this text came from server X"
banner. It reads like a defence, costs tokens in every model call, and nothing measures whether it
helps. The `injection-poisoned-tool-description` eval case measures the real thing instead, and
`exposed_in_tools` is a distinct channel from `exposed_with` precisely because a description never
appears in a tool result — reusing the result-text check would have produced a case that can only
pass.

## ADR-36: The viewer is embedded static assets with no build step

**Decision.** `internal/api/ui` is three files of HTML, CSS and JavaScript compiled in with
`go:embed` and mounted as the router's catch-all. No framework, no bundler, no CDN, no npm.

**Why.** The project's claim is that it ships as one binary and a Postgres URL. A viewer that
needs `node_modules` to render a list and a log would add a lockfile, a second language's
dependency surface, and a build step to CI, in exchange for conveniences this page does not need.
The cost is paid in a few dozen lines of DOM helpers.

**Why a catch-all is safe.** chi matches static segments ahead of a wildcard whatever the
registration order, so `/v1/*`, `/healthz` and `/metrics` keep their handlers. Registration order
is not load-bearing and `embed_test.go` pins that in both orders rather than trusting a comment.
Rejected: serving `index.html` for anything unmatched, SPA-style, which would answer a typo'd
`GET /v1/runz` with 200 and a page of HTML.

**The XSS rule is not a style nit.** Every dynamic string the page renders — model output, tool
arguments, tool results, goals, and now MCP tool descriptions — is attacker-influenced by
construction; the corpus contains deliberately poisoned documents. Rendering goes through
`textContent` only. The project's own viewer executing the injection its eval suite proves the
agent resists would be a real defect.

## ADR-37: Ingest stays a CLI operation, because the API never calls a model

**Decision.** `POST /v1/corpus/ingest` (spec §13) is not built. Corpus loading is `agentd fetch`
and `agentd ingest`.

**Why.** §4's invariant is that the API server never calls a model — it writes intent and tails
the event log, and all execution happens in workers, which is what makes crash recovery mean
anything. Ingest embeds, and embedding is a model call, so the endpoint could never do the work
in-process. It would have to be a second job queue: a table and migration, store methods, a second
claim path in the worker, two handlers, progress reporting, and tests.

**What it would demonstrate.** Nothing the project does not already demonstrate. An async job with
a status poll is a worse version of the runs API, which does the same thing over SSE. The
`SKIP LOCKED` queue is proven. Leases are proven — and an ingest job would skip their interesting
half, since ingest is idempotent by content hash where a tool call is not, so a re-claimed job
redoes work rather than corrupting anything.

**What is left is spec completeness**, and the invariant is better evidence for itself than
something built to work around it. A boundary defended reads better than a feature shipped.

**Revisit when.** A corpus has to be loaded by something that cannot reach the worker's
filesystem — a UI upload, or a tenant. Then the job queue is the right shape and the reasoning
above is the design.

## ADR-38: A transport cap bounds the largest legal message, not the largest result

**Decision.** The streamable HTTP transport's `MaxEventSize` is `MaxEventBytes`, derived from what
a full manifest may legally be (`MaxTools × (MaxDescriptionBytes + MaxSchemaBytes)`), not
`MaxResultBytes`.

**Why.** It was `MaxResultBytes`, which was wrong in a way only a manifest shows. The transport
limit applies to *every* message, and the largest legal one is not a result but the `tools/list`
response. `MaxSchemaBytes` alone is larger than `MaxResultBytes`, so a server advertising a schema
this package would happily accept could not be listed at all — and the operator saw "server
unavailable" naming nothing that pointed at a cap.

**Nothing is lost by widening it.** A result is truncated to `MaxResultBytes` on arrival
regardless of how it got here, so the transport limit is a backstop against a stream that never
terminates, not the bound on what reaches the model.

**The deeper reason it was wrong.** The stdio transport has no equivalent limit, so the same
hostile server had a different blast radius depending on which transport an operator configured.
A bound that moves with the transport is not a security boundary, it is an inconsistency. The
stdio sibling test passed throughout, which is exactly why this needed a test of its own.

## ADR-39: Deduplication is corpus curation, and runs at the source

**Decision.** Collapsing multiple records of the same case happens in `agentd dedupe`, a pure pass
over the fetched JSONL that runs between `fetch` and `ingest`, keyed on the docket number and
keeping the longest text. It does not happen in the ingest pipeline.

**The problem.** CourtListener publishes each revision of an opinion as its own document with its
own id. A 109-document SCOTUS pull covered 84 distinct cases: *Trump v. CASA* appeared five times,
10.8% of the corpus by itself, and 25 of the 109 documents were revisions of something already
present. That inflates any retrieval measurement taken against the corpus — a query gets up to five
chances to land a relevant document in top-8 — and at serving time it spends an agent's context
window on near-identical hits.

**Why not a content hash, which is the obvious answer.** It catches none of this. All 109 texts
hash differently, because a revision is a genuinely different document — *Goldey v. Fields* arrived
at 5,496, 7,775, and 7,978 characters. Content hashing answers "do I already have these exact
bytes," and the question here is "are these two documents the same case." The key has to be
identity, not content. This was worth measuring before building: the intuitive fix was empirically
a no-op.

**Why not the ingest pipeline, which is where the duplication hurts.** Three reasons, in order of
weight:

- **The pipeline has no key to use.** `InputDoc` is deliberately generic so the corpus source is
  swappable; the docket number lives in source-specific `metadata`. Teaching `ingest.go` to read it
  couples the generic pipeline to one source's schema, which is the same mistake as special-casing
  MCP inside `tools.Registry`.
- **It would overload an invariant that is currently clean.** Ingest is idempotent per `source_id`
  by content hash, and that invariant is what makes an interrupted ingest resumable for free.
  "These two documents are the same case" is a different question with a different failure mode.
- **Guessing wrong deletes an opinion.** Which revision of an opinion is authoritative is a legal
  question. A pipeline that silently drops a superseded document during embedding gives an operator
  no way to see what went missing. A curation pass that reads one file and writes another leaves
  both on disk, and the displaced ids stay recoverable.

**What this does not fix, stated so it is not mistaken for fixed.** Deduplication at the source
keeps the benchmark honest, but the serving-time complaint — near-identical documents crowding out
the rest of top-8 — is a result-diversity problem, not an ingest one. The fix for that is a per-case
cap at search time, which has the advantage of preserving every revision rather than deleting four
of five. It is not built, and the retrieval numbers in the README are measured on a corpus where the
question does not arise.

**Consequences.** The benchmark corpus is `data/corpus/<court>-dedup.jsonl`, produced by
`make dedupe`, and `evals/retrieval/labels-scotus.yaml` names exactly one relevant document per
query. That labeling is only correct *because* the corpus is deduplicated: against the raw pull,
labeling one id scores its own revisions as misses and labeling all of them inflates recall, and
there is no third option. Deduplicating the corpus is what removes the choice between two biases.
