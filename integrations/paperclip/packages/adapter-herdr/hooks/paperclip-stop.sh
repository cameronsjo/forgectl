#!/usr/bin/env bash
# Claude Code Stop hook for a Paperclip herdr worker.
#
# The adapter writes .paperclip/turn.json in the worker's worktree before each
# turn: {"runId", "callbackUrl", "token"}. When the turn ends, this hook POSTs
# the run id, the Claude session id and the transcript path back to the
# adapter's loopback listener. It never blocks the turn: every failure exits 0.
set -u

payload=$(cat)
dir=${CLAUDE_PROJECT_DIR:-$(pwd)}
turn="$dir/.paperclip/turn.json"
[ -f "$turn" ] || exit 0

url=$(jq -r '.callbackUrl // empty' "$turn" 2>/dev/null) || exit 0
case "$url" in
  http://127.0.0.1:*/*) ;;
  *) exit 0 ;;
esac

body=$(jq -c --argjson hook "$payload" \
  '{runId, token, sessionId: $hook.session_id, transcriptPath: $hook.transcript_path, stopHookActive: ($hook.stop_hook_active // false)}' \
  "$turn" 2>/dev/null) || exit 0

command curl -sS -m 5 -o /dev/null -X POST -H 'Content-Type: application/json' \
  --data "$body" "$url" >/dev/null 2>&1 || true
exit 0
