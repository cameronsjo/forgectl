## Why

fang v1.0.0 renders `--help`, `--version` and error frames and asks the terminal for its background (OSC 11 plus DA1) on any stdout TTY, with no option to skip it. Inside tmux, with `NO_COLOR`, with `TERM=dumb` or with a forced `[theme] mode`, forgectl's own policy refuses that query, yet fang sent it anyway: the bytes landed in the output, and a terminal that never answered stalled each render for about 4.5 s (#546, #1152). Cameron chose on 2026-10-14 to stop fang's probe where the policy refuses it.

## What Changes

- Where the policy refuses a probe, help and error frames render with stdin detached from the terminal for the length of the render, so lipgloss never sends the query. The frames, colors and handlers are fang's own, unchanged.
- `TERM=dumb` joins the refusal list for this path.

## Impact

- `internal/cli/fang_noprobe.go` (new), `execute.go`, `unknown_help.go`, and tests.
- `docs/configuration.md`.
