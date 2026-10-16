# forgectl attention — the attention queue for this machine

> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).

```sh
forgectl attention list                      # one line per wait, by delay cost
forgectl attention list --json               # {"rows": [...], "skipped": N, "sources": {...}}
forgectl attention list --session ID --json  # one session's rows
forgectl attention put --session ID --turn ID [--pane ID] [--socket PATH] [--cwd NAME] < reply.txt
forgectl attention rm --session ID
```

`forgectl attention` keeps one queue of everything on this machine that waits on you. Each Claude Code session publishes the `What's next:` items of its latest reply with `put` (the steward mod does this after every turn). `list` merges those items with the desk's waiting items and the last `forgectl sweep` result, one row per wait, and orders the rows by what a delay costs. Unix only.

## Delay-cost order

| Cost | Row | Why it is first |
| ---: | --- | --- |
| 1 | a blocked worker (`kind: worker`, or a desk row a worker is blocked on) | it is burning its usage block |
| 2 | a desk item waiting (`kind: desk`) | a script is staged and nothing runs until you do |
| 3 | a `marker-clean` PR (`kind: pr`) | a review claim says it is clean; your reviewers still run before any merge |
| 4 | an `Action (Cameron)` (`kind: a`) | a physical step only you can take |
| 5 | a question (`kind: q`) | a session waits on an answer |
| 6 | an action Claude offers (`Action (Claude)`, or no actor) | it waits on a go |

Ties go oldest first.

**One row per wait.** A desk item absorbs the waits that are the same wait:

- a worker the sweep reports `blockedBy: desk` whose pane queued the item (the row takes cost 1);
- a session's `Action (Cameron)` whose leading verb is `run`, `bash`, `brew`, `restart` or `sudo`, when that session's pane queued a waiting desk item during or after the turn the action came from.

A run-type `Action (Cameron)` with no such desk item gets `notOnDesk: true`: the step was asked for in prose instead of being staged on the desk.

## Row fields

| Field | Meaning |
| --- | --- |
| `kind` | `q`, `a`, `desk`, `worker` or `pr` |
| `session` | the Claude Code session id the row belongs to, when known |
| `pane` | where Jump goes: the herdr pane `herdr agent list` reports for the session (or, for a desk or worker row, the pane it names when herdr lists it). Never a pane read from a record. Absent when the session is not in herdr. |
| `worker` | the worker name, on worker and PR rows and on a desk row a worker is blocked on |
| `tag` | `Q` or `A` on an item row, the item name on a desk row, `prompt` or `desk` on a worker row, `owner/repo#n` on a PR row |
| `n` | the item number (the PR number on a PR row) |
| `actor` | `Cameron` or `Claude` on an action |
| `text` | one line, at most 200 characters, with control, bidi and invisible characters removed |
| `rec` | the `Recommended:` value mapped to `now`, `later`, `park` or `forget` (`go` and `yes` read as `now`, `defer` as `later`, `leave` as `forget`) |
| `turn` | the turn an item row came from |
| `hash` | a desk item's sha256, first 12 hex characters |
| `at` | when the wait began: the record's write time, the desk item's `added_at`, or the sweep's `generatedAt` |
| `cost` | the delay cost above |
| `notOnDesk` | see above |
| `origin` | `worker` for rows from a chief-of-staff worker, else `own` |

`skipped` counts spool records and sweep entries dropped as malformed. `sources` reports `spool`, `desk`, `herdr` and `sweep` as `ok`, `absent` (nothing there yet), `error`, `malformed` or `unavailable` (herdr could not be asked).

**Worker origin.** A row is `origin: worker` when any name its session goes by starts with `[attention] worker_prefix` (default `cos-`). The names come from the session's herdr row (its worktree, and the basenames of its working directory and its pane's directory) and from the record's `--cwd`. The herdr names are checked so a worker cannot shed the tag by reporting a different `--cwd`; the record's name is checked so a worker outside herdr is still tagged.

## The parser

`put` reads the last `What's next:` heading outside fenced code (bold, plain, or a `#` heading) and the list under it. Accepted item lines, with `-`, `*`, `+` or `1.` bullets:

