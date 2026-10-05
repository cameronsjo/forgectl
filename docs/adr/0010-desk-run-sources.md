# 0010 — Desk run sources: one run model, an experimental spec, and the approval queue's trust model

- **Status:** Accepted (2026-10-05)
- **Context:** Related: 0005 (module architecture; `desk` registers as an extension-tier module), 0008 (agent contract; `desk` refuses off a TTY and has `--json` verbs). Command reference: [desk](../commands/desk.md).

## Context

The desk is a queue of scripts and batches that Claude stages but does not run itself. The operator reviews each item and runs it or skips it. desk-core already holds the queue protocol (`pending/`, `running/`, `done/`, `skipped/`), the claim, the detached supervisor (`desk _supervise`), and the event file each run writes. It had no screen: the `desk` command only printed that the dashboard was not available.

The operator also watches multi-step runs from other tools, which write JSONL event logs. The tool this dashboard replaces tailed one run's log, was hard-wired to one workflow, redrew the whole screen on each poll, and took no input. The operator wants to:

- see a run move through its steps, with its gate and checks;
- read the parsed events;
- switch between about ten live runs;
- open details with a click or `enter`;
- replay a finished run;
- use one pane when the terminal is small;
- approve queued desk items from the same screen.

Three questions follow. How does one viewer show desk items and other tools' logs without forgetting which is which? How does forgectl learn another tool's log format without naming that tool? And what does an approval actually guarantee when Claude runs as the same user as the desk?

## Decision

### One run model for every source

A **source** lists runs and loads each run's changes from a cursor. There are three:

| Source | Runs | Built by |
|---|---|---|
| desk | Queued, running, finished and skipped items. A script is a one-step run; a batch's steps and edges come from its manifest | `NewDeskSource`, always present |
| JSONL log | One run per directory that a run spec's `runs` pattern matches | `NewSpecSource`, one per `--spec` |
| summary | A summary JSON file that a spec's `summaries` pattern matches and that has no run directory. It reads `ended`: a summary is written once its run is over | the same spec source |

Every run, whatever its source, is folded into one `RunState`: steps with edges, a gate (a reject loop), checks, an exit code with its meaning, restarts, a live state, and counts of what was dropped. A pure reducer does the folding (`Fold(spec, defs, events, now)`). Replay is the same reducer over a prefix of the events (`Folder.At(i)`, served from a checkpoint every 1 000 events), so replay has no second code path. A desk run folds with a spec built in code (`DeskSpec()`), which maps the desk's own event keys (`STEP-START`, `STEP-END`, `STEP-FAIL`, `RUN-END`) to the same actions.

### The run spec is experimental

forgectl names no specific tool. What a tool's log means lives in a TOML **run spec** that the operator owns: where the runs are (`root_env`, `runs`, `summaries`), which JSON keys carry the event name, time, step and attempt (`[log]`), the steps and their order (`[[steps]]`), which event names are which actions (`[on]`), the gate, the checks, the exit meanings, the metrics, and when a run counts as stale. The [command reference](../commands/desk.md#run-spec-reference-experimental) lists every key.

The format is **experimental**, with no stability promise and no deprecation period. It has one consumer and one example so far, and the reducer's rules (when a restart counts, what `close` means for a step that is not running) were settled while building this. Freezing the format now would freeze those choices too.

The spec is strict in the ways that are cheap to be strict:

- An unknown key is an error, so a misspelling is reported rather than ignored.
- Every error names the file and the TOML key.
- `after` may name only an earlier step, so a spec's steps cannot form a cycle.

### Trust boundaries for the files a source reads

The spec is trusted operator config. The run root and everything under it are not: another process writes the logs, and a log can hold any bytes.

