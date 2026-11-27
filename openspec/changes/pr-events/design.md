## Context

The c3 mod is the event pump: Claude Code channels (push into a session) are a research preview the operator cannot use, and the Monitor tool has to be started by the model and stops after 30 minutes. c3 already submits prompts on its own, so it reads this store's feed on a timer and submits new events when a turn ends.

## Decisions

- **Files, not a daemon socket.** The poller writes; readers read files under a shared flock. Nothing has to be running for a reader to work: an absent store reads as empty, a missing snapshot as unknown.
- **The poller takes the lock once per poll.** GitHub reads run with no lock held, so a slow answer never blocks `watch` or a reader.
- **First look is not news.** Watching a PR records its snapshot and emits nothing. A rollup going back to pending emits nothing; the push that caused it does.
- **Only the viewer's markers count**, summed by sweep's rule, as `surface watch` and `sweep` count them.
- **Bounds.** 50 PRs per session; a log or feed past 1 MiB is cut to its newest 500 lines; a JSON file past 4 MiB is refused.

## Not built: MCP tasks

The plan has `forgectl mcp` serve `wait_for_checks` and `merge_when_green` as MCP tasks, which Claude Code polls and whose completion wakes the session. The Go MCP SDK has no tasks extension as of `v1.9.0-pre.1`, and the 2026-07-28 tasks extension's wire shapes are not pinned anywhere this repo can test against. That part waits for SDK support, or for a hand-written server with a conformance fixture. The feed covers the same wake-ups without the model starting anything.
