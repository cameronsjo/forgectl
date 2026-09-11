# forgectl — Roadmap (2026-09-05)

## Context

forgectl stops needing a human in the loop for routine forge work: reviews queue and drain themselves, and every surface stays drivable by an agent with no TTY (ADR-0008).

That Frame is the anchor for the cancellation test below. It is deliberately *forward*-looking — the founding Frame ("forgectl as the workbench forge", epic #1) is substantially built, which is why so little of the open tracker lands on the spine.

Supersedes [`2026-07-01-forgectl-forge-roadmap.md`](2026-07-01-forgectl-forge-roadmap.md), whose platform-first spine (`#9 → #12 → #10 → #29`) is spent.

Tracker: [cameronsjo/forgectl#474](https://github.com/cameronsjo/forgectl/issues/474)

## What the tracker gets wrong

Charting this required auditing 28 open issues against the tree, and the audit is the more useful half of the output. Read this before picking anything up.

**Five `exec:blocked` / `block:waiting` labels name prerequisites that have since shipped.** The label reads as "cannot start"; for these it means "was parked a while ago".

| # | Claims to wait on | Actually |
|---|---|---|
| #32 `pr poll` daemon | clean-room review + PR discovery | all shipped — `pr_dash.go`, `pr_pick.go`, `pr_prs.go`, `pr_local.go` |
| #27 `mcp` server | `net` (#19) | shipped — `internal/net`. Also self-labelled `idea`, "no current pain" |
| #14 `audit` | clean-room strip-list / quarantine | shipped — `internal/quarantine` |
| #13 `status` cockpit | `pr`, `clean`, `branch`, `tmux` read-paths | all shipped; only the aggregator is missing |
| #55 menu-bar tray | — | names no external wait at all; generic backlog parking |

**One label is honest:** #140 (collect-runner write confinement) really is blocked — `internal/workflow/exec.go:200` still returns `ErrNotYetWired` for the `collect` step.

**Two epics are near-closeable.** #3 (`pr` sub-epic) has only #32 left under it. #1's checklist maps almost entirely onto `internal/` packages that exist today (`launch`, `clean`, `branch`, `ghostty`, `net`, `quarantine`, `proxy`, `k8s`, `docker`, `pip`, `projects`).

**One premise is half-stale.** #194 describes the admission cap as unbuilt. `internal/pr/admission.go` implements `MaxConcurrentReviews` (default 4) today — it is simply never called from a launch path. The epic's remaining content is the drainer, not the cap.

**No open issue is gated by another open issue for more than one hop.** Every multi-step dependency chain in the tracker terminates at something already closed (#443, #218, #29–#31, #19). Ordering here is a judgment about value, not a graph constraint.

## Critical path (the spine)

#299 → #472 → #473 → #192 → #32

Five items out of 28. Everything else has a workaround — usually "a human does it", which is exactly what this Frame removes, so the test is doing real work rather than waving items through. The scariest piece is **#299** (durable session lifecycle): it is the least understood, and a drainer that corrupts state while unattended is worse than no drainer at all. Spike it first even though it ships alongside the drainer.

## Now

| # | Item | Scope | Depends on | ~ |
|---|------|-------|-----------|---|
| #299 | Durable review-session lifecycle | Crash-safe state machine for `pr` review sessions: a lock (`.pr-session-lifecycle.lock`), atomic transitions, and a `pr repair` verb for a session that died mid-flight. Its own items 1–3 gate 4/6/7/8; 5 and 9 are independent. Spike the lock and transition model before building the rest. | — | ~ |
| #458 | `check-changelog-owner` false-fails stale-base PRs | The script diffs against `BASE_SHA` rather than the merge-base, so any PR branched before a release cut is blamed for main's own `CHANGELOG.md` edit. Fix the ref. | — | |
| #469 | `recipe afk` calls an absent herdr verb | Step 2 calls `herdr agent type-submit`, which the installed herdr 0.8.2 does not have — the recipe is broken end to end. Add a preflight capability check, not a version pin. | — | |

#458 and #469 are supporting, not spine — but #458 taxes every future PR on this repo (it cost two merge-ups in one session on 2026-09-05), and #469 means a shipped verb does not work at all.

## Next

| # | Item | Depends on | ~ |
|---|------|-----------|---|
| #472 | Wire the admission cap into the launch paths | #299 | ~ |
| #473 | The drainer — empty the review backlog | #472 | ~ |
| #192 | Notify when the drainer auto-launches | #473 | ~ |
| #32 | `pr poll` — the auto-review daemon | #192 | ~ |
| #444 | Render Obsidian-flavored markdown (docs spine) | #443 (done) | |
| #445 | `docs check` — broken links and orphans | #444 | |
| #446 | Full-text search via qmd, rg fallback | — | |
| #447 | `docs read` via mdroll | — | |
| #448 | Preview fidelity — reload, KaTeX, details, rich copy | — | |
| #449 | Backlinks panel | #443 (done) | |
| #450 | Knowledge-bundle trust badges | #445 | |
| #453 | Link-resolver follow-ups from the #451 review | #443 (done) | |
| #467 | `theme.Fang()` discards fang's own light/dark probe | — | |
| #464 | `recipe afk` passes an unvalidated herdr target | — | |
| #413 | `githubauth` follow-ups — unpinned `gh` callers | — | |

The docs-reader cluster is nine of the 28 open issues and still lands in Next rather than on the spine: Obsidian is the workaround, so omitting it does not cancel the Frame. It has its own epic (#442) — and a roadmap doc that is still on the unmerged `plan/docs-reader-roadmap` branch rather than on `main`; this roadmap defers to those for its internal order.

## Later

- **#13** `status` cockpit — a human-facing aggregate view, which an autonomous system needs less, not more. Its read-paths all exist; only the aggregator is unbuilt.
- **#55** menu-bar tray — same, plus it is a second UI surface to maintain.
- **#81** `fleet status` — port of the observatory; the M5 fleet is still a standalone system (`docs/lore/herdr-fleet.md`).
- **#27** `mcp` server — ADR-0008 already makes every verb agent-drivable via CLI and JSON, so MCP is a nicer surface, not a missing one. Self-labelled a parked idea.
- **#14** `audit` — prompt-injection surface inventory. Real value, manual workaround.
- **#140** collect-runner write confinement — genuinely blocked; do it in the PR that wires `collect`'s runner.
- **#434** retire `wings.tsv` — cross-repo; the edit target is `cameronsjo/cadence`.

## Verification / done

- The spine is done when a new PR on a watched repo gets a review session started, bounded, and reported without anyone typing a command — and a machine crash mid-review leaves a session that `pr repair` can recover rather than a corrupt directory.
- **Tell-the-story test:** today you launch every review by hand. First make sessions survive a crash, because a queue that corrupts state unattended is worse than no queue. Then wire the cap that already exists, so launches are bounded. Then the drainer empties the backlog. Then it tells you when it did. Then it notices new PRs itself — and the babysitting is gone.

## Risks / open items

- ~~The spine's middle two items are unfiled.~~ Resolved 2026-09-05: filed as #472 (cap-wiring) and #473 (drainer). #194 keeps the epic framing; its cap-is-unbuilt premise is corrected in #472.
- **#299 is nine mechanisms in one issue.** It is the spine head *and* the largest single item; it likely wants splitting before it is worked, which is a decision for whoever starts it.
- **This roadmap's ordering is a value judgment, not a constraint.** The dependency audit found no multi-hop chains among open issues, so a different Frame reorders almost everything. The staleness findings above survive any re-Frame; the spine does not.
