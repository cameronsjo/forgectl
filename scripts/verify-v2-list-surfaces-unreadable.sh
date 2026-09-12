#!/usr/bin/env bash
# Proves that `forgectl pr list` SURFACES a session record it cannot read
# instead of silently listing fewer rows (forgectl#299, Task 1).
#
# Why HOME: forgectl resolves its config dir through os.UserConfigDir, which
# reads $HOME (macOS: ~/Library/Application Support/forgectl; Linux:
# ~/.config/forgectl). There is no FORGECTL_CONFIG_DIR. Pointing HOME at a
# scratch dir also moves gh and git config for the child process, which is
# fine here: `pr list` runs no gh or git.
#
# Usage: scripts/verify-v2-list-surfaces-unreadable.sh [path/to/forgectl]
# Exit 0 when the note is printed and the command exits 0; non-zero otherwise.
set -uo pipefail

BIN=${1:-$(command -v forgectl || true)}
if [ -z "$BIN" ] || [ ! -x "$BIN" ]; then
  echo "VERDICT: FAIL no forgectl binary (pass a path or put one on PATH)" >&2
  exit 2
fi

SCRATCH=$(mktemp -d)
trap 'rm -rf "$SCRATCH"' EXIT
case "$(uname -s)" in
  Darwin) SESSIONS="$SCRATCH/Library/Application Support/forgectl/pr-sessions" ;;
  *)      SESSIONS="$SCRATCH/.config/forgectl/pr-sessions" ;;
esac
mkdir -p "$SESSIONS"
chmod 700 "$SESSIONS"

# A record carrying one key this build does not know — the shape an older
# binary sees when a newer one has written the sessions dir.
cat > "$SESSIONS/o-r-9-1.json" <<'EOF'
{
  "workspace": "/tmp/forgectl-workflow-verify",
  "ref": "o/r#9",
  "createdAt": "2026-09-11T00:00:00Z",
  "futureKey": true
}
EOF

OUT=$(mktemp); ERR=$(mktemp)
HOME="$SCRATCH" "$BIN" pr list > "$OUT" 2> "$ERR"
rc=$?
echo "--- stdout ---"; cat "$OUT"
echo "--- stderr ---"; cat "$ERR"

if [ "$rc" -ne 0 ]; then
  echo "VERDICT: FAIL pr list exited $rc (a survey verb must exit 0 and surface the note)"
  exit 1
fi
if ! command grep -q '1 record(s) could not be read' "$ERR"; then
  echo "VERDICT: FAIL the unreadable-record note was not printed on stderr"
  exit 1
fi
echo "VERDICT: PASS pr list exits 0 and names the record it could not read"
rm -f "$OUT" "$ERR"
