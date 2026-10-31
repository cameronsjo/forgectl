## Why

A chief-of-staff session runs several unattended worker sessions at once, each in an hc (herdr-core) terminal, each owing a report file in its brief directory and usually a pull request. Today the operator follows them by running `hc ls`, `hc read`, `ls <brief dir>` and `gh pr view` by hand, one worker at a time, and a worker that is blocked on a permission dialog sits unnoticed until someone looks. The operator asked for a simple live terminal view first, and a more playful interactive one later.

## What Changes

- New verb `forgectl surface watch`: a read-only, live Bubble Tea view with one row per worker. Each row shows the worker's name, hc address, self-reported status (working, idle, done, blocked, error), age, model when its argv names one, its report files (how many, which end in `REPORT-COMPLETE`, the newest), and its PR (number, head SHA, CI rollup, the latest `cadence-review` marker and whether it is at head).
- A blocked worker is highlighted as **needs you**, and a banner names it and the command that shows its dialog.
- The selected row shows a tail of that terminal's screen (`hc read`), refreshed while selected. Worker text is untrusted: escape sequences are stripped and every remaining control character is escaped before rendering.
- Workers come from `--workers FILE` (chief-of-staff `name|worktree|brief dir` lines) and, with `--cos`, every live hc terminal whose name starts with `cos-`.
- Updates are driven by `hc events` (a non-consuming subscription, so the view never takes the orchestrator's `hc wait` news), with a poll as the fallback. GitHub is read through one GraphQL call per worker, cached for `--gh-ttl` (90 s default, 30 s minimum) and serialized.
- Off a terminal it prints one plain snapshot; `--json` prints one snapshot as JSON (ADR-0008).

Surfaces touched: one new subcommand `surface watch` with flags `--workers`, `--cos`, `--json`, `--interval`, `--gh-ttl`. No config key, no new exit code. It never sends keys, kills, or briefs.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `surface`: a new worker-watch requirement.

## Impact

- Code: new `internal/workerwatch` (workers file, hc client, report scan, GitHub read, collector), `internal/tui/workers.go` (the view), `internal/cli/surface_watch.go` (the verb).
- Docs: `docs/commands/surface.md`, README.
- hc 0.1.15 is the contract read (`hc get`, `hc ls`, `hc read`, `hc events`); a missing `hc` shows as an error row, not a crash.
