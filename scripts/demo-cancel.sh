#!/usr/bin/env bash
# Cancellation demo: cancel a run that is sitting inside a long tool call and
# measure how long the cancel takes to land.
#
# The point is the number it prints. Before M4 a cancel was honoured at the
# next step boundary, so a run inside a 120-second sandbox call or a 10-minute
# model call kept going until that call returned on its own. Now the loop
# polls the flag during the call and interrupts it, so the bound is the poll
# interval (-cancel-poll, 1s by default) plus however long the callee takes to
# honour its context (ADR-25).
#
# It also shows the log stays well-formed: the interrupted call gets a
# tool_failed rather than being left as a dangling tool_requested.
#
# Needs: the API and a worker up (`make up`), Ollama on the host with the
# model pulled, and jq.
#
# Env:
#   API            control plane URL   (default http://localhost:8080)
#   TOOL_DELAY_MS  artificial per-tool delay, so there is a call to interrupt
#                  (default 30000 — half a minute the run would otherwise spend
#                  inside one tool call, against a cancel that should land in ~1s)
set -euo pipefail

API=${API:-http://localhost:8080}
TOOL_DELAY_MS=${TOOL_DELAY_MS:-30000}
GOAL=${GOAL:-"A motion was served on 2026-09-03. Use compute_deadline to find the date 30 weekdays later (skip weekends), and then call finish with that date."}

step() { printf '\n\033[1;36m▶ %s\033[0m\n' "$*"; }

step "submit a run whose every tool call takes ${TOOL_DELAY_MS}ms"
id=$(curl -s -X POST "$API/v1/runs" -H 'content-type: application/json' \
  -d "$(jq -cn --arg g "$GOAL" --argjson d "$TOOL_DELAY_MS" '{goal:$g, agent_config:{tool_delay_ms:$d}, max_steps:8}')" | jq -r .id)
echo "  run $id"

step "wait until the run is actually inside a tool call"
for _ in $(seq 1 600); do
  n=$(curl -s "$API/v1/runs/$id/events" | jq '[.events[] | select(.type=="tool_requested")] | length')
  st=$(curl -s "$API/v1/runs/$id" | jq -r .run.status)
  if [[ "$n" -ge 1 ]]; then break; fi
  if [[ "$st" != "running" && "$st" != "queued" ]]; then
    echo "  run finished ($st) before any tool call; run again" >&2
    exit 1
  fi
  sleep 0.5
done
echo "  tool call in flight; it would run for another ${TOOL_DELAY_MS}ms"

step "POST /v1/runs/$id/cancel and time the run_finished frame"
# The stream is opened before the cancel so the terminal frame cannot be
# missed, and the clock starts at the POST rather than at the connect: what is
# being measured is the operator's wait, not the SSE tail's.
started=$(python3 -c 'import time; print(time.time())')
curl -s -X POST "$API/v1/runs/$id/cancel" >/dev/null
last=$(curl -s "$API/v1/runs/$id/events" | jq '.events[-1].seq')
curl -sN -H "Last-Event-ID: $last" "$API/v1/runs/$id/stream" | grep --line-buffered -m1 '^event: run_finished' >/dev/null
finished=$(python3 -c 'import time; print(time.time())')

printf '\n\033[1;32m  cancel landed in %.2fs\033[0m\n' "$(python3 -c "print($finished - $started)")"

step "the log is well-formed: the interrupted call was answered"
curl -s "$API/v1/runs/$id/events" | jq -r '.events[] | "  \(.seq)\t\(.type)\t\(.payload.error // .payload.phase // .payload.name // "")"'
echo
curl -s "$API/v1/runs/$id" | jq -r '"  status: \(.run.status)   spent: \(.run.spent_usd)"'

trace=$(curl -s "$API/v1/runs/$id/trace" | jq -r '.url // empty')
[[ -n "$trace" ]] && echo "  trace:  $trace"
echo "  the cancel's phase is on agentd_cancellations_total{phase=...} in /metrics"
