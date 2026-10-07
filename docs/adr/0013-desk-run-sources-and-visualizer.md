# 0013. Desk run sources and the run visualizer

**Status: Proposed**

Date: 2026-10-06

## Context

`forgectl desk` ([docs/commands/desk.md](../commands/desk.md)) is an operator queue (ADR-0012). Its dashboard shows waiting items, a focus panel and a history of finished runs. What it does not show is a run's shape: which steps of a batch have finished, which are running, which waited on a step that failed, and in what order things happened. `desk status NAME` prints each step's row from `status.tsv`, and `desk watch NAME` streams the raw event lines, but neither draws the graph, and neither can show the state at an earlier point of a run.

The operator also wants the same view for runs the desk did not start: a tool that writes a JSONL event log, one JSON object per line.

A parallel design built a full run viewer: a run model, a reducer, sources for the desk and for other tools' logs, and an experimental TOML "run spec" that described where another tool keeps its runs and what its events mean. forgectl#1070 decision 4 deferred all of it ("not now"), because it had no consumer and widened the desk past ADR-0012. The operator has since asked for the visualizer, which reverses that decision. #1070 also set two conditions for a return: an ADR of its own (this one), and a `desk show` that does not duplicate `desk status NAME`.

## Decision

### One run model, folded from events

`internal/runview` holds the model. A run has steps (`StepDef`: an id, a note, the steps it waits on), edges, an exit, and a live state. A pure reducer, `Fold(spec, defs, events)`, turns a run's events into its `RunState`. A `Spec` built in code says which event names start, close, fail or skip a step and which end the run.

Replay is the same reducer over a prefix of the events: `Folder.At(i)` equals `Fold` over the first `i` events, served from a checkpoint every 1 000 events. There is no second code path for replay, so replay cannot disagree with the live view.

### Two sources

A source lists runs and loads each one's new events from a cursor.

| Source | Runs | Steps |
|---|---|---|
| desk | Every running, done and skipped item. A pending item is not a run yet; the queue shows it | A script is one step, `script`. A batch's steps and edges come from its manifest |
| log | The one JSONL file named with `--log` | None: a log has no step model, so it shows its events and no step state |

The desk source folds with `DeskSpec()`: `STEP-START` starts a step, `STEP-END` closes it (or fails it when `rc` is not 0), `STEP-SKIP` skips a step that never started, `RUN-END` carries the exit, and `RUN-LOST` (added by the source when the owner is gone with no `RUN-END`) ends the run as lost, its running step interrupted. Desk events carry no time of their own, so step durations come from the batch's `status.tsv`, else the `STEP-END dur=` field, else the item's start and end times. While following, the run's live state is the desk's own word (`running`, `lost`, `skipped`, `changed`, `ended`); a replay uses the fold's.

A log line's event name, step and time come from three keys, `event`, `step` and `time` by default, which `--event-key`, `--step-key` and `--time-key` rename.

### No run spec file

_Superseded by [ADR-0014](0014-log-lenses.md): a lens file now teaches the run view an app's log, with the operator as the consumer this section waited for._

The parallel design's TOML run spec is left out. Its fields (a run root from an environment variable, glob patterns for run directories and summaries, a gate with a reject limit, checks, metrics, exit meanings, staleness) described one other tool's run layout, and that tool was its only consumer. Nothing forgectl's users run writes runs in that shape. Without the spec, a log is read as a timeline with no step state, which is true to what forgectl knows about it.

If a tool in use starts writing multi-step JSONL logs, a step model for it comes back as a change to this ADR: either a spec file or a fixed event vocabulary, decided then with a real consumer to check it against.

### `desk show` is the run; `desk status NAME` is the record

| Verb | Shows |
|---|---|
| `desk status NAME` | The item's record: WHAT, WHY, the full sha256, added, started and ended times, exit, skip fields, log and events paths, and each step's row as the runner wrote it |
| `desk show NAME` | The run as a flow: each step's state, duration and the steps it waits on, the exit, the event count. `--events` adds the event timeline; `--at N` replays to event N; `--log FILE` reads a JSONL log instead |
| `desk runs` | Every run and how far it got: steps done of total, failed steps, last activity |

`desk show` prints none of the record's fields; its last line names `desk status NAME` for them. `desk runs` is the progress view of runs, where `desk status` is the queue view of items (waiting items, hashes, WHAT, skip reasons).

### Files a source reads are untrusted

The desk source reads only through the desk's own pinned, `O_NOFOLLOW`, non-blocking readers (ADR-0012). The log source opens the named file with `O_NOFOLLOW` and `O_NONBLOCK` and reads it only when `fstat` says it is a regular file; the directory holding it is the operator's own choice and is followed as given. Reads are capped: 32 MiB per file (past that the run is `partial`), 64 KiB per line, 50 000 events per run. A file whose device or inode changes, or that shrinks, is read again from the start.

JSON keeps strings, booleans and integers that fit in 64 bits; any other value is dropped and counted. Every string from a file goes through `termsafe.SafeLineMax` (256 characters) when it is read, before any view or JSON output sees it.

### Polling

The dashboard polls the selected run, as the rest of the desk does. Nothing here adds fsnotify or a background process.

## Alternatives considered

- **Port the TOML run spec as written.** Declined: it serves one other tool's layout, and an unconsumed config format is a stability promise with nothing to check it against.
- **Infer step state from a log with a fixed event vocabulary** (`start`, `end`, `fail`). Declined for now: no tool in use writes such a log, and a guessed vocabulary would be wrong for the first one that does.
- **Fold `desk show` into `desk status NAME`.** Declined: the record and the run's shape answer different questions, and `status NAME --json` is an existing contract (ADR-0008) that a graph and a timeline would bloat.
- **Replace the dashboard with the parallel design's.** Declined by #1070 decision 1: the visualizer goes into the existing dashboard as a view.

## Consequences

- `desk runs [--json]` and `desk show <name> [--json]` are new verbs with additive-only JSON (ADR-0008).
- The dashboard gains a run view: the flow, the event timeline and replay for one run. It lands in its own change after the verbs.
- `internal/desk` gains `ParseEvent`, a portable parser for the event lines the desk writes, where `msg=` and `log=` take the rest of the line.
- A JSONL log shows no step state. That is a known limit, not a bug: closing it needs a tool in use that writes such logs, and a change to this ADR.
- Reading the desk scans it, and a scan changes the queue as `desk status` does (ADR-0012).
