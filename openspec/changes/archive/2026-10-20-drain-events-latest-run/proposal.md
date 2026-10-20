## Why

`surface drain events --since 0` returns every drain run in the events file, while any cursor above 0 returns only the latest run's events. cadence's `watch-drain.sh` therefore finds the latest run's last `seq` 1 event in jq before it filters. This is part of the owned-concessions sweep (cameronsjo/cadence-ecosystem#649); there is no forgectl issue.

## What Changes

- `surface drain events --latest-run` prints only the latest run's events, from its last `seq` 1 event on. With no `seq` 1 in view, every event shown is from one run, and all print.
- `--since 0` keeps its meaning: every run. `--since <n>` above 0 already reads only the latest run, with or without `--latest-run`.
- `docs/herdr.md` and the command's help describe the flag.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `surface`: adds a requirement for the drain events cursor and `--latest-run`.

## Impact

- Changed: `internal/surface/drain/status.go` (`LatestRun`), `internal/cli/surface_drain.go`, their tests, `docs/herdr.md`.
- New flag: `surface drain events --latest-run`. No `--json` shape change and no new exit code.
