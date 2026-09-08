# agentd

A control plane for running LLM agents as durable, resumable, sandboxed, observable jobs.

**Status: M1.5 (second model provider) complete.** Sandbox, retrieval, tracing, and evals are
not implemented yet.

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

## Quickstart

You need Docker, Go 1.25, and [Ollama](https://ollama.com) on the host. For Claude, export
`ANTHROPIC_API_KEY` before `make up` (or `make work`); without it local runs are unaffected and
runs that target Claude fail with an authentication error.

```bash
brew install ollama && ollama serve &   # or the desktop app
make model-pull                         # ollama pull qwen2.5:7b
make up                                 # Postgres 16 (pgvector), Jaeger, api, worker

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
`-reaper-interval`, `-owner`, `-anthropic-prices`, `-anthropic-thinking-display`. Env
equivalents: `AGENTD_MODEL_URL`, `AGENTD_MODEL`, `AGENTD_DSN`, `AGENTD_WORKER_OWNER`,
`AGENTD_ANTHROPIC_PRICES`, `AGENTD_ANTHROPIC_THINKING_DISPLAY`. Credentials for Claude come
from `ANTHROPIC_API_KEY` (or a profile from `ant auth login`); `-model claude-opus-5` makes
Claude the default for runs that name no model.

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
(allowlist; defaults to every registered tool and is fixed at submission), `max_tokens`,
`tool_delay_ms` (demo only). Unknown fields and unknown tool names are rejected with 400.

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
make test                 # unit + testcontainers integration tests; requires Docker
make test-short           # unit tests only
make test-live            # one real call to Ollama; requires the model pulled
make test-live-anthropic  # two real calls to Claude (a tool call, then the tool result
                          # with the thinking turn echoed back); needs ANTHROPIC_API_KEY
```

The Anthropic provider's unit tests point the real SDK client at an `httptest` server, so the
wire body is asserted exactly: thinking blocks echoed in order, tool schemas passed through with
every keyword, cache tokens priced. The loop's router test runs two fakes behind a `Router` and
checks each run is priced and labelled by the backend that answered it.

## Layout

```
cmd/agentd/            # serve | work | migrate
internal/api/          # handlers, SSE tail, config normalisation
internal/runtime/      # event payloads, Reduce, the loop, worker (claim/heartbeat/reaper)
internal/model/        # Provider interface, pricing, Router; local/ (OpenAI-compatible), anthropic/ (Claude), fake/ (tests)
internal/tools/        # Tool interface, registry + schema validation; builtin/ (finish, compute_deadline)
internal/store/        # Postgres access, fenced writes, ledger, embedded migrations
internal/testutil/     # shared testcontainers Postgres fixture
scripts/crash-demo.sh  # the kill -9 demo
scripts/compare-providers.sh  # one goal, both providers, trajectories side by side
docs/DECISIONS.md      # ADRs
docs/plans/            # per-milestone plans
deploy/                # docker-compose.yml, Dockerfile
```

## Notes on choices

See [docs/DECISIONS.md](docs/DECISIONS.md) for the ADRs: Postgres as the queue, content
blocks as the canonical message format, OpenAI-compatible local provider, at-least-once model
calls vs exactly-once tool calls, lease fencing, micro-USD cost accounting, the reaper,
submission-time allowlists, reduce-every-iteration, provider routing by model name, thinking
blocks as opaque log content, and refusing to call an unpriced model.

Earlier notes from M0 still hold: hand-written pgx rather than sqlc while the schema moves;
SSE polls the log at 200ms rather than `LISTEN/NOTIFY`; the terminal status and
`run_finished` event now commit in one transaction.

## Future work

Human-in-the-loop approval flows are out of scope for v1 (spec §2) and would slot in as an
`approval_requested` / `approval_granted` event pair that suspends the loop.
