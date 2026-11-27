# forgectl events — what changed on the PRs your sessions watch

> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).

```sh
forgectl events watch --session "$ID" "owner/repo#12"      # this session hears about #12
forgectl events unwatch --session "$ID"                  # stop watching everything
forgectl events poll --interval 90s                      # read watched PRs until interrupted
forgectl events feed --session "$ID" --since 40 --json   # {"events": [...], "last": N, "skipped": N}
forgectl events state "owner/repo#12" --json               # the last snapshot; exit 3 when there is none
```

`forgectl events` turns changes on watched pull requests into typed events, so a Claude Code session hears about them without polling. Unix and github.com only: `events poll` refuses to run when `[github].host` names another host.

## Who reads it

- **The c3 mod** reads its session's feed on a timer and, when a turn ends, submits the new events as one prompt. Nothing for the model to start or remember.
- **cadence-hooks' merge gate** reads `events state` before a `gh pr merge`: head, checks at head, review counts at head, CITL, allowed merge methods.

## Events

| Kind | When |
| --- | --- |
| `pr.pushed` | the head moved |
| `pr.checks` | the head's rollup is `SUCCESS`, `FAILURE` or `ERROR`, and it changed or the head moved |
| `pr.review` | another cadence-review marker from your own login counts at the head (`crit`, `imp` summed) |
| `pr.ready` | draft to ready for review |
| `pr.citl` | the `CITL` label went on |
| `pr.merged`, `pr.closed` | the PR is done; it comes off every watch list |

The first snapshot of a PR records nothing. Each event carries `seq`, `at`, `kind`, `repo`, `pr`, and where it applies `head`, `checks`, `crit` and `imp`. Nothing else: no title, body, comment or reviewer name, since a feed line reaches a model.

## Files

`$XDG_STATE_HOME/forgectl/events/` (else `~/.local/state/forgectl/events/`), mode 0700:

| File | Holds |
| --- | --- |
| `watch.json` | session id to the PRs it watches (at most 50 each) |
| `state.json` | each PR's last snapshot, and the last seq |
| `log.jsonl` | every event |
| `feed.<session>.jsonl` | one session's events |
| `lock` | the flock writers take exclusive and readers shared |

A log or feed past 1 MiB is cut to its newest 500 lines. Snapshots no session watches are kept for 7 days.

## Running the poller

Run `forgectl events poll` in a tmux or herdr pane, or under launchd. Each poll costs one GraphQL call per watched PR; `--interval` is at least 30s. `--once` polls once, for a cron line or a test.
