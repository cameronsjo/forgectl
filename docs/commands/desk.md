# desk

An operator queue for scripts an agent stages but will not run itself. The agent queues an item with `forgectl desk add`; a person reads it on the dashboard and presses `y` to run it or `s` to skip it. Each item's sha256 is fixed when it is queued, and an item whose bytes change afterwards is skipped as `changed` instead of run.

What the desk protects against, and what it does not, is [ADR-0012](../adr/0012-desk-threat-model.md) (defend against accidents, not against a same-uid process).

```bash
forgectl desk                                   # the dashboard (needs a terminal)
forgectl desk --frame                           # one frame to stdout, sized by $COLUMNS/$LINES
forgectl desk add ./fix.sh --what "..." --why "..."   # queue an item; prints name= and sha256=
forgectl desk plan nightly.manifest             # check a batch: waves, warnings, sha256
forgectl desk status                            # the queue, one line per item
forgectl desk status 17-fix --json              # one item in detail, as JSON
forgectl desk watch 17-fix --deadline 540       # stream its events; exit with the run's outcome
forgectl desk skip 17-fix --reason "superseded" # skip a waiting item, or clear a lost run
forgectl desk layout --progress 'CMD'           # herdr split: this pane left, the desk right, CMD below
forgectl desk prune --days 30                   # delete done/ and skipped/ items older than 30 days
```

## Requirements

- **A Unix system.** The desk is built on `openat`, `O_NOFOLLOW`, and process sessions. On other systems every verb exits 2 with `forgectl desk is not supported on this OS`.
- **A terminal, for the dashboard.** `forgectl desk` with no terminal on stdin and stdout exits 2 and names `--frame` and `desk status` instead. No other verb reads the terminal.
- **A herdr pane, for `layout`.** It needs `HERDR_ENV=1`, a `HERDR_SOCKET_PATH` naming a live socket, and `HERDR_PANE_ID`, which herdr sets in every pane it hosts.

## The agent workflow

1. Write the script to a file and queue it, with one line on what it does and one on why it needs a person:

   ```bash
   forgectl desk add ./merge-1201.sh --what "Merge PR 1201 once checks are green" --why "You own merges"
   ```

   It prints `name=`, `kind=` and `sha256=` lines.

2. Report the name and the full sha256 to the person. The dashboard shows the same hash, so they can confirm that what they approve is what you described.

3. Watch the run under a monitor. `watch` waits while the item is pending, prints each event line, and exits with the run's outcome:

   ```bash
   forgectl desk watch 17-merge-1201 --deadline 540
   ```

   On exit 75 the last line is `resume=forgectl desk watch 17-merge-1201 --skip N --deadline 540`. Run that line to pick up after the `N` event lines already printed.

4. Read the result with `forgectl desk status 17-merge-1201` (or `--json`): the exit code, the times, the log path, and for a batch each step's outcome.

## Commands

### `forgectl desk`

The dashboard: three stat tiles (waiting, runs today, outcomes), the queue with a bar per item, a focus panel showing the selected item's WHAT, WHY and first script lines, and the history of finished runs.

| Key | Action |
|---|---|
| `y` | run the selected item (a TTY item runs in this pane; anything else runs detached) |
| `s` | skip the selected item; asks first |
| `u` | undo the last skip |
| `v` | view the selected item's script |
| `l` | view the latest log |
| `a` | run every waiting item on screen, except TTY and changed items; asks first, and runs exactly the names and hashes shown |
| `j` / `k` | move |
| `q` | quit; detached runs keep running |

The dashboard rings the terminal bell when an item arrives, and again every 5 minutes while anything waits; inside a herdr pane it also sends a herdr notification. The window title reads `desk ● N waiting`.

`--frame` prints one frame to stdout and exits, sized by `$COLUMNS` and `$LINES` (80x40 when unset). Colour follows `NO_COLOR` and is dropped on a pipe. It reads the queue the same way `status` does.

### `forgectl desk add <file>`

| Flag | Meaning |
|---|---|
| `--what TEXT` | what the script does, one line; required and non-empty |
| `--why TEXT` | why it needs a person, one line; required and non-empty |
| `--tty` | the script needs a terminal (a password prompt, `sudo`); it runs in the dashboard's own pane |
| `--name FILE` | the file name for an item read from stdin (`<file>` is `-`), such as `deploy.sh` |
| `--json` | print `{name, kind, sha256, path, warnings}` |

The kind comes from the extension: `.sh` is a script, `.manifest` is a batch. The name is the next free `NN-` plus the file's base name. `# WHAT:`, `# WHY:` and (with `--tty`) `# TTY: yes` lines are inserted after a shebang before the hash is taken. A file that already carries one of those lines refuses the matching flag.

A `.manifest` is planned before it is queued: one that cannot run is refused, and its warnings are printed as `warning:` lines on stderr. A batch cannot be a TTY item.

