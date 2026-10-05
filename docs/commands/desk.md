# forgectl desk — run viewer and approval queue

> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).

```sh
forgectl desk                                                       # full-screen dashboard: the desk queue and the desk's runs
forgectl desk --spec ~/pipeline.toml                                # add the runs a run spec describes (repeatable)
forgectl desk --no-icons                                            # ASCII glyphs instead of icon glyphs
forgectl desk runs --json                                           # every run from every source, as JSON (no TTY needed)
forgectl desk show 07-build                                         # one run's flow as plain text
forgectl desk show 07-build --at 120 --json                         # the run's state after its first 120 events
forgectl desk show nightly --from pipeline --spec ~/pipeline.toml   # pick the source when two sources hold a run of that name
```

`forgectl desk` has two jobs:

- **The approval queue.** Claude stages scripts and batches it will not run itself in the desk directory. The operator reads each one, then approves it, skips it, or runs it in the terminal.
- **The run viewer.** It shows any multi-step run as it moves through its steps: a top-down flow with the gate and the checks, the parsed events, a timeline, metrics, and a summary. You can switch between runs while they are live and replay a finished run.

Runs come from three kinds of **source**:

- **The desk**, always present. Its items and batches are runs. The desk directory is `$DESK_DIR`, else `$CLAUDE_DESK_DIR`, else `$XDG_STATE_HOME/forgectl/desk`, else `~/.local/state/forgectl/desk`.
- **A JSONL event log** for each run directory that a run spec matches. Pass the spec with `--spec`.
- **A summary-only JSON result** that a run spec matches and that has no run directory of the same name. A summary is written once its run is over, so a summary-only run reads `ended`.

The desk is Unix-only. Design and trust model: [ADR-0010](../adr/0010-desk-run-sources.md) (desk run sources and the approval queue).

## Quickstart

