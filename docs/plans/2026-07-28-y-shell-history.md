---
name: y-shell-history
date: 2026-07-28
status: scoped
ruled: 2026-07-29
---

# forgectl `y` — shell-history half of #26 (scoped plan)

**Refs cameronsjo/forgectl#26**

## Architectural position (shared with #167 / #168)

forgectl remains an exec-and-replace tool. `forgectl launch` execs into claude and does not
return, and this plan does not change that. Neither this feature nor its companion changes it
either — no persistent foreground supervisor, no shell-integration shim, in
`plan/26-y-shell-history` (PR cameronsjo/forgectl#167) or `plan/153-launch-auto-exit` (PR
cameronsjo/forgectl#168). Any future change to that posture is a separate, deliberate decision,
made only after a probe demonstrates today's architecture cannot serve the need.

## Context

Issue #26 requested `forgectl y` — clipboard + shell-history utilities for macOS. The clipboard half already shipped (internal/clip/clip.go with `Copy`/`Paste` over `pbcopy`/`pbpaste`, `y copy`/`y paste` in y.go, tests passing). The issue deferred the shell-history half "same pattern as `proxy`'s eval-wrapper."

**Verified finding, corrected premise:** there is no `proxy` command, eval-wrapper, shellenv, or shell-shim precedent anywhere in the current forgectl tree. The referenced pattern is aspirational, not extant, and the issue's framing ("same pattern as...") was wrong. That correction stands on its own, independent of the architectural position above: even if forgectl *did* have a shim precedent to copy, the ruling above forecloses adding one here.

## Scope (ruled 2026-07-29)

The three design questions below are resolved by the architectural position: a shim is out,
which settles all three together rather than independently.

### Read-only history parsing. No shim.

`~/.zsh_history` is stored in zsh extended format (`: <epoch>:<elapsed>;<cmd>`), pure-Go
parseable, and inheritable by child processes via `$HISTFILE`. `y last [n]` reads `$HISTFILE`
(defaulting to `~/.zsh_history`), parses it, and returns the N most recent commands. No shell
function, no `forgectl y shell-init`, no `.zshrc` integration — that would be a shell-integration
shim, and the architectural position above declines it until a probe proves it necessary.
Re-reading history from disk on each `y last` is fast enough for interactive use.

### Commands only. Output capture is a named non-goal.

"Copy the last command" is a history-reading feature — parse the history file, extract the
command. "Copy the last command's output" is a different feature: it requires a precmd/postexec
shell hook to capture output in real time plus a side database to store it — exactly the
persistent shell-integration surface the architectural position declines. `y last [n]` returns
commands only. Output capture is out of scope, not deferred-pending-design — it does not ship
until the architectural ruling itself changes.

### zsh first. bash is a follow-on, not in scope.

Bash and zsh store history differently (`HISTFILE` format differs; zsh needs
`INC_APPEND_HISTORY` set or the current shell's commands aren't on disk yet). This plan ships
zsh support only. **Caveat to document in the implementation:** `INC_APPEND_HISTORY` must be set
in `.zshrc` for the current shell's commands to appear in history before the shell exits —
without it, `y last` won't see commands from the still-running shell. bash support is a
follow-on, filed as a separate issue with its own bash-specific design, not part of this plan.

## Implementation

Files are in `internal/clip/` (shared with the clipboard half) or a new `internal/history/` package with:

- Pure-Go zsh history parser (`.zsh_history` extended format)
- No shell shim, no `forgectl y shell-init`
- No config key, no registry entry
- One subcommand: `y last [n]` (default n=1)
- Size: S (one ops file + tests + one verb integration in `y.go`)
- Risk: low

### Exact files

- `internal/clip/history.go` *or* `internal/history/history.go` — `Parser` with `ReadZshHistory(ctx, path) ([]HistoryEntry, error)` and `LastN(entries, n) []HistoryEntry`. Entries are `{Timestamp time.Time, Elapsed time.Duration, Command string}`. Pure parser, no file I/O (path passed as argument).
- `internal/clip/history_test.go` *or* `internal/history/history_test.go` — table-driven, fixtures from a real `.zsh_history`, verify parsing and ordering.
- `internal/cli/y.go` — add a `last` subcommand under the parent: `y last [n]` (flag `--count` or default to last 1).
- Verification: `go build ./...`, `go vet ./...`, `go test ./...`; manual on macOS: `forgectl y last`, then `forgectl y last 5`.

### Out of scope

- Bash support (follow-on if requested)
- Shell shim or `.zshrc` integration
- Output-capture or postexec hooks
- Any config key or registry entry

---

## Ruling record

This plan originally presented three open decisions to Cameron. The owner ruled on the shared
architectural fork between this plan and #153 on 2026-07-29: **forgectl stays exec-replace; it
does not grow a foreground supervisor or a shell shim until a probe proves one necessary.** That
ruling settles all three decisions here in one pass (a shim was the only path to a shell
function, to output capture, and to same-process bash support), so this plan is scoped and ready
for implementation rather than awaiting further input. The prior recommendations (read-only
parsing, commands only, zsh first) are unchanged by the ruling — they are now the plan, not a
recommendation pending it.
