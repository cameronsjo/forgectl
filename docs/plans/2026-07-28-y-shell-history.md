---
name: y-shell-history
date: 2026-07-28
status: awaiting-ruling
---

# forgectl `y` — shell-history half of #26 (decision doc)

**Refs cameronsjo/forgectl#26**

## Context

Issue #26 requested `forgectl y` — clipboard + shell-history utilities for macOS. The clipboard half already shipped (internal/clip/clip.go with `Copy`/`Paste` over `pbcopy`/`pbpaste`, `y copy`/`y paste` in y.go, tests passing). The issue deferred the shell-history half "same pattern as `proxy`'s eval-wrapper."

**Verified finding:** there is no `proxy` command, eval-wrapper, shellenv, or shell-shim precedent anywhere in the current forgectl tree. The referenced pattern is aspirational, not extant. Building the history-reading half now means **designing** a shell-integration convention from scratch — that's a design decision, not a spec'd implementation. This doc captures the decision points Cameron needs to rule on before the implementation can ship.

## Decision 1: Is a shell shim required, or just read-only history parsing?

### Background

`~/.zsh_history` is stored in zsh extended format (`: <epoch>:<elapsed>;<cmd>`), pure-Go parseable, and inheritable by child processes via `$HISTFILE`. A read-only path needs no shell integration — just a Go package that parses the file and exposes the N most recent commands.

A true shim (emitting a function to eval in `.zshrc` or equivalent) is heavier — it establishes a repo-wide shell-integration convention, requires shell-specific implementations, and adds ongoing maintenance surface.

### Options

1. **Read-only history parsing only** — `y last [n]` reads `$HISTFILE` (defaulting to `~/.zsh_history`), parses, and returns the N most recent commands. No shell function, no `.zshrc` integration, no shim. Size: S (one ops package + one verb).

2. **Shell shim (full integration)** — `forgectl y shell-init` emits a shell function to eval in `.zshrc`. This enables additional features (e.g., history-on-demand, captured in a special event hook). Size: M (multi-shell support, shim maintenance, convention establishment).

### Recommendation

**Option 1 (read-only parsing, no shim).** The read-only path covers the stated user story ("copy the last command") without inventing a shell convention under time pressure. If additional features later require shell-side capture, that's a separate decision with full context. Re-reading history from disk on each `y last` is fast enough for interactive use.

---

## Decision 2: What is the user story? Commands only, or command output?

### Background

"Copy the last command" is a history-reading feature — parse the history file, extract the command. "Copy the last command's output" is not — that requires a precmd/postexec shell hook to capture the output in real time and store it somewhere. Two different feature sets; conflate them and the scope explodes.

### Options

1. **Commands only** — `y last [n]` returns the N most recent *commands* from `~/.zsh_history`. Output capture explicitly out of scope (deferrable follow-on if needed).

2. **Commands + output** — requires a shell-side hook (the shim from Decision 1) to capture output into a side database. Much larger scope; requires shell integration and ongoing maintenance.

### Recommendation

**Option 1 (commands only).** Decouple the user story cleanly. Name the deferral explicitly ("output-capture is a follow-on") so there's no ambiguity later about what "copy history" means.

---

## Decision 3: zsh only, or bash support too?

### Background

Bash and zsh store history differently (`HISTFILE` format differs; zsh needs `INC_APPEND_HISTORY` set or the current shell's commands aren't on disk yet). Supporting both requires shell-specific parsers and documentation of platform caveats.

### Options

1. **zsh first, bash later if requested** — ship zsh support immediately (most of the homelab runs zsh). Document the limitation. If bash support is later requested, file a follow-on issue with bash-specific design.

2. **zsh and bash simultaneously** — ship both in the same PR. Larger scope; need to test both implementations.

### Recommendation

**Option 1 (zsh first).** Smaller scope, lower risk, faster to ship. The majority of macOS machines run zsh by default (post-Catalina). Bash support is a straightforward follow-on once the zsh path is proven. **Important caveat:** document that `INC_APPEND_HISTORY` must be set in `.zshrc` for the current shell's commands to appear in history before the shell exits — this is a real gotcha that users may hit.

---

## If all three recommendations are taken

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

## Awaiting ruling

**Decision 1:** Read-only parsing (no shim) or full shell-integration shim?  
*(Recommend: read-only parsing. No shim.)*

**Decision 2:** Commands only, or commands + output capture?  
*(Recommend: commands only. Output-capture is explicitly deferred.)*

**Decision 3:** zsh first, or zsh + bash simultaneously?  
*(Recommend: zsh first, with INC_APPEND_HISTORY caveat documented. Bash as a follow-on.)*

Once ruled, the implementation is straightforward — S-sized, low-risk, no breaking changes to forgectl's architecture.
