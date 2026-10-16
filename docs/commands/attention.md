# forgectl attention — the attention queue for this machine

> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).

```sh
forgectl attention list                      # one line per wait, by delay cost
forgectl attention list --json               # {"rows": [...], "skipped": N, "sources": {...}}
forgectl attention list --session ID --json  # one session's rows
forgectl attention put --session ID --turn ID [--pane ID] [--socket PATH] [--cwd NAME] < reply.txt
forgectl attention put --session ID --turn ID --json < reply.txt   # echo the recorded items as JSON
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
- a session's `Action (Cameron)` whose leading verb is `run`, `bash`, `brew`, `restart` or `sudo`, when that session's pane queued a waiting desk item during or after the turn the action came from. Only a session herdr lists has a pane for this: a record's own `--pane` is a claim and never merges.

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
| `origin` | `worker` for rows from a chief-of-staff worker; `own` for a session herdr lists that is not a worker; `unverified` for a record whose session herdr does not list (nothing but the record vouches for it) |

`skipped` counts spool records and sweep entries dropped as malformed. `sources` reports `spool`, `desk`, `herdr` and `sweep` as `ok`, `absent` (nothing there yet), `error`, `malformed` or `unavailable` (herdr could not be asked).

**Worker origin.** A row is `origin: worker` when any name its session goes by starts with `[attention] worker_prefix` (default `cos-`). The names come from the session's herdr row (its worktree, and the basenames of its working directory and its pane's directory) and from the record's `--cwd`. The herdr names are checked so a worker cannot shed the tag by reporting a different `--cwd`, and a record whose session herdr does not list is never `own`: it is `unverified` (or `worker`, when its `--cwd` matches), so a worker cannot pass off a record under an unlisted session id as yours. **`own` is not proof of authorship.** It says the record names a session herdr lists and that session is not a worker; any process of yours, a worker included, can write a record under a listed session id (it replaces that session's record). Treat `origin` as a label for sorting and display, never as a reason to act. Desk rows take the origin of the herdr session in the pane that queued them; sweep rows from a worker are `worker`.

**Which pane a session is in.** A session's herdr row is the `herdr agent list` row whose `agent_session.value` is that session id. A Claude row with no `agent_session` falls back to its `session` display token, which the herdr-bridge mod sets; with herdr's own Claude integration uninstalled, the token is the only place herdr shows the id. When a row has both and they differ, `agent_session` wins and the token is not matched. A token that is not a valid session id is ignored. Neither field is proof. Any process of yours can report either one over the herdr socket. The token is the weaker of the two: herdr keeps one token map per pane, not one per reporter, so any program that sets display tokens on the pane (`forgectl surface tag --token session=…` included) replaces it, and herdr does not clear it when the agent exits. A wrong token can send Jump to the wrong pane, mark a record `own`, or merge a session's action into a desk item queued from that pane. It cannot run anything.

## The parser

`put` reads the last `What's next:` heading outside fenced code (bold, plain, or a `#` heading) and the list under it. Accepted item lines, with `-`, `*`, `+` or `1.` bullets:

- `**Question N**: …` and `**Action N (Cameron|Claude)**: …`, with the colon inside or outside the bold, or the same without bold;
- a bare actor, `**Cameron** — …` or `**Claude**: …` (an action numbered by its list number or position);
- a lettered option, `**C.** …` (read, not queued: it belongs to the question above it);
- any other bold lead, `**Nothing owed by me.** …` (read as a note, not queued).

A line that matches none of these counts as seen and not read. A line under an item with no blank line between continues that item. The block ends at a blank line followed by prose, a heading, a thematic break (`---`, `***`, `___`) or a fence. As in CommonMark, a backtick run followed by text that holds a backtick is an inline span, not a fence, and a fence closes only on a bare marker. `Recommended: X` on an item's line (or a continuation) is cut from its text and mapped; a value outside the map stays in the text and sets no `rec`. At most 20 items are kept. `put` prints `items=`, `seen=` and `read=`.

### `put --json`

With `--json`, a successful `put` prints exactly one JSON object on stdout and nothing else there:

```json
{
  "session": "s1",
  "turn": "t1",
  "items": [
    {"kind": "a", "n": 1, "actor": "Claude", "text": "do a thing.", "rec": "later"}
  ],
  "seen": 1,
  "read": 1
}
```

`items` holds the question (`q`) and action (`a`) items exactly as the record stores them: the same parse, the same cleaning and 200-character cut, the same order. Notes and options are stored in the record but are not rows, so they are left out. Every key is always present: `actor` and `rec` are `""` when the item has none, and `items` is `[]` when the reply had no block. `seen` and `read` are the parser's counts, as in the plain output.

The echo exists so a viewer can keep its own session's items in memory and compare spool rows against them: the spool can be rewritten by any process of yours after `put` returns, and the echo cannot. A refused `put --json` prints nothing on stdout and one `{"error", "code", "path"}` object on stderr, with the same exit code as without `--json`. Housekeeping notes (evict, prune) still go to stderr as plain lines.

stdin must not be a terminal. A reply over 64 KiB is parsed from its last 64 KiB, starting at its first whole line; a cut inside a fenced block can hide the header, which is accepted for replies that long. One over 16 MiB is refused. Once the record is written, `put`'s housekeeping (evicting and pruning) is best effort: a failure is reported on stderr and `put` still exits 0. It never prunes the record it just wrote, so a pane herdr has not detected yet does not lose its record.

### The corpus gate

```sh
ATTENTION_CORPUS=/path/to/corpus.jsonl go test ./internal/attention -run TestCorpus -v
```

The corpus is JSONL, one object per line whose `text` holds one full assistant reply. The test fails when the corpus has no block with a header, or when fewer than 98% of the item lines in its blocks parse. It logs counts only, never corpus text. Unset, it skips.

## The spool

Records live in `$XDG_STATE_HOME/cadence/attention` (`~/.local/state/cadence/attention`; a relative `XDG_STATE_HOME` is ignored). `put` creates the directory at `0700`; an existing one that is a symlink, is another user's, or has any permission bit beyond `0700` is refused and left as it is.

Each session has one file, `<session>.json`, mode `0600`. The session id must match `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$` and is used only as that file name. `put` writes a fresh `O_EXCL` temporary file in the spool, then renames it over the record; a record name that is a symlink or not a regular file is refused by `put` and `rm`.

**The spool is untrusted.** Any process running as you can write it, workers included. On every read, `list` re-checks every field (session, pane `^[A-Za-z0-9][A-Za-z0-9:_-]{0,63}$`, socket, directory name, turn, each item), refuses unknown fields, requires the record to name the session its file is named for, and strips and cuts every text again. A record that fails is skipped and counted, and its content is never printed. A record can still claim any session id, including a live one herdr lists: the queue trusts it for nothing beyond showing its text, and its `origin` is a label, not an authentication (see Worker origin).

## Pruning

`list`, and `put` after it writes, prune the spool:

- A record that has a `--pane` and whose `--socket` equals the caller's `HERDR_SOCKET_PATH` is judged by herdr alone. It stays while `herdr agent list` reports its pane, however old, and goes as soon as the pane is gone.
- Any other record (no socket, another herdr server, or herdr not answering) goes once it is older than `[attention] ttl_hours` (default 24).
- `put` also drops any other session's record for the same pane and socket, but only when herdr itself lists the putting session in that pane on the caller's socket: the pane now runs the new session (after `/clear`, or a new session in that pane).
- A malformed or refused record (a symlink or hardlink at a record name), and a leftover temporary file, are removed once older than `ttl_hours`, judged by the name's own mtime; a symlink is unlinked, never followed. Other names in the directory are left alone and counted as skipped.

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
| 1 | `put` or `rm`: the spool was refused (a symlink, another user's, wider than `0700`) or could not be written. `list` does not fail on a refused spool: it reports the desk and sweep rows, sets `sources.spool` to `error`, and names the reason on stderr |
| 2 | a flag or the `[attention]` config was refused, or `put`'s stdin is a terminal; nothing was written |