When `<file>` is `-`, stdin is read (at most 1 MiB) and must not be a terminal.

Exit codes: 0 queued; 1 refused (an unreadable file, a manifest that cannot run, no free number); 2 a usage error (a missing or empty `--what` or `--why`, `--name` misused).

### `forgectl desk plan <name|file>`

Checks a batch manifest without running it. The argument is a file when it ends in `.manifest` or contains a `/`, and an item name otherwise (looked up in `pending/`, then `running/`, `done/` and `skipped/`).

```text
name=nightly.manifest
sha256=<64 hex digits>
steps=4
order=alpha,beta -> gamma -> delta
warning: gamma uses OUT_beta_key but beta is not an ancestor
```

`--json` prints `{name, sha256, steps: [{id, after, timeout, private}], waves, order, warnings}`.

Exit codes: 0 the manifest can run, with or without warnings; 1 it cannot, it is a script, or the item was not found; 2 a usage error.

### `forgectl desk status [name]`

Without a name, one line per item: waiting and running items first, then the 20 most recent done items, then skipped items. `--json` lists every item under `pending`, `running`, `done` and `skipped`, plus `dir` and `taken`.

```text
desk /home/me/.local/state/forgectl/desk: 1 waiting, 0 running, 2 done, 0 skipped
waiting  17-merge-1201  age=12m  sha256=3f1a9c0d2b7e  what="Merge PR 1201 once checks are green"
done     16-cleanup  age=1h  exit=0  took=41s
done     12-probe  age=3h  exit=1  took=18s
```

A waiting item older than 24 hours is flagged `stale`. A running item whose supervisor is gone with no `RUN-END` shows as `lost`. A legacy done item whose log has no `EXIT=` line shows `no-exit-recorded`.

With a name, the item in detail as `key=value` lines: `name`, `state`, `kind`, `what`, `why`, `tty`, the full `sha256`, `added`, `started`, `ended`, `exit`, `skip_reason` and `skip_note`, the `log` and `events` paths, and for a batch a `summary` line and one `step` line per step. `--json` prints `{item, log, events, record, steps, summary}`; `summary` is the run's `summary.json` once a batch has finished, and `null` before.

Each item in the JSON has `name`, `number`, `kind`, `state`, `what`, `why`, `tty`, `sha256`, `added_at`, `started_at`, `ended_at`, `age_seconds`, `duration_seconds`, `stale`, `exit_code`, `skip_reason`, `skip_note`, `refusal` and `pid`. Times are UTC. `age_seconds` counts from when the item was added (waiting), started (running) or ended (done).

Like the dashboard, `status` fixes the hash of a hand-dropped item the first time it sees it, and moves a pending item whose bytes changed to `skipped/`.

Exit codes: 0 shown; 1 no such item, or the desk could not be read; 2 a usage error.

### `forgectl desk watch <name>`

Prints the item's event lines as they arrive and exits when the run does. It reads no input.

| Flag | Meaning |
|---|---|
| `--deadline S` | stop after `S` seconds; `0` (the default) waits for the run |
| `--skip N` | leave out the first `N` event lines, the resume point an earlier watch printed |

| Code | Meaning |
|---|---|
| 0 | `RUN-END` with `rc=0` |
| 1 | `RUN-END` with another rc; `RUN-LOST`; the item was skipped; or no such item |
| 2 | a usage error |
| 75 | the deadline passed first; the last line is `resume=forgectl desk watch NAME --skip N` (plus `--deadline` and `--dir` when they were given) |
| 130 | interrupted; the last line is the same `resume=` line |

### `forgectl desk skip <name> --reason <text>`

