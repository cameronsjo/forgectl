# herdr commands — Roadmap (2026-09-29)

## Context

Manage a crowd of herdr sessions from the keyboard: see which need attention, get to one, and keep the layout tidy and recoverable. Native forgectl commands replace the standalone `forgectl-herdr` script, which already answers `forgectl herdr organize` through external-command dispatch.

Tracker: —

## Critical path

1 → 3 → 4

Without a shared client nothing else can talk to herdr safely, without `inbox` there is no answer to "what needs me", and without `jump` there is no quick way to reach it. Everything else has a manual workaround. The scariest piece is **item 3**: herdr's own `blocked` and `idle` states have been observed wrong (`blocked` at a plain input prompt, `idle` on a stuck agent; see `docs/plans/2026-09-28-forgectl-herdr-coordinator.md`), so item 2 measures that before `inbox` trusts it.

## Now

| # | Item | Scope | Depends on | Must-have |
|---|------|-------|-----------|-----------|
| 1 | Shared `internal/herdr` client | Typed wrappers over `herdr workspace\|tab\|pane\|agent list\|get\|move\|focus`. Error model: exit 1 with `{"error":{"code","message"}}` on stderr, matched on `code`. Parse `move_result.changed` and `reason` (herdr exits 0 when it declines a move). Capability probe for `tab move` (present in the `cameronsjo/herdr` fork, absent upstream and hidden from `tab --help`) with a clear refusal. Session gate on `HERDR_ENV=1`. Fixtures captured read-only from a live session and sanitized. One package shared with the `herdr-coordinator` plan: agree the surface with that plan (forgectl#536) before either lands. | — | yes |
| 2 | Status-reliability spike | Compare `agent_status` against an independent read of each pane (`herdr pane read`) across live claude, codex, and pi sessions. Output: a short note on which states to trust, which to cross-check, and the rule `inbox` will use. No shipped command. | 1 | |

## Next

| # | Item | Depends on | Must-have |
|---|------|-----------|-----------|
| 3 | `herdr inbox` (`--json` per ADR-0008): agents needing attention, blocked first, then done | 1, 2 | yes |
| 4 | `herdr jump`: fuzzy-find a tab or agent and focus it, restoring by `terminal_id` | 1, 3 | yes |
| 5 | `herdr organize`: Go port of the script; findings in forgectl#720 | 1 | |
| 6 | Notifier: poll, diff `state_change_seq`, fire `herdr notification show` | 1, 3 | |

## Later

- **Layout save/restore** — rebuild workspaces, tabs, and panes after a herdr restart (which kills every pane) and relaunch sessions through `resume`. Biggest unknown; spike it early even though it is not on the critical path.
- **Fan-out prompt** — send one prompt to selected agents through `herdr agent prompt`, listing targets and confirming first.
- **Stale-session report** — agents idle or done for days; report only, never close.
- **Sidebar tokens** — `herdr workspace report-metadata`.
- **Workspace placement in `surface launch`** — a second caller for the organize rule table; touches the `herdr-coordinator` plan's territory.
- **`recipe afk` stale-env fix** — moved panes keep old `HERDR_PANE_ID` values that resolve only until herdr reuses the number; verify against `herdr pane get` before typing into a pane.

## Verification / done

- Each item ships with tests against a `FakeRunner` replaying captured fixtures, including post-move ids for anything that moves tabs.
- Every command that mutates layout restores the caller's focus, also on the error path.
- Modules are registered with their completeness pins and config sections in one commit; `CHANGELOG.md` is left to release-please.
- **Tell-the-story test**: install the shared client, learn which herdr states can be trusted, ask `inbox` what needs attention, and `jump` to it. Organize, notifier, and restore then make the layout tidy, push the news to you, and survive a restart.

## Risks / open items

- **Fork dependency.** `tab move` needs the `cameronsjo/herdr` fork. This is a public repo, so the dependency must be documented and the probe must fail with a clear message on stock herdr.
- **Two plans, one client.** forgectl#536 (worker verbs on `surface`) and this roadmap both need herdr access. Reconcile the package name and surface before item 1 starts.
- **Ids renumber on move.** A cross-workspace `tab move` gives the tab and its panes new ids; plan tab order from a fresh snapshot, not the one taken before the moves.
- **Concurrent writers.** Two `--apply` runs or a plugin ticker can act on stale ids; take a lock and verify each target's `terminal_id` before mutating.
