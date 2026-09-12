#!/usr/bin/env bash
# Dogfoods `forgectl pr <ref> --queue` + `forgectl pr drain` against two real
# PRs (forgectl#473): queues both, drains one pass, and asserts the pass
# report. Uses a scratch HOME so it never touches a real ~/.config/forgectl.
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

echo "--- queue $REF1 ---"
HOME="$SCRATCH" "$BIN" pr "$REF1" --queue
echo "--- queue $REF2 ---"
HOME="$SCRATCH" "$BIN" pr "$REF2" --queue

echo "--- pr queue ---"
HOME="$SCRATCH" "$BIN" pr queue

DRAIN_ARGS=(pr drain --once --json)
if [ "$DRY_RUN" = true ]; then
  DRAIN_ARGS=(pr drain --once --dry-run --json)
fi

echo "--- ${DRAIN_ARGS[*]} ---"
REPORT=$(HOME="$SCRATCH" "$BIN" "${DRAIN_ARGS[@]}")
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
