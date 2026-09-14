#!/usr/bin/env bash
# Crash-recovery demo: kill -9 a worker mid-tool-call, start another, and
# watch the run finish without re-executing the tool call that already
# completed.
#
# Needs: Postgres and the API up (`make up`, or `make migrate` + `make serve`
# against a local Postgres), Ollama on the host with the model pulled
# (`make model-pull`), and jq.
#
# Env:
#   API            control plane URL           (default http://localhost:8080)
#   DSN            Postgres DSN for the workers (default local compose DSN)
#   MODEL_URL      OpenAI-compatible base URL   (default http://localhost:11434/v1)
#   MODEL          model name                   (default qwen2.5:7b)
#   LEASE          worker lease duration        (default 5s)
#   TOOL_DELAY_MS  artificial per-tool delay so the crash window is wide (default 8000)
#   OTEL_EXPORTER_OTLP_ENDPOINT  where the workers send spans (default the compose
#                  Jaeger on localhost:4318; both workers' spans land in the run's
#                  one trace, which is half of what this demo shows)
set -euo pipefail

API=${API:-http://localhost:8080}
DSN=${DSN:-postgres://agentd:agentd@localhost:5432/agentd?sslmode=disable}
MODEL_URL=${MODEL_URL:-http://localhost:11434/v1}
MODEL=${MODEL:-qwen2.5:7b}
LEASE=${LEASE:-5s}
TOOL_DELAY_MS=${TOOL_DELAY_MS:-8000}
OTLP=${OTEL_EXPORTER_OTLP_ENDPOINT:-http://localhost:4318}
GOAL=${GOAL:-"A motion was served on 2026-09-03. Use compute_deadline to find the date 30 weekdays later (skip weekends), and then call finish with that date."}

cd "$(dirname "$0")/.."
bin=$(mktemp -d)/agentd
go build -o "$bin" ./cmd/agentd
logdir=$(mktemp -d)

# Each worker gets its own metrics port: both run on this host, and a shared
# :9091 would leave the second one without a /metrics (it degrades to a
# warning rather than refusing to start, but the demo wants both).
worker() { # name metrics-addr
  "$bin" work -dsn "$DSN" -lease "$LEASE" -owner "$1" -model-url "$MODEL_URL" -model "$MODEL" \
    -metrics-addr "$2" -otlp-endpoint "$OTLP" -otel-service "agentd-worker-$1" -skip-migrate \
    > "$logdir/$1.log" 2>&1 &
  echo $!
}

events() { curl -s "$API/v1/runs/$id/events" | jq -r '.events[] | "\(.seq)\t\(.type)\t\(.payload.name // .payload.stop_reason // .payload.status // "")"'; }
tool_requests() { curl -s "$API/v1/runs/$id/events" | jq '[.events[] | select(.type=="tool_requested")] | length'; }
status() { curl -s "$API/v1/runs/$id" | jq -r '"\(.run.status)  lease_owner=\(.run.lease_owner // "none")"'; }

step() { printf '\n\033[1;36m▶ %s\033[0m\n' "$*"; }

step "start worker A (lease $LEASE)"
A=$(worker worker-a :9091)
echo "  pid $A"

step "submit a run with tool_delay_ms=$TOOL_DELAY_MS so each tool call takes a while"
id=$(curl -s -X POST "$API/v1/runs" -H 'content-type: application/json' \
  -d "$(jq -cn --arg g "$GOAL" --argjson d "$TOOL_DELAY_MS" '{goal:$g, agent_config:{tool_delay_ms:$d}, max_steps:8}')" | jq -r .id)
echo "  run $id"

step "wait for the second tool call to start (the first has then already committed)"
for _ in $(seq 1 240); do
  n=$(tool_requests)
  st=$(curl -s "$API/v1/runs/$id" | jq -r .run.status)
  if [[ "$n" -ge 2 ]]; then break; fi
  if [[ "$st" != "running" && "$st" != "queued" ]]; then
    echo "  run finished ($st) before a second tool call happened; run again" >&2
    kill "$A" 2>/dev/null || true
    exit 1
  fi
  sleep 0.5
done
sleep 1 # make sure the tool is actually executing, not just requested
events

step "kill -9 worker A mid-tool-call"
kill -9 "$A"
sleep 0.5
echo "  run is still: $(status)"
echo "  (the dead worker's lease is left behind; it expires in $LEASE)"

step "start worker B"
B=$(worker worker-b :9092)
echo "  pid $B"

step "tail the stream from where we left off"
last=$(curl -s "$API/v1/runs/$id/events" | jq '.events[-1].seq')
curl -sN -H "Last-Event-ID: $last" "$API/v1/runs/$id/stream" | grep --line-buffered '^event:' || true

step "final state: $(status)"
events
echo
curl -s "$API/v1/runs/$id" | jq -r '"final answer: \(.state.final_answer)"'
echo
echo "worker B log ($logdir/worker-b.log):"
jq -r 'select(.msg=="resuming run") | "  resuming run \(.run_id) from \(.events) events (last: \(.last_type))"' \
  "$logdir/worker-b.log" 2>/dev/null || true

# Both workers' spans are in one trace, because the trace id lives on the run
# row rather than in a context neither the queue nor the kill -9 survives
# (ADR-24). The gap where worker A's attempt span should be *is* the crash:
# the process died before it could end that span, so it was never exported,
# while the children it had already finished were.
trace=$(curl -s "$API/v1/runs/$id/trace" | jq -r '.url // empty')
if [[ -n "$trace" ]]; then
  step "one trace, both workers"
  echo "  $trace"
else
  step "no trace for this run"
  echo "  the API was started without -otlp-endpoint; bring the stack up with 'make up' to get one"
fi

echo
echo "counters: worker A http://localhost:9091/metrics, worker B http://localhost:9092/metrics"
echo "  agentd_runs_claimed_total{resumed=\"true\"} on B is this hand-off"

kill "$B" 2>/dev/null || true
