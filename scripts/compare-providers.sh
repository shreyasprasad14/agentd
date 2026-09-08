#!/usr/bin/env bash
# M1.5 demo: submit one goal twice, once to the worker's default local model
# and once to Claude, then put the two trajectories side by side. Same loop,
# same tools, same event schema; only the model_responded events differ.
#
# Needs: the API and a worker up (`make up`, with ANTHROPIC_API_KEY exported
# before `make up` so the worker container gets it), Ollama on the host, jq.
#
# Env:
#   API              control plane URL      (default http://localhost:8080)
#   ANTHROPIC_MODEL  hosted model           (default claude-opus-5)
#   BUDGET_USD       per-run budget         (default 0.50)
#   GOAL             the shared goal
set -euo pipefail

API=${API:-http://localhost:8080}
ANTHROPIC_MODEL=${ANTHROPIC_MODEL:-claude-opus-5}
BUDGET_USD=${BUDGET_USD:-0.50}
GOAL=${GOAL:-"A motion was served on 2026-09-03. Use compute_deadline to find the date 30 weekdays later (skip weekends), and then call finish with that date."}

submit() { # model ("" for the worker default)
  local cfg='{}'
  if [[ -n "$1" ]]; then cfg=$(jq -cn --arg m "$1" '{model:$m}'); fi
  curl -s -X POST "$API/v1/runs" -H 'content-type: application/json' \
    -d "$(jq -cn --arg g "$GOAL" --argjson c "$cfg" --arg b "$BUDGET_USD" '{goal:$g, agent_config:$c, budget_usd:$b, max_steps:8}')" \
    | jq -r .id
}

wait_terminal() { # id
  for _ in $(seq 1 600); do
    st=$(curl -s "$API/v1/runs/$1" | jq -r .run.status)
    case "$st" in queued|running) sleep 0.5 ;; *) return 0 ;; esac
  done
  echo "run $1 did not finish" >&2
  return 1
}

trajectory() { # id
  curl -s "$API/v1/runs/$1/events" | jq -r '
    .events[] | "\(.seq)\t\(.type)" + (
      if .type == "model_responded" then "  \(.payload.provider)/\(.payload.model) \(.payload.stop_reason) in=\(.payload.usage.input_tokens) out=\(.payload.usage.output_tokens) $\(.payload.cost_micro_usd/1000000)"
      elif .type == "tool_requested" then "  \(.payload.name) \(.payload.args|tostring)"
      elif .type == "tool_succeeded" then "  \(.payload.name) -> \(.payload.result|tostring)"
      elif .type == "tool_failed" then "  \(.payload.name) !! \(.payload.error)"
      elif .type == "run_finished" then "  \(.payload.status) \(.payload.final_answer // .payload.error // "")"
      else "" end)'
}

summary() { # label id
  curl -s "$API/v1/runs/$2" | jq -r --arg l "$1" '
    "\($l)\t\(.run.status)\t\(.state.steps) steps\t\(.run.input_tokens) in / \(.run.output_tokens) out\t$\(.run.spent_usd)\t\(.state.final_answer // .state.error // "")"'
}

printf '\033[1;36m▶ submitting the same goal to both providers\033[0m\n  %s\n' "$GOAL"
local_id=$(submit "")
hosted_id=$(submit "anthropic/$ANTHROPIC_MODEL")
echo "  local  run $local_id"
echo "  hosted run $hosted_id  (anthropic/$ANTHROPIC_MODEL, budget \$$BUDGET_USD)"

printf '\n\033[1;36m▶ waiting for both to finish\033[0m\n'
wait_terminal "$local_id"
wait_terminal "$hosted_id"

printf '\n\033[1;36m▶ summary\033[0m\n'
{ summary local "$local_id"; summary hosted "$hosted_id"; } | column -t -s $'\t'

printf '\n\033[1;36m▶ trajectories (local | hosted)\033[0m\n'
tmp=$(mktemp -d)
trajectory "$local_id" > "$tmp/local"
trajectory "$hosted_id" > "$tmp/hosted"
if command -v diff >/dev/null; then
  diff --side-by-side --width="${COLUMNS:-200}" "$tmp/local" "$tmp/hosted" || true
else
  paste "$tmp/local" "$tmp/hosted"
fi
rm -rf "$tmp"
