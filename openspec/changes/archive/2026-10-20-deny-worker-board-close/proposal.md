## Why

A drain worker could close a board task with `forgectl tasks done`. forgectl refuses that when `FORGECTL_DRAIN_WORKER` is set, but a worker can unset the variable. chief-of-staff's own `launch-workers.sh` adds a `Bash(forgectl tasks done:*)` deny rule, and forgectl's drain launches went without it. This is part of the owned-concessions sweep (cameronsjo/cadence-ecosystem#649); there is no forgectl issue.

## What Changes

- Every claude worker's `--settings` denies `Bash(forgectl tasks done:*)`, in every permission mode.
- ADR-0010 gains a 2026-10-20 amendment, and `docs/herdr.md` lists the rule.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `surface`: adds a requirement that a claude worker's settings deny `forgectl tasks done`.

## Impact

- Changed: `internal/launch/invocation.go` (`workerClaudeSettings`, `workerClaudeEditSettings`) and its tests, `docs/adr/0010-worker-auto-permission-mode.md`, `docs/herdr.md`.
- No new flag, exit code or `--json` field.
