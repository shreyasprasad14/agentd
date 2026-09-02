# agentd

A control plane for running LLM agents as durable, resumable, sandboxed, observable jobs.

**Status: M0 (skeleton) complete.** The model provider, sandbox, and retrieval are not implemented yet.

## M0 — Skeleton

M0 proves the plumbing that everything else hangs off: the append-only event log and the
streaming path. A run is a row in `runs` plus an ordered log in `run_events`. The API server
never calls a model — it writes intent and tails the log — so all execution lives in workers,
which is what makes crash recovery meaningful later. Workers claim runs straight out of
Postgres with `SELECT ... FOR UPDATE SKIP LOCKED`, take a 60-second lease, and heartbeat it;
no Redis or NATS, because at this scale a table with a partial index is the queue. In place of
the real agent loop, the worker runs a stub that appends `run_started`, one `model_responded`,
and `run_finished` with a one-second pause between each, which is enough to watch events land
on the SSE stream in real time. The SSE contract carries each event's `seq` as its SSE `id`,
so a client that drops the connection reconnects with `Last-Event-ID` and gets exactly the
events it missed before the live tail resumes. An integration test (`internal/api`) boots real
Postgres 16 + pgvector via testcontainers, submits a run, and asserts the three events arrive
over SSE in order — then reconnects at `Last-Event-ID: 1` and asserts only the last two replay.

Deliberately deferred: `Reduce(events) RunState` and the lease reaper arrive with the real loop
in M1, since M0's stub has no state worth folding.

## Quickstart

```bash
make up                       # Postgres 16 (pgvector), Jaeger, api, worker
curl -X POST localhost:8080/v1/runs \
  -H 'content-type: application/json' \
  -d '{"goal":"what is the holding in the stub opinion?"}'
# => {"id":"...","status":"queued"}

curl -N localhost:8080/v1/runs/<id>/stream
# id: 1 / event: run_started ...
# id: 2 / event: model_responded ...
# id: 3 / event: run_finished ...
```

Resume a dropped stream from where it left off:

```bash
curl -N -H 'Last-Event-ID: 1' localhost:8080/v1/runs/<id>/stream
```

`make demo` does the submit-and-tail in one step. Jaeger's UI is on
[localhost:16686](http://localhost:16686) — nothing exports to it until M4.

## Running without compose

```bash
make migrate     # apply migrations
make serve       # API on :8080
make work        # worker, in a second shell
```

Both `serve` and `work` apply migrations at startup (advisory-locked, so racing them is safe)
and retry the initial connection for 30s, so container start order does not matter. Pass
`-skip-migrate` to opt out.

## Endpoints

| Method | Path | |
|---|---|---|
| `POST` | `/v1/runs` | submit a run → `{id, status}` |
| `GET` | `/v1/runs/:id` | run record |
| `GET` | `/v1/runs/:id/events` | full event log as JSON |
| `GET` | `/v1/runs/:id/stream` | SSE tail, replays from `Last-Event-ID` |
| `GET` | `/healthz` | liveness |

## Tests

```bash
make test        # includes testcontainers integration tests; requires Docker
make test-short  # skips anything needing Docker
```

## Layout

```
cmd/agentd/            # serve | work | migrate
internal/api/          # handlers, SSE tail
internal/runtime/      # event types, worker, stub agent (real loop lands in M1)
internal/store/        # Postgres access + embedded migrations
deploy/                # docker-compose.yml, Dockerfile
```

## Notes on choices made in M0

- **Hand-written pgx, not sqlc yet.** Every query lives in `internal/store/store.go`; the
  schema is still moving. Swapping in sqlc later is mechanical.
- **SSE polls the log at 200ms** rather than using `LISTEN/NOTIFY`. Polling has no missed-
  notification edge cases around reconnects, and one indexed query per stream per 200ms is
  free at this scale. `LISTEN/NOTIFY` is the upgrade when event volume justifies it.
- **Terminal status is written before `run_finished` is appended**, so a client that sees the
  final event and immediately re-reads the run never observes a finished log against a
  still-`running` status.

## Future work

Human-in-the-loop approval flows are out of scope for v1 (spec §2) and would slot in as a
`approval_requested` / `approval_granted` event pair that suspends the loop.
