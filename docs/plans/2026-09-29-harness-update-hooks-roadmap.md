---
status: done
branch: plan/harness-update-hooks
next: complete when #892 merges (items 1-3 shipped in #874, #875, #876; item 4 is #892). Remaining check is live: install the watcher on a released build and confirm `forgectl resume hooks status` after the next Claude Code update.
---

# Harness update hooks — Roadmap (2026-09-29)

## Context

When a harness updates, running sessions move onto the new version and pick up where they left off, with no per-pane manual restart. Claude Code first; codex and pi fit the same shape later.

Evidence the problem is live (2026-09-29, one machine): 9 live Claude Code sessions, 5 of them on 2.1.283/2.1.284 while 2.1.285 was installed. All 9 ran inside herdr panes.

What already exists:

- `~/.claude/sessions/<pid>.json` is Claude Code's live-process registry: `sessionId`, `cwd`, `version`, `status` (`busy` / `idle` / `shell`), `updatedAt`. `internal/resume/registry.go` already parses it and probes pid liveness.
- Every `claude` process carries `HERDR_PANE_ID`, `HERDR_SOCKET_PATH`, and `CMUX_SURFACE_ID` in its environment, readable with `ps eww` for the same user.
- `~/.local/bin/claude` is a symlink to `~/.local/share/claude/versions/<X>`; an update moves it.
- `forgectl resume <id>` already relaunches a session in its recorded cwd with the configured launch profile.

Decisions made at planning:

- Commands live under `forgectl resume`, not `forgectl sessions` (that group is the Postgres session index).
- A busy outdated session is queued and restarted once it goes idle; never interrupted.
- The manual command ships first; the watcher follows once restart is proven.
- Update hooks are configurable (`[[on_update]]`, harness + command); restart is one built-in action.

Tracker: cameronsjo/forgectl#724 (item 1 = #725, item 2 = #726, item 3 = #727, item 4 = #728)

## Critical path

item 1 → item 2 → item 3

Restart (item 3) is the outcome; outdated detection (item 2) is what it targets, and the idle-safety probe (item 1) decides whether restart can be built as designed. The scariest piece is **item 1**: if `idle` hides unsent input, an open permission prompt, or running background agents, the restart trigger changes shape.

## Now

| # | Item | Scope | Depends on | Must-have |
|---|------|-------|-----------|-----------|
| item 1 | Idle-safety probe | Measure what `status: idle` guarantees: unsent prompt text, a pending permission prompt, background tasks or teammates, and which stop signal leaves the transcript resumable. Also measure `herdr pane run` into a pane whose `claude` just exited. Output: a findings note naming the safe-to-restart predicate. | — | yes |
| item 2 | `forgectl resume outdated` | Read-only list of live sessions whose registry `version` is older than the installed `claude`, with session id, cwd, status, version, and herdr pane. `--json` for scripts. No process or pane is touched. | — | yes |

## Next

| # | Item | Depends on | Must-have |
|---|------|-----------|-----------|
| item 3 | `forgectl resume restart --outdated [--dry-run]` in herdr panes, busy sessions queued until idle | item 1, item 2 | yes |
| item 4 | `[[on_update]]` hooks + launchd watcher on the `claude` symlink | item 3 | |

## Later

- **codex / pi** — version detection and resume for the other harnesses; waits on a second harness wanting it.
- **Other surfaces** — tmux and bare cmux panes; every live session today is in herdr.
- **More hook actions** — notify-only, restart-selected; waits on real demand.

## Verification / done

- `forgectl resume outdated` on a machine with mixed-version sessions lists exactly the older ones.
- After an update, one command (or the watcher) restarts every idle outdated herdr session onto the new version, each resumed into its prior conversation in its prior pane, with no lost unsent input.
- Sessions outside herdr are reported with the command to run by hand, never silently skipped.
- **Tell-the-story test:** probe what idle means → list what's outdated → restart those safely in place → fire that restart automatically when the symlink moves.

## Risks / open items

- `status` is written by Claude Code and undocumented; a harness update can change its values. Item 2 must treat unknown values as busy.
- The registry file can outlive its process; liveness comes from the pid probe, never from `status`.
- Pid reuse: a stale registry file whose pid now belongs to another process. Restart must confirm the pid is a `claude` process before signalling it.

## Deviations

- item 4 (#728): the spec and this plan name a top-level `[[on_update]]`; it shipped as `[[resume.on_update]]`, because `resumeModule` owns config section `resume` (`ConfigKey: "resume"`) and every section has exactly one owning module. A top-level `[[on_update]]` or `[on_update]` is a config error that names `[[resume.on_update]]`, so the spelling from the spec fails loudly instead of being ignored.
- item 4 (#728): the timeout key is `timeout_seconds`, following `[net] ttl_seconds` / `timeout_ms`.