- Every path below the root is opened one component at a time with `O_NOFOLLOW` from the previous descriptor, so a symlinked run directory, intermediate directory, or log is refused, and no gap exists between a check and an open. Only real directories and regular files are accepted.
- `LoadSpec` rejects absolute patterns, `..`, `:`, `?`, `[`, and more than one `*`. Run and summary names must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`. Nothing outside the spec's named paths is opened.
- Reads are capped: 32 MiB per file (past that the run is `partial`), 64 KiB per line, 50 000 events per run, and 500 matches per pattern. A changed file identity or a shrink resets the cursor.
- JSON keeps strings, integers that fit in 64 bits, and booleans (as the text `true` or `false`); anything else is dropped from the event and counted.
- Every string from a file is escaped at ingest (`termsafe.SafeLineMax`, 256 characters) before any view sees it. The confirm modal's body is the one exception: it escapes each line with no length cap, so the operator sees every byte that will run.

### Threat model: accidents, not a same-uid agent

The desk defends against accidents: a script edited after it was queued, an item run twice by two desks, hostile text in a header. It does not defend against a same-uid process that can already write the desk directory or press keys in the desk's terminal. Claude runs as the operator's user, so an agent that wants to run something without approval has other ways to do it. The desk's job is to make the approved path the easy one and to make unapproved runs not look approved by accident.

That makes the agent's **deny rules** the control that keeps Claude off the run path. The gate is meaningful only when the agent harness denies all of these:

- **Running the supervisor in any form:** `forgectl desk _supervise …`, `forgectl` followed by flags and then `desk _supervise`, the binary by its absolute path, the binary run through `env` (`env forgectl …`), and a copy of the binary under another name. A rule that matches only the command prefix `forgectl desk _supervise` misses every form after the first.
- **Writing under the desk directory.** Rewriting `running/`, `done/`, or `skipped/`, or an item's meta, is how a run is made to look approved.
- **Sending keys to the desk's terminal pane,** through the terminal multiplexer or any other route.

**`done/` records what a supervisor ran, not proof of approval.** Anything that can write the desk can move an item into `running/` and run `_supervise` by hand, and the run it leaves in `done/` (events, log, meta) looks the same as an approved one. The claim stamp below records the approval event, and a same-uid process can forge that too. `_supervise` is hidden from help, and hiding it is presentation, not a control.

### Approval binding

What the operator approves is the exact bytes shown, as the kind shown:

- **The modal works on a frozen copy.** The confirm modal freezes a copy of the queue item when it opens. Claiming passes that copy's full 64-hex sha256 to `Claim`, which refuses an empty, short, or non-hex value, and refuses a kind other than the one recorded at queue time.
- **The claim stamps the approval.** `Claim` writes `claim_pid`, `claim_start`, `claimed_at`, and `approved_sha256` into the item's meta in `running/`.
- **The supervisor is bound to the sha and the kind.** It takes required `--sha` and `--kind` flags and runs the item only when its bytes hash to both the `--sha` value and the hash fixed at queue time, and its kind is still the approved one. Otherwise the item moves to `skipped/` with a reason (`changed`, or `supervisor: <cause>`), so a failed start leaves a record.
- **Bytes run from a pipe, never from a path.** bash reads the verified bytes from a pipe on fd 3 (`AttachScript`, run as `/bin/bash /dev/fd/3`). The copy in `running/` is a record and is never executed.
- **Only `y` confirms.** The key must not be a repeat, must arrive at least 250 ms after the key event before it, at least 400 ms after the modal opened, and at least 400 ms after the body's last line first came on screen (it must be scrolled to the end, or fit). `enter`, clicks, and keys typed before the modal opened never confirm. Terminals that report key event types mark a held key as a repeat; the 250 ms gap catches a held or typed-ahead `y` on every terminal, since a repeat is not always marked.
- **The modal reads the item itself.** The queue lists items without their bodies. `a` reads the one item again (`QueueBody`, `desk.PendingItem`) and opens the modal only when its bytes still hash to the listed sha, and the body is laid out once per hash and width. A scan reads at most 200 pending items and 64 MiB, so a large queue costs about its bytes, not a copy per line.
- **Lost claims can be cleared.** A claim whose claimer died before the supervisor started reads as `lost` and can be skipped. A claim that fails after its stamp gives itself back (`skipped/`, reason `claim-failed`), and a start that fails after a successful claim calls `Abandon`, which moves the item to `skipped/` with reason `launch-failed`.
- **A stray supervisor cannot take a foreground claim.** Until a run records its owner, only the claimer, its child (the real supervisor), or anyone once the claimer is dead may move a claim out of `running/`; a `_supervise` given the stamped sha by another process refuses in place.

The hash covers the item's own bytes, not the scripts a batch step calls or anything a script sources or downloads. The confirm modal says so for a batch.

### What an approved item runs with

Approved items run as the operator, with the operator's environment, from `$HOME`:

- **The working directory is `$HOME`** for the supervisor, the script, every batch step, and the foreground runner, so an item never inherits whatever directory the desk was started in.
- **The environment is the operator's minus bash startup hooks** (`ChildEnv`): `BASH_ENV`, `ENV`, `SHELLOPTS`, `BASHOPTS`, `CDPATH`, `GLOBIGNORE`, `PS4`, and every `BASH_FUNC_*` key are removed. Each of these makes bash run code other than the approved bytes before or instead of them, or changes what the script's own lines do. Everything else, including credentials in the environment, passes through: the item is the operator's own command.
- **Foreground (`# TTY: yes`) items are claimed by the CLI, not the dashboard.** The dashboard closes, and the CLI claims the item, records itself as the run's owner, and runs it with the terminal in the foreground, forwarding `INT`, `TERM`, `HUP` and `QUIT` to the item's process group. `SIGTSTP`, `SIGTTIN` and `SIGTTOU` are ignored in that group, so Ctrl-Z cannot stop it while the desk waits. When the item exits, the CLI ends anything left in its group, records the exit (an item killed by `INT`, `TERM`, `HUP` or `QUIT` is `interrupted`), and reopens the dashboard.

