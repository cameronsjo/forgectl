#!/usr/bin/env bash
# Dogfoods `forgectl pr <ref> --queue` + `forgectl pr drain` against two real
# PRs (forgectl#473): queues both, drains one pass, and asserts the pass
# report. Uses a scratch HOME *and* scratch XDG dirs so it never touches a real
# ~/.config/forgectl on either macOS or Linux.
#
# Usage: scripts/dogfood-drain.sh [--launch] <ref1> <ref2> [path/to/forgectl]
#
# By default the script queues both refs (inside the scratch HOME), runs
# `pr drain --dry-run --json`, and asserts the report names both refs as
# would-launch. It creates no workspace and dispatches no tmux window. The
# live pass is opt-in: with --launch the drain clones each head and dispatches
# a review agent into a tmux window under the `forgectl` session. Muscle memory
# runs the safe path; the destructive one has to be named.
set -uo pipefail

DRY_RUN=true
if [ "${1:-}" = "--launch" ]; then
  DRY_RUN=false
  shift
elif [ "${1:-}" = "--dry-run" ]; then
  # Accepted for compatibility; it is already the default.
  shift
fi

REF1=${1:-}
REF2=${2:-}
BIN=${3:-$(command -v forgectl || true)}

if [ -z "$REF1" ] || [ -z "$REF2" ]; then
  echo "usage: $0 [--launch] <ref1> <ref2> [path/to/forgectl]" >&2
  exit 2
fi
if [ -z "$BIN" ] || [ ! -x "$BIN" ]; then
  echo "VERDICT: FAIL no forgectl binary (pass a path or put one on PATH)" >&2
  exit 2
fi

SCRATCH=$(mktemp -d)
trap 'rm -rf "$SCRATCH"' EXIT

# HOME ALONE DOES NOT ISOLATE ON LINUX. forgectl resolves its state dir through
# os.UserConfigDir(), which reads $XDG_CONFIG_HOME first there and only falls
# back to $HOME/.config — so with XDG_CONFIG_HOME set (the common case) a
# HOME-only override writes real queued records into the operator's real
# session dir, where the next real `pr drain` would launch them. XDG_STATE_HOME
# is overridden too (internal/config/usage_base.go reads it); XDG_DATA_HOME and
# XDG_CACHE_HOME are not read by this binary. macOS ignores all of these
# (os.UserConfigDir is HOME-derived), which is exactly why a run here would not
# surface the gap.
SCRATCH_ENV=(env
  "HOME=$SCRATCH"
  "XDG_CONFIG_HOME=$SCRATCH/config"
  "XDG_STATE_HOME=$SCRATCH/state"
)

echo "--- queue $REF1 ---"
"${SCRATCH_ENV[@]}" "$BIN" pr "$REF1" --queue
echo "--- queue $REF2 ---"
"${SCRATCH_ENV[@]}" "$BIN" pr "$REF2" --queue

echo "--- pr queue ---"
"${SCRATCH_ENV[@]}" "$BIN" pr queue

DRAIN_ARGS=(pr drain --once --json)
if [ "$DRY_RUN" = true ]; then
  DRAIN_ARGS=(pr drain --once --dry-run --json)
fi

echo "--- ${DRAIN_ARGS[*]} ---"
REPORT=$("${SCRATCH_ENV[@]}" "$BIN" "${DRAIN_ARGS[@]}")
RC=$?
echo "$REPORT"

if [ "$RC" -ne 0 ]; then
  echo "VERDICT: FAIL drain exited $RC" >&2
  exit 1
fi

if [ "$DRY_RUN" = true ]; then
  if ! command grep -q "$REF1" <<<"$REPORT" || ! command grep -q "$REF2" <<<"$REPORT"; then
    echo "VERDICT: FAIL dry-run report does not name both refs as would-launch" >&2
    exit 1
  fi
  echo "VERDICT: PASS dry-run named both refs; nothing was launched"
  exit 0
fi

LAUNCHED=$(printf '%s' "$REPORT" | command grep -o '"launched":[0-9]*' | head -1 | command grep -o '[0-9]*$')
if [ "${LAUNCHED:-0}" -ne 2 ]; then
  echo "VERDICT: FAIL launched=$LAUNCHED, want 2" >&2
  exit 1
fi
echo "VERDICT: PASS drained and launched both queued reviews"
