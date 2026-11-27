## Why

Claude Code sessions learn that a PR's checks finished, a review landed or a PR merged by polling: chief-of-staff's watcher scripts (`watch-peers.sh`, `watch-reports.sh`, `watch-drain.sh`, `ready-poll.sh`, `merge-watcher.sh`) or a model remembering to look. Each is a prose rule or an opt-in script, and each one depends on the model remembering to run it.

The operator asked for one cohesive, always-on layer instead. forgectl becomes the one place that watches GitHub and records what changed; the c3 mod hands a session its new events when a turn ends; cadence-hooks reads the same snapshot before it lets a merge through.

Tracking: CAM-1262 (executive assistant and per-project chiefs of staff, phase 2), CAM-1496 (reactive report watcher). Plan: "Making cadence more deterministic" (2026-11-27).

## What Changes

- **Watch lists.** `events watch --session ID <pr>...` and `events unwatch` keep which sessions watch which PRs, at most 50 per session.
- **A poller.** `events poll` reads each watched PR with one GraphQL call and records an event per change: `pr.pushed`, `pr.checks`, `pr.review`, `pr.ready`, `pr.citl`, `pr.merged`, `pr.closed`. A merged or closed PR comes off every watch list.
- **Typed events only.** Every field is an id, an enum, a SHA or a count, validated on write and again on read. No title, body, comment or pane text is stored, because a feed line reaches a model's context.
- **Feeds.** Each event goes to a machine log and to the feed of every session that watches its PR. `events feed --session ID --since N` reads a feed.
- **Snapshots.** `events state <pr>` prints the last snapshot: state, draft, head, check rollup, review counts at head, CITL, allowed merge methods, `polled_at`. No snapshot exits 3.

## Capabilities

### New Capabilities

- `events`: the watch lists, the poller, the event log and feeds, and the snapshot reader.

## Impact

- New module and verbs: `events watch|unwatch|poll|feed|state`.
- New directory `$XDG_STATE_HOME/forgectl/events` (else `~/.local/state/forgectl/events`), mode 0700, pinned with `privdir`; files 0600, opened no-follow. Unix only.
- New GitHub reads: one GraphQL query per watched PR per poll. No writes.
- Out of scope here: an MCP server with tasks. The Go MCP SDK (`v1.9.0-pre.1`) has no tasks extension yet; see design.md.
