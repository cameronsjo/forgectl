## Context

Chief-of-staff workers run in hc terminals named `cos-<slug>`. hc reports each one's status from the program's own OSC 7501 reports, so the status is the agent's claim about itself, not an observation. Each worker owes `report.md` (or `report-<topic>.md`) in its brief directory, ending in `REPORT-COMPLETE`, and usually opens a draft PR from its worktree's branch.

## Goals / Non-Goals

**Goals (slice 1):** one screen that answers "who is working, who is stuck, who has reported, and where is each PR" without the operator typing anything; a blocked worker is impossible to miss; the view is cheap enough to leave open all day.

**Non-Goals:** sending keys, answering dialogs, killing or briefing workers (the view is read-only by design); replacing the orchestrator's report verification; a herdr-app or browser UI.

## Decisions

- **Verb: `surface watch`.** The `surface` group already owns worker reads (`surface read`, `surface status`, `surface wait`). This reads hc directly, through a small interface of its own, so it does not depend on the hc surface backend in flight elsewhere; when that lands, the hc reads can move onto it.
- **Event-driven, poll as fallback.** `hc events --json` is a subscription: it does not consume anyone's unseen news, unlike `hc wait --children`, which would take the orchestrator's notifications. Any `status`, `turn`, `created`, `finished`, `removed`, `lost` or `restarted` event triggers a re-collect, at most once a second. With the stream up, a 30 s safety poll remains; if it ends, the view says so and polls every `--interval` (5 s default).
- **hc reads are batched.** One `hc ls` per collect gives every live worker's address and status; `hc get` runs once per new address for its start time and argv (which never change), and for a name `hc ls` does not list (finished or gone). `hc read` runs only for the selected row, every 2 s. `--cos` remembers every `cos-*` name it has seen live, so an exited worker stays as an `exited` row. On quit the view cancels the `hc events` child and waits (3 s bound) until it is reaped.
- **Narrow terminals.** Columns have priorities (NAME, STATUS, REVIEW, CI, REPORTS first; PR, AGE, HC, MODEL after). NAME narrows before a verdict column drops, REPORTS has a short form, and a `→ N more cols` hint says when anything is hidden, and `↕ N more rows` when the table is cut. Priority is strict: a column that does not fit is never replaced by a lower-priority one. Cells are cut by display width, so wide names cannot shift columns. Keys lead the header so they survive a cut line; `?` shows help, and Esc closes it rather than quitting.
- **GitHub is cached and serialized.** One GraphQL call per worker returns the PR for the worktree's branch, its head, its check rollup and the viewer's last 20 reviews. Entries live for `--gh-ttl`; at most two stale entries refresh per collect, one call at a time, so a dozen workers never burst a dozen calls. `r` forces a refresh of entries older than 15 s.
- **Markers are claims, summed like sweep.** Only reviews by the authenticated login are read, as `sweep` does, but workers post under that login too, so this is not a trust filter. The headline REVIEW cell applies `sweep`'s rule: only `[sweep] reviewers` count when that list is set, and `crit`/`imp` are summed over every counted marker at head with no per-reviewer deduplication, so the watch is never less alarming than `sweep` beside it (a later `crit=0` re-run cannot cancel an earlier `crit=1`). The column is starred as a claim.
- **Brief dirs are worker-owned.** Report files are opened `O_NONBLOCK|O_NOFOLLOW` and must be regular on the open handle, so a swapped-in FIFO or symlink cannot stall or redirect the scan.
- **Untrusted text.** Screen tails pass through `ansi.Strip` and then `termsafe.SafeLine`, so an OSC 52 clipboard write, a hyperlink, a screen clear or a bidi override in a worker's output renders inert. Names, branches and file names are capped and escaped the same way.

## Risks / Trade-offs

- [Self-reported status] A wedged agent can report `working` forever. → The column is labelled self-reported, and the age and screen tail are beside it.
- [hc starts a core] Some hc commands start a core when none runs. → The view only reads; tests never run hc, and a live smoke uses a scratch `HC_SESSION` and scratch XDG dirs.
- [Rate limits] Many workers and a short TTL could still add up. → TTL floor of 30 s and two refreshes per collect.

## Slice 2 (sketch only)

Ideas from the slice-1 TUI review, to pick from:

- Flash a row for a second when its status changes (working to done, a new `REPORT-COMPLETE`, a PR merged); a one-cell spinner or pulsing dot beside working rows.
- Sort or group BLOCKED first, then working, idle, done; dim finished rows.
- A per-row status sparkline or "last change 3m ago", from the snapshots already collected; age text that turns amber when a worker has been silent for an hour.
- A quiet green banner when everything lands ("N reports complete, M PRs green"); an optional terminal bell on a new NEEDS YOU, off by default.
- Set the terminal title (`surface watch: 1 need you`) so a blocked worker shows in a tab or dock.
- Tail pane: a "following" marker, and `Tab` or `f` to pin to the blocked worker's screen.

Earlier sketch:

A "fun interactive" view built on the same `workerwatch` snapshot:

- A timeline lane per worker: status transitions from `hc events` drawn as coloured spans over the session, with report and PR events as markers (the desk timeline in `internal/tui/desk_timeline.go` is the starting point).
- Light animation: a spinner per working worker, a pulse on a newly blocked one, a flourish when a report lands.
- Possibly a local web view served by forgectl (`docs serve` already has the server, auth token and SSE plumbing) for a second screen.
- Still read-only. Any action (attach, answer) stays an explicit `hc` command the view prints, never one it runs.