- `**Question N**: …` and `**Action N (Cameron|Claude)**: …`, with the colon inside or outside the bold, or the same without bold;
- a bare actor, `**Cameron** — …` or `**Claude**: …` (an action numbered by its list number or position);
- a lettered option, `**C.** …` (read, not queued: it belongs to the question above it);
- any other bold lead, `**Nothing owed by me.** …` (read as a note, not queued).

A line that matches none of these counts as seen and not read. A line under an item with no blank line between continues that item. The block ends at a blank line followed by prose, a heading or a fence. `Recommended: X` on an item's line (or a continuation) is cut from its text and mapped. At most 20 items are kept. `put` prints `items=`, `seen=` and `read=`.

stdin must not be a terminal. A reply over 64 KiB is parsed from its last 64 KiB; one over 16 MiB is refused.

### The corpus gate

```sh
ATTENTION_CORPUS=/path/to/corpus.jsonl go test ./internal/attention -run TestCorpus -v
```

The corpus is JSONL, one object per line whose `text` holds one full assistant reply. The test fails when the corpus has no block with a header, or when fewer than 98% of the item lines in its blocks parse. It logs counts only, never corpus text. Unset, it skips.

## The spool

Records live in `$XDG_STATE_HOME/cadence/attention` (`~/.local/state/cadence/attention`; a relative `XDG_STATE_HOME` is ignored). `put` creates the directory at `0700`; an existing one that is a symlink, is another user's, or has any permission bit beyond `0700` is refused and left as it is.

Each session has one file, `<session>.json`, mode `0600`. The session id must match `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$` and is used only as that file name. `put` writes a fresh `O_EXCL` temporary file in the spool, then renames it over the record; a record name that is a symlink or not a regular file is refused by `put` and `rm`.

**The spool is untrusted.** Any process running as you can write it, workers included. On every read, `list` re-checks every field (session, pane `^[A-Za-z0-9][A-Za-z0-9:_-]{0,63}$`, socket, directory name, turn, each item), refuses unknown fields, requires the record to name the session its file is named for, and strips and cuts every text again. A record that fails is skipped and counted, and its content is never printed. A record can still claim any session id: the queue trusts it for nothing beyond showing its text.

## Pruning

`list`, and `put` after it writes, prune the spool:

- A record whose `--socket` equals the caller's `HERDR_SOCKET_PATH` is judged by herdr alone. It stays while `herdr agent list` reports its pane, however old, and goes as soon as the pane is gone.
- Any other record (no socket, another herdr server, or herdr not answering) goes once it is older than `[attention] ttl_hours` (default 24).
- `put` also drops any other session's record for the same pane and socket: the pane now runs the new session (after `/clear`, or a new session in that pane).
- A malformed record, and a leftover temporary file, are removed once older than `ttl_hours`. Other names in the directory are left alone and counted as skipped.

## The desk, read without writing

`list` opens the desk (`$DESK_DIR`, then `$CLAUDE_DESK_DIR`, then `$XDG_STATE_HOME/forgectl/desk`) with the same read-only open `desk --frame` and `desk status` use. It creates, pins, moves and changes the mode of nothing: a hand-dropped item's hash is computed in memory and never written, and a changed item stays in `pending/`, reported refused and not queued. Only waiting items become rows. Nothing in `attention` runs, skips or approves a desk item.

## The sweep

When `forgectl sweep` has written `$XDG_STATE_HOME/cadence/sweep.json` (schema 2), `list` adds its blocked workers and its `marker-clean` PRs. An absent file adds nothing. A file that is not a regular file, is over 4 MiB, is not JSON, or is not schema 2 counts as one skipped entry; a worker or PR entry with an invalid name, pane, repository or number is dropped and counted. `marker-clean` is a claim filter, not an authenticity check: any session can post a review marker.

## Config

```toml
[attention]
ttl_hours = 24          # 1 to 720
worker_prefix = "cos-"  # 1 to 32 letters, digits, '.', '_' or '-'
```

A value out of range exits 2 and names the key.

## Exit codes

| Code | Meaning |
| ---: | --- |
| 0 | done |
| 1 | the spool was refused (a symlink, another user's, wider than `0700`) or could not be written |
| 2 | a flag or the `[attention]` config was refused, or `put`'s stdin is a terminal; nothing was written |
