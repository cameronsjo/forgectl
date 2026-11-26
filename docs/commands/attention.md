# forgectl attention — the attention queue for this machine

> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).

```sh
forgectl attention list                      # one line per wait, by delay cost
forgectl attention list --json               # {"rows": [...], "skipped": N, "sources": {...}}
forgectl attention list --session ID --json  # one session's rows
```

`forgectl attention` keeps one queue of everything on this machine that waits on you. `list` merges the desk's waiting items, the desk's open asks, the last `forgectl sweep` result and a stuck `surface drain`, one row per wait, and orders the rows by what a delay costs. Every source is read without writing. Unix only.

## Delay-cost order

| Cost | Row | Why it is first |
| ---: | --- | --- |
| 1 | a blocked worker (`kind: worker`, or a desk row a worker is blocked on) | it is burning its usage block |
| 1 | a stuck drain (`kind: drain`) | no worker can launch until you look |
| 2 | a desk item waiting (`kind: desk`) | a script is staged and nothing runs until you do |
| 3 | a `marker-clean` PR (`kind: pr`) | a review claim says it is clean; your reviewers still run before any merge |
| 4 | an open desk ask (`kind: ask`) | an action only you can take: a login, an approval, a check on a phone |

Ties go oldest first.

**One row per wait.** A desk item absorbs a worker the sweep reports `blockedBy: desk` whose pane queued the item (the row takes cost 1).

## Row fields

