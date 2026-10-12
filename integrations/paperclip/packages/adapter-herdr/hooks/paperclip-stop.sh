#!/usr/bin/env bash
# Claude Code Stop hook for a Paperclip herdr worker.
#
# The adapter writes a turn file outside every worktree, keyed by the Claude
# session id forgectl gave the worker:
#   ${XDG_STATE_HOME:-~/.local/state}/forgectl-paperclip/turns/<session_id>.json
# holding {"runId", "token", "callbackUrl"}. When the turn ends, this hook
# POSTs the run id, token and session id to the adapter's loopback listener.
# A session with no turn file (any ordinary session) does nothing.
# No -e or pipefail: a Stop hook that fails would surface in the worker's turn;
# every failure path exits 0 instead.
set -u

payload=$(cat)
session=$(jq -r '.session_id // empty' <<<"$payload" 2>/dev/null) || exit 0
[[ "$session" =~ ^[A-Za-z0-9-]{1,64}$ ]] || exit 0

state=${XDG_STATE_HOME:-$HOME/.local/state}
turn="$state/forgectl-paperclip/turns/$session.json"
# A plain file only: never follow a link into some other file.
[[ -f "$turn" && ! -L "$turn" ]] || exit 0

url=$(jq -r '.callbackUrl // empty' "$turn" 2>/dev/null) || exit 0
run=$(jq -r '.runId // empty' "$turn" 2>/dev/null) || exit 0
token=$(jq -r '.token // empty' "$turn" 2>/dev/null) || exit 0
[[ "$url" =~ ^http://127\.0\.0\.1:[0-9]{1,5}/stop$ ]] || exit 0
[[ "$run" =~ ^[A-Za-z0-9-]{1,64}$ ]] || exit 0
[[ "$token" =~ ^[0-9a-f]{64}$ ]] || exit 0

body=$(jq -cn --arg runId "$run" --arg token "$token" --arg sessionId "$session" \
  '{runId: $runId, token: $token, sessionId: $sessionId}') || exit 0

command curl -sS -m 5 -o /dev/null --proto =http --max-redirs 0 -X POST \
  -H 'Content-Type: application/json' --data "$body" "$url" >/dev/null 2>&1 || true
exit 0