### ADR-0008 conformance

`desk` refuses to start when stdin or stdout is not a terminal, with exit 1, and names the verbs to use instead. `desk runs [--json]` and `desk show <run> [--json]` give the same information without a TTY. Listing the desk runs its scan, and the scan changes the queue: it fixes the hash of a hand-dropped item and moves a changed item to `skipped/`. The command reference says so.

### Polling, not fsnotify

Sources are polled: the selected run every second and the run list every 5 seconds. Other runs reload only when listed as changed, at most 12 per list reload. The cockpit and desk-core already poll, so the dashboard matches them. fsnotify is already in the module graph but is not used here, and no module is added. Every source reads through the same capped, cursor-based read on each poll.

## Alternatives considered

- **A Node and ink viewer in the consumer's repository.** Declined: it would be a second TUI to maintain, and the operator wants one.
- **A zero-dependency raw-ANSI Node TUI.** Declined: it would rebuild the layout, focus, and modals that bubbletea and bubbles provide.
- **A section in the cockpit.** Declined: the cockpit is a flat row list with no graph and no mouse, and its sources are fixed in code.
- **`forgectl docs serve` with Mermaid.** Declined: it is not a terminal, and it redraws the whole page on each update.
- **A liveness probe that runs a command named in the spec.** Declined: it adds a way for a spec to run commands. Liveness comes from the end event and the age of the last event instead.
- **The viewer first, the queue verbs later.** Declined: the operator chose one change.
- **Refusing TTY items in this change.** Declined: the operator chose to keep them, with the claim made in the CLI.
- **Waiting for desk-core to merge first.** Declined: the dashboard builds on the planned desk-core and is integrated where desk-core is merged.
- **fsnotify instead of polling.** Declined: the cockpit and desk-core poll, and polling keeps one read path for every source (see [Polling, not fsnotify](#polling-not-fsnotify)).
- **Sending the item's bytes to the supervisor over an inherited pipe** instead of binding the supervisor to the sha. Not chosen for now: the `--sha` and `--kind` binding closes the rewrite-after-claim gap with a smaller change to desk-core. A pipe would let the supervisor skip re-reading `running/` entirely.

## Consequences

- `forgectl desk` is a registered extension-tier module. `desk _supervise` moves under it, hidden, with the same argv plus the required `--sha` and `--kind`.
- desk-core's public surface changes: `Launch` takes the `*Claimed` value instead of a name, `Claim` refuses an empty or short sha and a kind mismatch, and meta gains the claim stamp.
- The run spec can change in any release. A consumer that writes a spec accepts that it may need to edit it after an upgrade.
- The desk's safety depends on deny rules outside forgectl. An operator who runs an agent without them gets a desk that records runs but cannot tell approved runs from others.
- Desk events carry no time of their own, so desk step times come from the batch status file, else from the step's `dur=` field, else from the item's start and end times.
- The batch runner still opens files under `done/<name>.d` by absolute path rather than through the pinned root. Only same-uid code can reach those paths, so this is left for desk-core.