Moves a waiting item to `skipped/` (`reason=operator`; the dashboard's `u` can bring it back), or a lost run out of `running/` (`reason=lost`; it cannot be re-armed). This is the only way a lost run leaves `running/`. A live run is refused. The `--reason` text, one line of at most 200 characters, is kept as the item's `skip_note`.

Exit codes: 0 skipped; 1 no such item, the item is running, or another desk claimed it first; 2 a usage error.

### `forgectl desk layout`

Splits the current herdr tab around this pane: the desk on the right, about `--width` columns wide (default 66), and with `--progress CMD`, `CMD` in a pane below the desk. The new panes are named `desk` and `progress`, and this pane keeps the focus.

- herdr's `--ratio` is the share the original pane keeps, so the ratio is `1 - width/tab width`, clamped to 0.45–0.8. The progress pane is split off the desk with the desk keeping 40%.
- The desk pane runs `forgectl desk --dir <the resolved desk directory>`, shell-quoted. `CMD` is typed into the progress pane's shell as given, so quote it for a shell. Both must be one line.
- Each pane is found again by its terminal id right before each call that names it, because herdr renumbers pane ids.
- It prints `desk=<pane id>`, `progress=<pane id>`, and `columns=<n> of <tab width>`.

Exit codes: 0 laid out; 1 a herdr call failed, and the panes made so far stay; 2 a usage error, or not in a herdr pane.

### `forgectl desk prune`

Deletes the protocol files (`.sh`, `.manifest`, `.log`, `.events`, `.meta.json`, `.d/`) of items in `done/` and `skipped/` whose newest file is older than `--days` (default 30). It never touches `pending/`, `running/`, unknown files, symlinks, or the desk root. Nothing else deletes desk files. `--json` prints `{removed, days}`.

Exit codes: 0 pruned (perhaps nothing); 1 a delete failed; 2 `--days` below 1.

## The queue protocol

### Directory

The desk directory is `--dir`, else `$DESK_DIR`, else `$CLAUDE_DESK_DIR`, else `$XDG_STATE_HOME/forgectl/desk`, else `~/.local/state/forgectl/desk`. A relative `DESK_DIR` or `CLAUDE_DESK_DIR` is refused. The first open makes the directory and its four protocol subdirectories `0700` and their files `0600`. Anything else at the desk root is ignored and never touched.

| Path | Holds |
|---|---|
| `pending/NN-name.sh` | a script waiting for approval |
| `pending/NN-name.manifest` | a batch waiting for approval |
| `running/NN-name.sh` or `.manifest` | the verified copy of what is running |
| `done/NN-name.sh` or `.manifest` | what ran |
| `done/NN-name.log` | the run's output; the last line is `EXIT=<rc>` |
| `done/NN-name.events` | the run's event lines |
| `done/NN-name.d/` | a batch's `steps/<id>.log`, `steps/<id>.out`, `status.tsv` and `summary.json` |
| `skipped/NN-name.sh` or `.manifest` | a skipped item |
| `<dir>/NN-name.meta.json` | the item's metadata; it moves with the item |

### Items

A script carries its header lines anywhere in the file (the first of each wins):

```bash
#!/bin/bash
# WHAT: Merge PR 1201 once checks are green
# WHY: You own merges
# TTY: yes
gh pr merge 1201 --squash --auto
```

`# TTY: yes` makes it a TTY item, which runs in the dashboard's own pane through `script(1)` so prompts work. Everything else runs detached, under a supervisor, with stdin from `/dev/null`, and keeps running when the dashboard quits.

An agent may also drop a file into `pending/` by hand. Its hash is fixed the first time a desk sees it, and its file time stands in for when it was added.

### Batch manifests

One step per line; `#` comments and blank lines are ignored:

```text
<step> [after=a,b] [timeout=S] [private] -- <command>
```

- A step id is lowercase letters and digits, starting with a letter, at most 32 characters.
- `after=` names the steps this one waits for. Steps run in waves, in manifest order, at most 4 at a time.
- `timeout=S` sends SIGTERM to the step's process group after `S` seconds, then SIGKILL 5 seconds later; the step's rc is 124.
- A failed step skips every step that depends on it.
- A step writes `key=value` lines to `$STEP_OUT`; every step that depends on it sees them as `OUT_<step>_<key>`.
- `private` keeps the step's output out of the combined log and replaces its `STEP_OUT` values of 6 or more characters in later output with `<redacted:OUT_<step>_<key>>`. Redaction reduces exposure; it is not a guarantee (see [ADR-0012](../adr/0012-desk-threat-model.md)).
- Everything after the first ` -- ` goes to `/bin/bash -c` as it is.

The batch ends with rc 0 (every step ok), 1 (a step failed), 2 (the batch could not run), or 130 (interrupted).

### Metadata

`NN-name.meta.json` holds `added_at`, `sha256`, `kind`, `skip_reason`, `skip_note`, `started_at`, `ended_at`, `exit_code`, and the owning process's `pid` and `pid_start`. A legacy item with none falls back to its log's times and its log's `EXIT=` line.

`skip_reason` is `operator` (skipped by a person; can be undone), `changed` (its bytes changed after it was queued), `lost` (its run's owner died), `name-reused` (its number was already in `done/`), or `refused: …` (not a regular file with one link). Only `operator` can be undone.

### Events

`done/NN-name.events` holds one line per event. Only the desk writes it; step output never reaches it.

| Line | When |
|---|---|
| `RUN-START id=<name> pid=<pid> [steps=<n> jobs=<n>]` | the run started |
| `STEP-START id=<step> deps=<a,b>` | a batch step started |
| `STEP-END id=<step> rc=<rc> dur=<s> reason=<r> outputs=<keys> log=<path>` | a batch step ended |
| `STEP-SKIP id=<step> reason=<r>` | a batch step was skipped |
| `STEP-WARN id=<step> msg=<text>` | a batch step's output could not be read cleanly |
| `RUN-END rc=<rc> reason=<r> [ok=<n> failed=<n> skipped=<n>]` | the run ended; always the last line |
| `RUN-LOST id=<name> pid=<pid>` | printed by `watch`, never written: the owner is gone with no `RUN-END` |
