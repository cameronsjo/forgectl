## Why

A manifest step with two `after=` options kept only the last one, so `c after=a after=b` waited on `b` alone. `c` could start while `a` was still running or after it had failed, and it never saw `a`'s outputs. A repeated `timeout=` was likewise silently last-wins.

## What Changes

- Repeated `after=` options on one step add up.
- A repeated `timeout=` on one step is a manifest error.

## Impact

- `internal/desk/manifest.go` (`parseStepLine`) and its tests.
- `docs/commands/desk.md`.
