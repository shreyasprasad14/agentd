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
