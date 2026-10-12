#!/usr/bin/env bash
# Append the review-handoff rule to a Paperclip agent's AGENTS.md instructions.
# Idempotent: an agent whose AGENTS.md already carries the rule is left alone.
#
# Usage: apply-in-review-rule.sh <agent-id> <reviewer-user-id>
# Env:   PAPERCLIP_API_URL (default http://127.0.0.1:3100)
#        PAPERCLIP_BOARD_TOKEN (needed in authenticated mode; unused under local_trusted)
set -euo pipefail

usage() { echo "usage: $0 <agent-id> <reviewer-user-id>" >&2; exit 2; }
[[ $# -eq 2 ]] || usage
agent=$1 reviewer=$2
[[ "$agent" =~ ^[A-Za-z0-9-]{1,64}$ ]] || { echo "agent id must be [A-Za-z0-9-]" >&2; exit 2; }
[[ "$reviewer" =~ ^[A-Za-z0-9_-]{1,64}$ ]] || { echo "reviewer id must be [A-Za-z0-9_-]" >&2; exit 2; }

api=${PAPERCLIP_API_URL:-http://127.0.0.1:3100}
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
template="$here/../instructions/in-review.md"
marker='<!-- forgectl-paperclip:in-review-rule -->'

auth=()
[[ -n "${PAPERCLIP_BOARD_TOKEN:-}" ]] && auth=(-H @<(printf 'Authorization: Bearer %s\n' "$PAPERCLIP_BOARD_TOKEN"))

url="$api/api/agents/$agent/instructions-bundle/file?path=AGENTS.md"
file=$(command curl -fsS ${auth[@]+"${auth[@]}"} "$url")
current=$(jq -r '.content // ""' <<<"$file")
# The PUT is optimistic: it must name the version it read.
base=$(jq -r '.contentHash // empty' <<<"$file")
if [[ "$current" == *"$marker"* ]]; then
  echo "agent $agent: rule already present"
  exit 0
fi

rule=$(sed "s/{{REVIEWER_USER_ID}}/$reviewer/" "$template")
body=$(jq -cn --arg c "$current"$'\n\n'"$rule" --arg h "$base" '{path: "AGENTS.md", content: $c, baseHash: (if $h == "" then null else $h end)}')
command curl -fsS -o /dev/null -X PUT ${auth[@]+"${auth[@]}"} -H 'Content-Type: application/json' \
  --data "$body" "$api/api/agents/$agent/instructions-bundle/file"
echo "agent $agent: rule appended (reviewer $reviewer)"
