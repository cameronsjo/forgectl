## Why

`timeout=` accepted any positive integer, and the batch runner turns it into a deadline with `time.Duration(S) * time.Second`. Above 9223372036 seconds that product wraps negative, so the deadline is already past when the step starts: it gets SIGTERM, then SIGKILL, and reports rc 124 having never timed out.

## What Changes

- A `timeout=` larger than 9223372036 is a manifest error naming the line and step.

## Impact

- `internal/desk/manifest.go` (`parseStepLine`) and its tests.
- `docs/commands/desk.md`.
