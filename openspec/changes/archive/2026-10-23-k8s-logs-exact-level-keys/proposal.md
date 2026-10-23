## Why

`k8s logs` read the severity by decoding into a struct, and `encoding/json` matches struct field names in any letter case, with the last match winning. So `{"Level":"debug"}` counted as a level and was hidden under `--log-level info`, and `{"level":"error","Level":"debug"}` was read as debug and hidden under `--log-level warn`.

## What Changes

- Only the exact keys `level` and `severity` are recognized. A line with only `Level` or `LEVEL` is unrecognized and always prints.

## Impact

- `internal/k8s/logs.go` (`jsonSeverity`) and its tests.
- `docs/commands/k8s.md`.