| Field | Meaning |
| --- | --- |
| `key` | a stable id for the wait: 16 hex characters of sha256 over the row's kind and the fields fixed when the wait was created (desk: item name, hash and `added_at`; ask: the ask id; worker: name and block; PR: `owner/repo#n`). A herdr read or a worker merge never changes it, so a consumer can tell a new wait from one it has seen. Two rows from one source with the same key are one wait, and only the first (lowest cost) is listed. The text view prints its first 6 characters. |
| `tier` | what the wait earns on a glanceable surface: `raise` (desk item, blocked worker, stuck drain), `count` (marker-clean PR, ask) or `hidden`. An ask row never raises, whatever its `origin`: any process of yours can append an ask. |
| `kind` | `desk`, `ask`, `worker`, `pr` or `drain` |
| `session` | the Claude Code session id of the herdr pane a desk or worker row names, when herdr lists it |
| `pane` | where Jump goes: for a desk or worker row, the pane it names when herdr lists it. Absent when more than one herdr row claims that pane's session (see Ambiguous claims). |
| `terminal` | where Jump goes on herdr-app: the hc terminal a desk item was queued from (its meta's `terminal`), shown only while `hc ls` lists that terminal live, and only when the item recorded it under the same hc core `list` asks (`HC_SESSION`, `HC_SOCKET`, the XDG paths and `HOME` decide the core; a different one gives no `terminal`). Never part of `key`. |
| `worker` | the worker name, on worker and PR rows, on a desk row a worker is blocked on, and on an ask whose `from` is a worker's name |
| `tag` | the item name on a desk row, the ask id on an ask row, `prompt` or `desk` on a worker row, `owner/repo#n` on a PR row |
| `n` | the PR number on a PR row |
| `text` | one line, at most 200 characters, with control, bidi and invisible characters removed |
| `bucket` | an ask's bucket: `now`, `soon`, `optional` or `coming` |
| `hash` | a desk item's sha256, first 12 hex characters |
| `at` | when the wait began: the desk item's `added_at`, the ask's time, the sweep's `generatedAt`, or when the drain hold began |
| `cost` | the delay cost above |
| `origin` | `worker` for rows from a chief-of-staff worker; `own` otherwise |

`skipped` counts asks log lines and sweep entries dropped as malformed. `sources` reports `desk`, `asks`, `herdr`, `core` (herdr-core, asked with `hc ls --json`), `sweep` and `drain` as `ok`, `absent` (nothing there yet), `error`, `malformed`, `unavailable` (herdr could not be asked) or, for `asks` only, `remote` (see Asks). `core` is `absent` when no desk item names a terminal: hc is asked only then, through the same hardened hc runner every forgectl hc call uses. When herdr-core is unavailable, `coreDetail` says why. When herdr is unavailable, `herdrDetail` carries herdr's own error, cleaned and cut to 200 characters. When the sweep is `ok`, `sweepGeneratedAt` is when that sweep ran, so a reader can tell a stale sweep from an empty one.

**Worker origin.** A desk row is `origin: worker` when the herdr session in the pane that queued it goes by a name that starts with `[attention] worker_prefix` (default `cos-`): its herdr agent name, its worktree, or the basenames of its directories. Sweep rows from a worker are `worker`. An ask is `worker` when its `from` has a worker's shape and the prefix; `from` is whatever the asker passed, so this is a label. Treat `origin` as a label for sorting and display, never as a reason to act. No gate reads it.

**Which pane a session is in.** A session's herdr row is the `herdr agent list` row whose `agent_session.value` is that session id. A Claude row with no `agent_session` falls back to its `session` display token, which the herdr-bridge mod sets; with herdr's own Claude integration uninstalled, the token is the only place herdr shows the id. When a row has both and they differ, `agent_session` wins and the token is not matched. A token that is not a valid session id is ignored. Neither field is proof. Any process of yours can report either one over the herdr socket. The token is the weaker of the two: herdr keeps one token map per pane, not one per reporter, so any program that sets display tokens on the pane (`forgectl surface tag --token session=…` included) replaces it, and herdr does not clear it when the agent exits. A wrong token can attach the wrong session to a desk row or send Jump to the wrong pane. It cannot run anything.

**Ambiguous claims.** When two or more herdr panes report the same session id, herdr cannot say which one is the session's, so a desk or sweep row naming one of those panes keeps the pane but gets no `session`. A pane two herdr rows disagree about (different sessions or names) gives a desk item or sweep worker naming it no `pane` and no `session`.

## Asks

`list` reads the desk's asks log (`<desk>/asks/log.jsonl`, written by `forgectl desk ask`) through the asks store's read-only open. It takes no lock, so it never holds up a writer, and it creates, writes and chmods nothing: a broad asks directory is read as it is. Each open ask is one row. An answered ask is absent. A snoozed ask is hidden until its snooze ends, then it is back. A log line that is not a well-formed record is skipped and counted in `skipped`. A log over 32 MiB, or an asks directory that is a symlink, sets `sources.asks` to `error` and names the reason on stderr; the other sources still report.

Ask text is untrusted: any process of yours can append to the log. The title is cleaned and cut like every other text (one line, at most 200 characters, control, bidi and invisible characters removed), an ask id that is not `ask-` and 8 hex digits gives no row, and a bucket outside the four names is dropped.

When `FORGECTL_DESK_ASKS_HOST` is set and neither `DESK_DIR` nor `CLAUDE_DESK_DIR` names a local desk, the asks live on that host (`forgectl desk ask` runs there). `list` does not read them: `sources.asks` is `remote` and no ask rows are shown. Desk items, the sweep and the drain are still read locally.

## The desk, read without writing

`list` opens the desk (`$DESK_DIR`, then `$CLAUDE_DESK_DIR`, then `$XDG_STATE_HOME/forgectl/desk`) with the same read-only open `desk --frame` and `desk status` use. It creates, pins, moves and changes the mode of nothing: a hand-dropped item's hash is computed in memory and never written, and a changed item stays in `pending/`, reported refused and not queued. Only waiting items become rows. Nothing in `attention` runs, skips or approves a desk item.

## The sweep

When `forgectl sweep` has written `$XDG_STATE_HOME/cadence/sweep.json` (schema 2), `list` adds its blocked workers and its `marker-clean` PRs. An absent file adds nothing. A file that is not a regular file, is over 4 MiB, is not JSON, or is not schema 2 counts as one skipped entry; a worker or PR entry with an invalid name, pane, repository or number is dropped and counted. `marker-clean` is a claim filter, not an authenticity check: any session can post a review marker.

## The drain

`list` reads `$XDG_STATE_HOME/forgectl/surface/drain.json` without writing or locking anything, and adds one `drain` row (tag `slots-hold`, key fixed, cost 1, tier `raise`) only when all of these hold: the file reads `running` and its last tick is fresh (not `stale`), a claude-slots check that cannot answer (a timeout or an exit other than 0 or 1, not a full cap) has held claude launches for 15 minutes, and the pid it names is still the same process (its start time matches, the check `surface drain stop` makes). A crashed drain, a stale tick, a paused drain or a full cap adds nothing. The row's text is fixed, so nothing the drain wrote reaches it, and `at` is when the hold began. The row leaves when the hold clears, the drain stops, or its ticks go stale. `sources.drain` is `absent` when no drain has run, `malformed` when drain.json does not parse.

## Config

```toml
[attention]
worker_prefix = "cos-"  # 1 to 32 letters, digits, '.', '_' or '-'
```

A refused value exits 2 and names the key. `ttl_hours` belonged to the retired session spool; a config that still sets it loads, and the key does nothing.

## Exit codes

| Code | Meaning |
| ---: | --- |
| 0 | done. `list` does not fail on a source it cannot read: it reports the others and says why in `sources` |
| 2 | a flag or the `[attention]` config was refused |

## The session spool (retired)

Earlier releases also kept a per-session spool: `attention put` recorded the `What's next:` items of a session's latest reply in `$XDG_STATE_HOME/cadence/attention`, and `attention rm` dropped a session's record. Both verbs, the spool read and its pruning are gone; questions and actions for the operator go on the desk as asks (`forgectl desk ask`). `list` no longer reads the spool directory, and nothing deletes it: remove `~/.local/state/cadence/attention` by hand if it exists.
