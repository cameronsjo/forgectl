## Why

`herdr organize --apply` exited 1 when every move landed and only the focus restore failed, the same code as a failed run. A script could not tell a finished run from a broken one, though focus is cosmetic. Cameron chose exit 0 with a warning on 2026-10-14 (#732).

## What Changes

- A focus-restore failure after a clean run exits 0 and prints `warning: could not restore focus: <why>`.
- The `--json` result gains `warnings`, which carries that warning and the session warnings.

## Impact

- `internal/cli/herdr.go` (`organizeReport.applied`, `writeOrganizeJSON`) and its tests.
- `docs/commands/herdr.md`.