1. Write a run spec for your own runs, or skip this step to see only the desk. The [spec reference](#run-spec-reference-experimental) below has a complete example.
2. Point the spec's root variable at the directory that holds the runs:

   ```sh
   export PIPELINE_ROOT="$HOME/pipeline"
   ```

3. Open the dashboard:

   ```sh
   forgectl desk --spec ~/pipeline.toml
   ```

   It opens on the queue (view 6) when items are waiting, else on the newest live run.

4. Press `?` for the keys and the glyph legend. Press `q` to quit. Quitting leaves running jobs running.

From a script or an agent, use `desk runs --json` and `desk show <run> --json` instead. The dashboard refuses to start without a terminal (see [Exit codes](#exit-codes)).

## Glyph legend

Every state has a glyph, an ASCII fallback for `--no-icons`, and a word. Colour is never the only signal: each coloured glyph has its word beside it.

| State | Glyph | ASCII | Word |
|---|---|---|---|
| pending / waiting | `·` | `.` | `pending` / `waiting` |
| running / live | `◐` (spins) | `*` | `running` / `live` |
| closed / ended ok | `✓` | `+` | `done` / `ok` |
| failed / exit not 0 | `✗` | `x` | `failed` / the exit code |
| interrupted (a step still running when its run ended) | `⊘` | `/` | `interrupted` |
| rejected | `↺` | `r` | `rej N/M` |
| resumed | `⟲` | `~` | `resumed` |
| stale | `!` | `!` | `stale` |
| lost | `?` | `?` | `lost` |
| skipped | `–` | `-` | `skipped:<reason>` |

Event rows carry a tag column: `start`, `ok`, `FAIL`, `rej`, `rst` (restart), `end`, `chk` (check), or `·` for an event the spec does not map.

A run row shows the glyph, a word or exit code, the name, and the time since the last event:

```text
◐ live    alpha-…-7     12s
✓ ok      beta-…-7       4m
✗ 3       gamma-…-7      1h
! stale   nightly-b3     2h
? lost    batch-04       3h
```

- Long names are cut in the middle (`head…tail`), so a suffix such as `attempt1-retry` stays visible.
- A source tag is added only when two sources hold a run of the same name.
- Live runs come first, then the rest by last-event time, newest first.
- The selection follows the run (its source and name), not its row, so a reload never moves you to a different run.
- A run with no log yet shows `· new`. A run that is no longer listed shows `gone` and keeps its last state.

## Views

| Key | View | Shows |
|---|---|---|
| `1` | flow | The steps top to bottom, with the gate (`rej 2/3` beside its loop) and the checks. A desk batch draws one wave of parallel steps per row band |
| `2` | events | One row per event: tag, step, and the fields `[log] show` lists. Follows new events |
| `3` | timeline | Each step's attempts as bars, with restart marks |
| `4` | metrics | Sparklines and totals for the `[metrics] sum` fields, and the error count |
| `5` | summary | The summary JSON as a key tree |
| `6` | queue | Desk items by state, with the skip reason and a stale mark |

The flow is drawn in full when `3 + 6·bands + 7` rows fit, where a band is one row of steps (one step in a chain, one wave in a batch). Otherwise it is compact, with two rows per step. Either form scrolls and follows the running step.

## Layout

The terminal's width picks the layout:

| Columns | Layout |
|---|---|
| 150 or more | run list (30 columns) │ flow (62) │ side view |
| 100–149 | a run strip in the header (`‹ 3/10 beta-… ◐ live 12s ›`); flow (62) │ side view |
| 40–99 | one pane, with a tab strip `1 flow 2 events 3 timeline 4 metrics 5 summary 6 queue` |
| under 40, or under 12 rows | `terminal too small (needs 40×12) — resize or press q` |

In the two wide layouts, `1` focuses the flow and `2`–`6` pick what the side pane shows. Modals are centred, cover at most 80% of each dimension and at least 36×8, scroll, and re-lay out on resize.

## Keys and mouse

The footer lists the keys that apply in the current mode. `?` shows all of them with the glyph legend.

| Input | Action |
|---|---|
| `[` `]` or `<` `>` | Previous or next run |
| `o` | Run switcher: a filterable list of full rows. `enter` picks, `esc` closes |
| `1`–`6` | Pick a view |
| `tab` | Next pane (moves focus only; never changes the view) |
| `↑` `↓` or `k` `j` | Move in the focused pane |
| mouse wheel | Scroll the pane under the pointer |
| `g` / `G` | Top / bottom. `G` also turns follow back on |
| click | Select a row. A second click on the selected row opens it |
| `enter` | Open the selected row: a step modal (that step's events, attempts, rejects and duration, updated live) or an event modal (every field, wrapped). `enter` on an event inside a step modal opens it on top |
| `p` | Toggle replay. The header shows `REPLAY i/N · +M new · by time` or `by event`. `←` `→` step one event, `space` plays at 4 events a second, `g` / `G` jump to the start / end |
| `a` · `s` · `u` | Approve · skip · unskip, in the queue view. Outside it, the footer says `approve is in the queue (6)` |
| `/` | Filter the run list (widest layout) or the events (other layouts). `enter` keeps the filter, `esc` clears it |
| `r` · `R` | Refresh the selected run · refresh everything. Refreshes are at least 15 s apart; the footer says when one is declined |
| `m` | Mouse capture on or off. With it on, hold Shift and drag to select text |
| `esc` | Close one level: modal, then replay, then filter |
| `q` | Close a modal, or quit. Quitting leaves running jobs running; the footer says so (`q quit; jobs keep running`) |
| `ctrl+c` | Quit |

### Approving an item

`a` on a waiting item opens a confirm modal on a frozen copy of the item. The modal shows:

- the title: run `<name>` now, in the background or in this terminal;
- the kind, the `# WHAT:` and `# WHY:` lines, and `N lines · M bytes · K escaped runes`, with invisible and non-ASCII runes counted on their own and the lines that hold them marked;
- the body, undimmed, with line numbers, wrapped and scrollable, and an `N more lines ↓` hint;
- for a batch, `hash covers the manifest's lines, not the scripts it calls`.

`a` reads the item again before the modal opens, and the modal opens only if its bytes still have the hash the queue listed. Keys typed while it reads go to the queue view, not the modal.

Only `y` confirms, and only when all of these hold:

- the key is not a repeat (the dashboard asks the terminal to report repeats; not every terminal does);
- it arrives at least 250 ms after the key event before it, so a held `y` or keys typed together never confirm, on any terminal;
- at least 400 ms have passed since the modal opened;
- the body has been scrolled to the end (or fits on screen), and at least 400 ms have passed since its last line first came on screen.

`enter` and a click never confirm. `n` or `esc` cancels. If a refresh no longer lists the item, or lists it with a different hash, the modal closes with `changed — re-review`. A refresh that cannot read the queue keeps the items it listed last and leaves the modal open.

- **A background item** is claimed with the full hash shown in the modal and handed to a detached supervisor. If the claim fails after it was recorded, the item moves to `skipped/` with reason `claim-failed`; if the start fails after the claim, with reason `launch-failed`.
- **A `# TTY: yes` item** runs in this terminal. The dashboard closes, the item runs in the foreground with its signals forwarded, and the command prints `ran <name>: exit N (<meaning>) — press any key`. The dashboard then reopens on that run with your view restored.
  - Ctrl-Z does nothing to the item: `SIGTSTP`, `SIGTTIN` and `SIGTTOU` are ignored in its process group, because a stopped group would leave the desk waiting with no terminal to resume it from.
  - Ctrl-C reaches the item, not the desk. An item killed by `INT`, `TERM`, `HUP` or `QUIT` is recorded as `interrupted`.
  - When the item exits, anything it left running in its process group is ended (`SIGTERM`, then `SIGKILL` after the grace period) before the run is recorded.
  - An item that cannot start prints `desk: <name>: <cause> — press any key` and waits. If the desk cannot take the terminal back, it records the run and exits.

`s` skips a waiting or `lost` item. For a waiting item the footer shows `skipped <name> · u to undo`; for a `lost` item it shows `skipped <name>`. `u` re-arms only an item skipped with reason `operator` that was never claimed. A skipped `lost` item was claimed, so `u` cannot undo it, and the queue does not offer it. An item skipped as `changed` cannot be re-armed either, because its hash is fixed when it is queued. Queue either one again to run it.

## States

| State | Shown as |
|---|---|
| Loading | `loading runs…` |
| No sources | `no sources: pass --spec` |
| A source cannot list | `cannot list <source>: <cause>`; a root variable that is unset reads `$PIPELINE_ROOT is unset` |
| No runs | `no runs match runs/*` (the spec's `runs` pattern), `no runs: every source failed (? for detail)`, or `no runs yet`. The queue view shows `queue empty` |
| Bad spec | Header chip `1 source failed`; `?` shows the path, the TOML key, and the cause. The other sources still load |
| Log missing or unreadable | `no log yet` for a new run, or `cannot read out/events.jsonl: <cause>` |
| Partial ingest | Header chip `partial: over 32 MiB cap`, or counts such as `3 lines dropped · 2 unknown steps · 1 bad exit` |
| Run vanished | `gone: no longer listed; showing the last state` |
| Log replaced or shrunk | `log restarted`; the run is read again from the start |
| Stale | `stale: no events for 14m (limit 10m)` |
| Lost | `lost: supervisor died — s to skip` |
| Queue waiting | Header badge `queue: 3 waiting · lost: 1` |
| Follow paused | `paused · N new · G to follow` |

A run is `ended` once its end event arrives (and no restart follows it), `live` while its last event is within `[live] stale_after`, and `stale` after that. Desk items use the desk's own states: `waiting`, `running`, `ended`, `lost`, and `skipped`. A desk item is `lost` when its supervisor died, or when it was claimed and its claimer died before the supervisor started.

## Run spec reference (experimental)

> **Experimental.** The run spec format may change in any release, with no deprecation period. [ADR-0010](../adr/0010-desk-run-sources.md) records why.

A run spec is a TOML file that tells the dashboard where a family of runs lives and how to read their event logs. forgectl knows nothing about any particular tool's logs; the spec carries that knowledge. The spec is trusted operator config. The run directories and the files in them are not trusted (see [What the sources read](#what-the-sources-read)).

```toml
name = "pipeline"
root_env = "PIPELINE_ROOT"            # names the variable that holds the absolute root
runs = "runs/*"                       # each match is one run directory
summaries = "results/*.summary.json"  # the text '*' matched is the run name

[log]
path = "out/events.jsonl"             # relative to the run directory
time = "at"
event = "kind"
step = "stage"
attempt = "try"                       # optional; unset means each start adds 1
show = ["stage", "try", "rc", "err"]

[[steps]]
id = "fetch"
note = "pull the inputs"
[[steps]]
id = "build"
note = "compile and package"

[gate]                                # applies to every step
label = "accepted?"
max_rejects = 3

[on]                                  # event name -> action
stage_started = "start"
stage_done = "close"
stage_refused = "reject"
stage_gave_up = "fail"
worker_boot = "restart"               # the first one, before any step began, is the initial start
run_finished = "end"

[end]
exit_field = "rc"                     # an integer 0–255

[[checks]]
id = "audit"
event = "audit_result"
field = "rc"
label = "audit ok?"
fail_exit = 3

[exits]                               # TOML keys are strings; read as integers 0–255
"0" = "finished, audit passed"
"3" = "audit rejected"

[metrics]
path = "out/usage.jsonl"
sum = ["in", "out"]
error_when = { status = "error" }     # exactly one key

[live]
stale_after = "10m"
```

`forgectl desk` reads a spec when it starts. A spec that fails to load is reported with its path, the TOML key, and the problem, as `run spec "<file>": <key>: <problem>`. Any key the reader does not know is an error, so a misspelled key is reported rather than ignored.

### Top level

| Key | Required | Meaning and constraints |
|---|---|---|
| `name` | yes | The source name. It tags runs when two sources share a run name, and `desk show --from` takes it |
| `root_env` | yes | The name of an environment variable. Its value must be an absolute path: the root that every pattern is resolved under |
| `runs` | yes | A pattern with exactly one `*`. Each match is one run directory. A match must be a real directory, not a symlink |
| `summaries` | no | A pattern with exactly one `*`. The text the `*` matched is the run name. A summary whose name matches a run directory joins that run; one with no run directory is a summary-only run |

**Patterns** (`runs`, `summaries`, `log.path`, `metrics.path`) are relative paths. A pattern must not start with `/` or `\`, contain `:`, contain a `..` component, or use `?` or `[`. `*` is the only wildcard, and only `runs` and `summaries` take one: `log.path` and `metrics.path` are opened as written, so they may not contain `*`. A run or summary name is the text `*` matched, and it must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`; other names are not listed.

### `[log]`

| Key | Required | Meaning and constraints |
|---|---|---|
| `path` | yes | The event log, relative to the run directory. At most one `*` |
| `event` | yes | The JSON key that holds the event name. `[on]` and `[[checks]]` map event names |
| `time` | no | The JSON key that holds the event time: RFC 3339 text or integer epoch seconds. Anything else counts as no time |
| `step` | no | The JSON key that holds the step id |
| `attempt` | no | The JSON key that holds the attempt number. Unset, each `start` of a step adds 1 |
| `show` | no | The fields an event row shows, in this order. Empty means every field. The step field is not repeated, because it has its own column |

### `[[steps]]`

At least one step is required.

| Key | Required | Meaning and constraints |
|---|---|---|
| `id` | yes | Unique among the steps |
| `note` | no | One line shown under the step |
| `after` | no | Step ids this step follows. Each must name a step declared earlier in the file, so the steps cannot form a cycle. With no `after` on any step, the steps form a chain in file order |

### `[gate]`

| Key | Required | Meaning and constraints |
|---|---|---|
| `label` | no | The gate's text in the flow |
| `max_rejects` | when `[gate]` is present | At least 1. The flow shows `rej N/max_rejects`. Reaching it does not fail the step; only a `fail` event does |

### `[on]`

Each key is an event name, and each value is one of the actions below. An event the table does not name stays in the event list and changes nothing.

| Action | Effect |
|---|---|
| `start` | The step becomes `running`. The attempt comes from `[log] attempt`, else goes up by 1. The step is marked `resumed` when a restart happened since its previous start. A step id the spec does not declare is counted as an unknown step |
| `close` | A `running` step becomes `closed` |
| `reject` | The step's reject count goes up by 1; its state does not change |
| `fail` | The step becomes `failed` |
| `restart` | The first restart before any step began is the initial start and is not counted. Any later one is recorded, and clears the run's exit. Steps keep their state |
| `end` | The run ends. Its exit comes from `[end] exit_field` and its meaning from `[exits]` |

`check` is not an `[on]` action. Checks come from `[[checks]]`.

### `[end]`

| Key | Required | Meaning and constraints |
|---|---|---|
| `exit_field` | when any event maps to `end` | The field that holds the exit code. A value that is missing, not an integer, or outside 0–255 is counted as a bad exit, and the previous exit stays |

### `[[checks]]`

| Key | Required | Meaning and constraints |
|---|---|---|
| `id` | yes | Unique among the checks |
| `event` | yes | The event that reports the check. It must not also appear in `[on]` |
| `field` | yes | The check passes when this field is the integer 0. A missing or non-integer value fails it |
| `label` | no | The check's text in the flow |
| `fail_exit` | no | An integer 0–255: the exit the run takes when the check fails, for display. A failed check only colours the check; the run's exit comes from its end event |

### `[exits]`

Each key is an exit code written as canonical decimal text from `"0"` to `"255"` (`"03"` and `"+3"` are rejected). Each value is the meaning shown beside that exit.

### `[metrics]`

| Key | Required | Meaning and constraints |
|---|---|---|
| `path` | no | A second JSONL file, relative to the run directory. At most one `*` |
| `sum` | no | The fields the metrics view totals across lines |
| `error_when` | no | A table with exactly one key, such as `{ status = "error" }`. A line where that field has that value counts as an error |

### `[live]`

| Key | Required | Meaning and constraints |
|---|---|---|
| `stale_after` | no | A positive Go duration such as `"10m"`. A run with no end event and no event within this time is `stale`. Unset, a run is never stale |

### What the sources read

- Every path below the root is opened one component at a time, without following a symlink, and only real directories and regular files are accepted. A symlinked run directory, intermediate directory, or log is refused.
- Nothing outside the paths the spec names is opened.
- Each file is read from the start, up to 32 MiB. Past that the run is marked `partial`.
- A line over 64 KiB is dropped and counted. A run keeps at most 50 000 events; the rest are counted. A pattern lists at most 500 matches, newest first.
- A file whose identity changes, or that shrinks, is read again from the start.
- JSON values: strings, integers that fit in 64 bits, and booleans are kept; a boolean is kept as the text `true` or `false`. Floats, nested values and nulls are dropped from the event and counted. When a key repeats, the first one wins.
- Every string from a file is escaped and capped at 256 characters before it reaches the screen.

## Agent verbs

The dashboard needs a terminal. These two verbs give the same information without one ([ADR-0008](../adr/0008-agent-contract.md), the agent contract). Both read the desk directory and every `--spec` passed, and neither opens a screen. `--spec` can be given to `desk` or to either verb.

**They change the queue.** Listing the desk runs its scan, and the scan does two things: it fixes the hash of an item that was dropped into `pending/` by hand and not hashed yet, and it moves an item whose bytes changed since its hash was fixed to `skipped/` with reason `changed`. The dashboard's list reload does the same.

One scan reads at most 200 pending items and 64 MiB of their content, in name order. Items past either cap are not read, so their hashes are not fixed yet; the queue view counts them (`N more pending not shown`), and a later scan reads them once earlier items leave `pending/`.

### `desk runs [--spec FILE]... [--json]`

Lists every run from every source. Without `--json` it prints one run row per line, in the dashboard's order. With `--json` it prints one JSON array:

```json
[
  {
    "source": "pipeline",
    "name": "alpha-7",
    "kind": "log",
    "live": "live",
    "updated": "2026-10-05T14:02:11Z",
    "exit": null,
    "partial": false
  }
]
```

- **`source`**: the source name: a spec's `name`, or `desk`.
- **`kind`**: `log`, `summary`, or `desk`.
- **`live`**: `live`, `ended`, or `stale` for a spec run; `waiting`, `running`, `ended`, `lost`, or `skipped` for a desk run. `waiting` is an item claimed whose run has not begun yet. A desk item still waiting for approval is in the queue, not a run, so it is not listed.
- **`updated`**: the time of the run's last change, in RFC 3339, or `null` when it is not known.
- **`exit`**: the exit code, or `null` before the run ends.
- **`partial`**: `true` when a file hit the 32 MiB cap.

### `desk show <run> [--spec FILE]... [--from SOURCE] [--at N] [--json]`

Prints one run. Without `--json` it prints the compact flow as plain text. With `--json` it prints the run's folded state and its counts:

```json
{
  "source": "pipeline",
  "name": "alpha-7",
  "live": "ended",
  "exit": 3,
  "exit_meaning": "audit rejected",
  "steps": [
    {"id": "fetch", "status": "closed", "attempt": 1, "rejects": 0, "resumed": false,
     "start": "2026-10-05T14:00:02Z", "end": "2026-10-05T14:00:40Z"},
    {"id": "build", "status": "closed", "attempt": 2, "rejects": 1, "resumed": true,
     "start": "2026-10-05T14:01:05Z", "end": "2026-10-05T14:02:09Z"}
  ],
  "edges": [["fetch", "build"]],
  "checks": [{"id": "audit", "label": "audit ok?", "seen": true, "passed": false}],
  "restarts": ["2026-10-05T14:00:55Z"],
  "last_event": "2026-10-05T14:02:11Z",
  "counts": {"events": 14, "dropped": 0, "unknown_steps": 0, "bad_exits": 0},
  "partial": false
}
```

- **`--from SOURCE`** picks the source when two sources hold a run with that name. Without it, a shared name exits 2 with `run "x" is in sources a and b; pass --from`.
- **`--at N`** replays: the state after the first `N` events, the same state the dashboard's replay shows at event `N`. For a desk run that means the state the events give, not the desk's word for the run now: a finished run replayed at `0` is not `ended`. An `N` past the last event shows the whole run.
- Every time (`start`, `end`, `restarts`, `last_event`) is RFC 3339 in UTC, or `null` when it is not known: a step that has not started, or a desk event, which carries no time of its own.

Both `--json` verbs escape control characters in every string. A failure before any output follows the [`--json` stderr contract](../json-contract.md).

A source that cannot be read (a spec that fails to load, or one whose root variable is unset) does not stop the others. Each verb prints what did load on stdout, names each failed source on stderr with its path and the TOML key, and exits 1.

## Exit codes

| Code | When |
|---:|---|
| 0 | The dashboard was quit, or the verb printed its result and every source was read |
| 1 | `desk` was run without a terminal on stdin or stdout. It prints `desk needs a terminal on stdin and stdout; use "desk runs --json" or "desk show <run> --json"` |
| 1 | `desk runs` or `desk show` could not read a source; what did load is still printed |
| 1 | `desk show` was given a run that no source holds: `no run named "x"` |
| 2 | `desk show` was given a run name that two sources hold, without `--from`: `run "x" is in sources a and b; pass --from` |
| 2 | `desk show --from` named no source, or `--at` was negative |

A spec that fails to load does not stop the other sources: the dashboard shows the `1 source failed` chip, and the other sources' runs still load. Running an approved item in the terminal does not change the dashboard's exit code.
