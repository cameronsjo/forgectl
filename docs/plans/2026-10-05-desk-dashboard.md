---
status: in-flight
next: "Task 7b (review and dry-run fixes) and Task 8 CLI in parallel"
branch: feat/desk-dashboard
pr: "—"
updated: 2026-10-05
approved_session_id: ce2f6f6c-f5ea-4155-92aa-4d8ed8923668
date: 2026-10-05
session_id: a5384bb4-343c-47a5-be33-01e47b7e4653
model: claude-opus-5-5
harness: claude-code 2.1.289
machine: 7dd9aa52f93e
---

# Desk dashboard: run viewer and approval queue for `forgectl desk`

> This is the public copy. A private appendix holds the consumer's own run spec and its live-check numbers, because the run names there carry identifiers.

## Goal

Ship `forgectl desk` as a full-screen bubbletea dashboard with two jobs:

1. **The approval queue.** Approve, skip, and run queued desk items, including foreground `# TTY: yes` items.
2. **The run viewer.** Watch any multi-step run walk its workflow. Views: a top-down flow with gates and checks, parsed events, a timeline, metrics, and a summary.

Runs come from three kinds of **sources**:

- the desk's own items and batches;
- any JSONL event log described by an **experimental** TOML run spec;
- summary-only JSON results.

Operator controls:

- switch among about ten runs live;
- open detail modals by click or `enter`;
- step or autoplay a replay of any run;
- small terminals fall back to one pane.

## Context

**What exists today.** The motivating tool is a Node script in a private repo. It tails one run's JSONL log and draws a flowchart beside the log. It has three limits:

- it is hard-wired to one workflow;
- it polls and redraws the whole screen;
- it shows one run and takes no input.

**What the operator wants:**

- see the run walk its workflow;
- read parsed logs;
- swap between runs;
- open modals on click;
- show one pane when the terminal is small;
- tie it to the desk.

**What forgectl already has** (unmerged, on desk-core and desk-tui):

- `internal/desk`, on `feat/desk-core` (open PR #1059, still moving): queue, `Claim`/`Launch`, `BeginRun`/`Run.Finish`, `.events`, manifest DAG, `Watcher.Poll`, and `Claimed.AttachScript`, which feeds verified bytes on fd 3.
- The `Panel`/`Sparkline`/`Bar*` widgets, on `feat/desk-tui`.
- A hidden placeholder `desk` command whose output reads "the desk dashboard is not available in this build".

So this plan builds the dashboard there. Workflow knowledge lives in a spec file the consumer owns.

**Delivery.** The patch series is merged on the machine that owns PR #1059. Any later desk-core moves are integrated there.

## Loop

| Thing created | Opened by (surfaces) | Closed by (surfaces) | Who closes |
|---|---|---|---|
| Claimed background item in `running/` | Task 7: approve → `Claim(name, fullSHA)` → `Launch` (dashboard) | existing: `desk _supervise` finishes it into `done/`. Task 4: a `Launch` error after `Claim` calls `Abandon(name, "launch-failed")` into `skipped/` | the supervisor; the dashboard on launch failure |
| Claimed item whose owner died (PID 0 or dead pid) | Task 7 claim paths | Task 4: PID 0 and meta older than 30 s, or a dead pid, now count as `lost`. Task 7: `s` skips a `lost` item | the operator |
| Foreground TTY run (`BeginRun(name, os.Getpid())`) | Task 7: the CLI claims after the TUI releases the terminal | Task 7: `Run.Finish(rc, reason)` after `cmd.Wait`, on a normal exit, an error, or a forwarded signal. If the parent is SIGKILLed, its recorded pid dies and the row above closes it | the CLI process, else the operator via `lost` |
| Operator skip (`skipped/`, reason `operator`) | Task 7: `s` | Task 7: `u` → `Unskip`; existing: `Prune(days)` | the operator |
| Changed-since-queued skip (reason `changed`) | existing: `Scan` moves the item | existing: `Prune(days)`. By desk-core design it cannot be re-armed, so the queue shows "changed — re-queue to run it" | the operator |
| Launch-failed skip (reason `launch-failed`) | Task 4 / Task 7 | existing: `Prune(days)`; the queue row shows the reason | the operator |

## Alternatives declined

- **Node + ink viewer in the consumer repo.** The operator chose the desk dashboard, so there is one TUI long-term.
- **Zero-dependency raw-ANSI Node TUI.** It would reinvent layout, focus, and modals that bubbletea and bubbles provide.
- **Cockpit section.** The cockpit is a flat row list with no graph and no mouse, and its sources are hardcoded.
- **`forgectl docs serve` with Mermaid.** It is not a terminal, and it redraws the whole page on every update.
- **Liveness probe that runs a command named in the spec.** That is a needless execution surface. Liveness instead comes from the end event plus the age of the last event.
- **Viewer first, queue verbs later.** The operator chose one PR.
- **Refusing TTY items in this PR.** The operator chose to keep them, with the claim done in the CLI.
- **Waiting for desk-core to merge first.** The operator chose to build on the planned desk-core and integrate on the machine that owns it.

## Panel

Panel: plan-reviewer ×2, red-team, security-posture, user-experience, owner-review ran — 114 findings, 107 folded in, 7 declined (see Panel review — findings declined)

## Panel review — findings declined

- **Owner #6, defer the foreground TTY runner.** The operator decided to keep it. The security fixes (claim in the CLI, `AttachScript`, signal forwarding) cover the risk the owner seat raised.
- **Owner #4, cut timed playback.** A fixed-rate autoplay (`space`, 4 events/s, with no speed control) is a few lines over `At(i)`, and the operator asked to watch a finished run walk. The speed keys from the UX seat are cut instead.
- **Owner #9, put redacted live-check numbers in the public cover letter.** The run names carry identifiers, per the red-team nit. The numbers stay in the private appendix, and the cover letter says only "verified against N real runs; counts matched".
- **UX C1, allow `u` on a `changed` skip.** desk-core says a changed item "cannot be re-armed" by design, because the hash is fixed once. Instead the Claim-time mismatch re-opens the modal on the new bytes (Task 7), and the `changed` row tells the operator to re-queue.
- **Underspec C3 and C5, "wantCount is 31" and "SafeLineMax does not exist".** These came from a stale local checkout. Live probes found `wantCount = 34` and `termsafe.go:134 SafeLineMax` on the base refs.
- **Conflicts advisory, add an `[Unreleased]` changelog entry.** forgectl's `AGENTS.md` and `changelog_ownership_test.go` forbid hand edits, and Release Please writes the changelog from the PR body's `BEGIN_COMMIT_OVERRIDE`. Task 9 writes that block.
- **UX advisory, open on the most useful run by default.** It is folded only as "open on the queue when items are waiting, else on the newest live run". A separate default-selection setting is cut.

## Architecture

```text
               ┌──────────────── internal/runview (pure, all platforms) ──────────────┐
  spec.toml ─▶ │ Spec ─▶ Fold(spec, defs, events, now) ─▶ RunState                    │
               │        Replay: Folder with checkpoints every 1k events → At(i)       │
               └───────────────▲───────────────────────────────▲─────────────────────┘
                               │ Delta{Events,Metrics,Summary,…}│
        ┌──────────────────────┴───────┐        ┌───────────────┴────────────────────┐
        │ source: jsonl (spec, unix)   │        │ source: desk (unix)                │
        │ os.OpenRoot(root); no symlink│        │ Scan, Manifest(name), BatchStatus, │
        │ in any component; caps;      │        │ Watcher.Poll → ParseEvent          │
        │ sanitized at ingest          │        │ (parser lives in internal/desk)    │
        └──────────────────────────────┘        └────────────────────────────────────┘
               ┌──────────── internal/tui/desk*.go (unix) ───────────────────────────┐
               │ header · run strip/list · views 1–6 · modals · replay · help/keymap │
               └──────────────────────────────┬──────────────────────────────────────┘
               internal/cli/desk.go (unix): loop { act := RunDesk(); if DeskRun → claim+run; else break }
```

- **One model for every source.** A run has:
  - steps with edges, so a desk batch draws as a DAG;
  - a gate (a reject loop);
  - checks (such as a final verification);
  - an exit code with its meaning;
  - events, metric series, and a summary.
- **Pure reducer.** Replay is `At(i)`, which is `Fold(events[:i])`, served from checkpoints. There is no second code path.
- **forgectl names no specific harness.** The spec is marked experimental in ADR-0010, with no stability promise.

### Run spec (experimental, neutral example)

```toml
name = "pipeline"
root_env = "PIPELINE_ROOT"         # absolute; required
runs = "runs/*"                    # one '*', no '..', relative; each match = one run dir (real dir, no symlink)
summaries = "results/*.summary.json"  # one '*'; the run name is the text '*' matched

[log]
path = "out/events.jsonl"          # relative to the run dir; every component checked, no symlinks
time = "at"
event = "kind"
step = "stage"
attempt = "try"                    # optional; unset means each start adds 1
show = ["stage", "try", "rc", "err"]

[[steps]]
id = "fetch"
note = "pull the inputs"
[[steps]]
id = "build"
note = "compile and package"

[gate]                             # applies to every step
label = "accepted?"
max_rejects = 3

[on]                               # event -> action
stage_started = "start"
stage_done = "close"
stage_refused = "reject"
stage_gave_up = "fail"
worker_boot = "restart"            # the first one, before any step began, is the initial start
run_finished = "end"

[end]
exit_field = "rc"                  # integer 0–255

[[checks]]
id = "audit"
event = "audit_result"
field = "rc"
label = "audit ok?"
fail_exit = 3

[exits]                            # TOML keys are strings; parsed as integers 0–255
"0" = "finished, audit passed"
"3" = "audit rejected"

[metrics]
path = "out/usage.jsonl"
sum = ["in", "out"]
error_when = { status = "error" }  # exactly one key

[live]
stale_after = "10m"
```

### Reducer transitions (Task 3 implements and tests one row each)

| Action | Effect |
|---|---|
| `start` | The step becomes `running`; `attempt` is taken from the event, else +1; `start` is set; `end` is cleared; marked `resumed` if a restart happened since the previous `start` of this step. A step id not in the defs is counted in `UnknownSteps`, and no step is added |
| `close` | `running` → `closed`; `end` is set |
| `reject` | `rejects += 1`; the state is unchanged (the loop is drawn). Reaching `max_rejects` does **not** fail the step by itself; only a `fail` event does |
| `fail` | The step becomes `failed`; `end` is set |
| `restart` | If no step has begun yet, this is the initial start and is not counted. Otherwise it is appended to `Restarts`, and `Exit` and `ExitMeaning` are cleared. Steps keep their state |
| `end` | `Exit` is set from `exit_field` (an integer 0–255, else ignored and counted); `ExitMeaning` comes from `exits` |
| `check` | `CheckState{passed: field == 0}`. Failure only colors the check. The exit comes from the `end` event |
| *(unmapped event)* | Kept in the event list; state is unchanged |
| Live | `ended` if an `end` event is present (and no later restart); else `live` if the last event is within `stale_after`; else `stale`. Desk items use `WatchState`: `waiting`, `running`, `ended`, `lost`, `skipped` |

### Layout: columns choose the layout, rows choose the flow form

| Columns | Layout |
|---|---|
| ≥ 150 | run list (28) │ flow (62) │ side view |
| 100–149 | run strip `‹ 3/10 beta-… ◐ live 12s ›` in the header; flow (62) │ side view |
| 40–99 | single pane; a tab strip `1 flow 2 events 3 timeline 4 metrics 5 summary 6 queue` shows the active view |
| < 40 or < 12 rows | "terminal too small (needs 40×12) — resize or press q" |

**Flow form by height.** The flow is drawn full when `3 + 6·steps + 7` rows fit, and compact (two rows per step) otherwise. Either form scrolls and follows the running step when it still doesn't fit. A desk batch draws one wave per row band.

**Modals** are centered and cover at most 80% × 80%, clamped to at least 36×8. They scroll with `bubbles/viewport` and re-layout on resize.

### State glyphs: one legend, an ASCII fallback, every color paired with a word

| State | Glyph (icons) | ASCII | Word |
|---|---|---|---|
| pending / waiting | `·` | `.` | `pending` / `waiting` |
| running / live | `◐` (spins) | `*` | `running` / `live` |
| closed / ended ok | `✓` | `+` | `done` / `ok` |
| failed / exit ≠ 0 | `✗` | `x` | `failed` / exit code |
| rejected | `↺` | `r` | `rej N/3` |
| resumed | `⟲` | `~` | `resumed` |
| stale | `!` | `!` | `stale` |
| lost | `?` | `?` | `lost` |
| skipped | `–` | `-` | `skipped:<reason>` |

Glyphs come from the cockpit's `pickGlyphs(noIcons)` set, extended. Event rows carry a tag column: `start`, `ok`, `FAIL`, `rej`, `rst`, `end`, `chk`, or `·` for unmapped. The `?` help shows this legend.

### Run row and ordering

```text
◐ live    alpha-…-7     12s
✓ ok      beta-…-7       4m
✗ 3       gamma-…-7      1h
! stale   nightly-b3     2h
? lost    batch-04       3h
```

- **Row layout:** glyph, then a word or exit code, then the name, then the age since the last event. Names are cut in the middle (`head…tail`), so suffixes like `attempt1-retry` stay visible. A source tag is added only when two sources share a name.
- **Ordering:** live runs first, then by last-event time, newest first.
- **Selection** follows the run's identity (source and name), never its row index.

### Keys and mouse

These are registered in `internal/keymap` and rendered by `bubbles/help`. The footer is context-sensitive.

| Input | Action |
|---|---|
| `[` `]` or `<` `>` | Previous or next run |
| `o` | Run switcher: a `bubbles/list` with filtering, full rows, `enter` picks, `esc` closes |
| `1`–`6` · `tab` | Pick a view · next pane (pane focus only, never views) |
| `↑↓`/`jk` · wheel · `g`/`G` | Move in the focused pane · scroll · top/bottom. `G` also resumes follow |
| `enter` · click | A click selects. A second click or `enter` opens a step modal (that step's events, attempts, rejects, duration; live-updating; `enter` on an event opens nested) or an event modal (all fields, wrapped) |
| `p` | Toggle replay (header `REPLAY i/N · +M new · by time` or `by event`). `←→` steps, `space` autoplays at 4 events/s, `g`/`G` jump to start/end |
| `a` · `s` · `u` | Approve · skip · unskip, in the queue view. Outside it, `a` shows "approve is in the queue (6)" |
| `/` | Filter: the run list (wide) or the events (single pane). `enter` keeps it, `esc` clears it |
| `r` · `R` | Refresh the selected run · refresh everything (15 s minimum gap; the footer explains a declined refresh) |
| `m` | Toggle mouse capture (Shift-drag selects text) |
| `?` · `esc` · `q` · `ctrl+c` | Help and legend · close one level (modal → replay → filter) · quit, or close a modal (footer: "quit leaves running jobs running") · always quit |

### States the operator can see

| State | Shown as |
|---|---|
| Loading | `loading runs…` |
| No runs | A cause-specific line, one of: `no sources: pass --spec`, `$PIPELINE_ROOT is unset`, `no runs match runs/*`. The queue view shows `queue empty` |
| Bad spec | Header chip `1 source failed`. The detail (path, field, cause) is in `?`; the other sources still load |
| Log missing or unreadable | `no log yet` for a new run, or `cannot read out/events.jsonl: <cause>` |
| Partial ingest | Header chip `partial: over 32 MiB cap` / `3 lines dropped · 2 unknown steps · 1 bad exit` |
| Run vanished | The row shows `gone`; the last state is kept and labeled |
| Log reset (shrank or replaced) | `log restarted` note; the cursor resets |
| Stale / lost | `stale: no events for 14m (limit 10m)` / `lost: supervisor died — s to skip` |
| Queue waiting | Header badge `queue: 3 waiting · lost: 1`. The dashboard opens on the queue when items wait, else on the newest live run |
| Follow paused | `paused · N new · G to follow` |

## Tech Stack

- **Toolchain:** Go 1.26.8, bubbletea v2.0.9 (`View.MouseMode = tea.MouseModeCellMotion`, `MouseClickMsg`, `MouseWheelMsg`), lipgloss v2.0.6, bubbles v2.2.1 (`list`, `viewport`, `help`, `key`), `charmbracelet/x/ansi` v0.11.8, `BurntSushi/toml` v1.6.0.
- **No new modules.** fsnotify v1.10.1 is present but not used here. Polling matches the cockpit and desk-core.
- **Tests:** `go test` with goldens via `-update`. Model tests call `Update(msg)` directly. Subprocess tests use the `TestHelperProcess` pattern. `govulncheck` runs in Task 9.

## Global Constraints

- **Pinned base.** Task 1 records the desk-core SHA `D` it built on. Every API is re-checked at `D`, never at older notes.
- **No consumer identifiers in forgectl.** Source, tests, fixtures, docs, and commits carry no org or repo names, job ids, program names, real log content, or the consumer's event, field, or exit vocabulary. The fixtures and example use the neutral vocabulary above. The redaction pass diffs the public patch against the appendix's tokens.
- **Sanitize every file-derived string at ingest.** That covers event fields, step ids, summary keys and values, desk names and headers, manifest step ids, `status.tsv`, and log lines. Use `termsafe.SafeLineMax(s, 256)`. The approve modal's body is the exception: it uses `termsafe.SafeLine` per line with no cap (Task 7). Never use `desk.Normalize` for display, because it deletes characters instead of showing them.
- **Path trust.**
  - The spec is trusted operator config.
  - Logs and run dirs are untrusted.
  - Open through `os.OpenRoot(root)`.
  - Every path component is `Lstat`-checked: no symlinks, and real dirs and regular files only.
  - `LoadSpec` rejects absolute patterns, `..`, and more than one `*`.
  - Run and summary names match `^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`.
  - Nothing outside the spec's named paths is opened.
- **Caps.**
  - Ingest reads from offset 0, up to 32 MiB per file. Past that the run is marked `partial`.
  - Lines over 64 KiB, and at most 50 000 events per run, are counted in `Dropped`.
  - At most 500 glob matches per source.
  - A file is replaced when its dev or inode changes, or when it shrinks. Replacement resets the cursor.
  - JSON: `UseNumber` is on; strings and integers that fit in int64 are kept; anything else is dropped from the field and counted. For a duplicate key, the first one wins.
- **Approval binding.**
  - `Claim` always gets the full 64-hex sha of the frozen `Item` the modal opened with. It is never empty and never a prefix.
  - Only a non-repeat `y` keypress confirms. It is accepted at least 400 ms after the modal opens and after the body has been scrolled to the end (or fits on screen). `enter`, clicks, and keys buffered earlier never confirm.
  - Items run only by `AttachScript`, never by path.
- **Threat model** (in ADR-0010):
  - The desk guards against accidents, not against a same-uid agent.
  - Approved items run with the operator's environment, and with cwd `$HOME`.
  - The gate is meaningful only with agent deny rules on `forgectl desk _supervise*`, writes under the desk dir, and send-keys to the desk pane.
- **Unix-only.** The `desk` source, `internal/tui/desk*.go`, and `internal/cli/desk*.go` carry `//go:build unix`. `_other.go` stubs return `desk.ErrUnsupported`. `internal/runview` core is portable.
- **ADR-0008.**
  - `desk` refuses off a TTY (stdin or stdout) with exit **1**: `desk needs a terminal on stdin and stdout; use "desk runs --json" or "desk show <run> --json"`.
  - The agent verbs exist and their exit codes are honest.
  - `Scan` changes the queue (it fixes hashes and moves changed items); `docs/commands/desk.md` says so.
- **Color.** Every color is paired with a glyph or word. Styles come only from `theme.Styles`; `theme_literals_test.go` enforces this.
- **File headers.** Every new Go file starts with `// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause`.
- **Changelog.** Never edit `CHANGELOG.md`. The release notes go in a `BEGIN_COMMIT_OVERRIDE` block in the cover letter.
- **Delivery is patch files.** The series is written to a local patches directory with `--base=D`, and the operator merges it on the machine that owns PR #1059.
- **Commits.** Conventional Commits, each with the producer-tuple trailer block.

## Orchestrator

**Driver:** opus — trigger: untrusted-input ingest, the approval control, and a security review whose findings feed back into later tasks.

## Tasks

> Every task runs in this branch's worktree, and dispatch prompts name that path. Each report goes to a scratch reports dir outside the worktree.
>
> Waves: Task 1 → {Task 2, Task 3} → Task 4 → Task 5 → Task 6 → Task 7 → {Task 7b, Task 8} → Task 9 → consumer live check.

### Task 1 — Worktree, pinned base, persisted plan

**Files:**

- Create: `docs/plans/2026-10-05-desk-dashboard.md` (this copy)
- Modify: `docs/plans/README.md` (generated)

**Interfaces:** Produces the branch `feat/desk-dashboard`, the SHA `D`, and the merge SHA `M`.

**Dispatch:** In-context · Opus

**Steps:**

- [x] `git fetch origin`, then `D=$(git rev-parse refs/remotes/origin/feat/desk-core)`. Record `D` in this plan's `## Learnings`.
- [x] Preview the merge in three steps, with quoted, fully qualified refs (`merge-tree --write-tree`, the `--name-only` conflict set, then `git diff --stat` only when the set is empty). A conflict set that isn't empty stops the plan and goes to the operator.
- [x] Create the worktree on `feat/desk-dashboard` at `D`, then `git merge --no-ff refs/remotes/origin/feat/desk-tui`. Record the merge SHA `M`.
- [x] Baseline: `go build ./... && go vet ./... && go test ./...`, with each exit status captured to a log. Record the test counts.
- [x] Re-resolve every cited API at `D`: `Claim`, `Claimed{RecordPath, …}`, `AttachScript`, `Launch`, `BeginRun`, `Finish`, `Skip`, `Unskip`, `Prune`, `Scan`, `NewWatcher`/`Poll`, `LoadManifest`, `Waves`; `wantCount`; `supervisorArgv`; `pickGlyphs`. Record any differences as `## Deviations`.
- [x] Persist this plan with `status: in-flight`. Run `go test -run TestPlansIndex -update .`, then the redaction scan and a manual identifier check. Commit `docs(plan): desk dashboard`.

### Task 2 — Security review of the desk approval control (before any code calls it)

**Files:** none modified.

**Interfaces:** Produces findings that Tasks 4, 6, and 7 must fold.

**Dispatch:** Parallel (wave 2, with Task 3) · fresh Opus security reviewer.

**Steps:**

- [x] Review these files at `D` and `origin/main`, as a whole and not as a diff:
  - `internal/desk/{desk,claim_unix,desk_unix,queue_unix,supervise_unix,events_unix,batch_unix,manifest,prune_unix,redact}.go`
  - `internal/desk/proc_*.go`
  - `internal/cli/desk_supervise.go`
  - `internal/termsafe/{termsafe,json}.go`
- [x] Focus on:
  - sha binding from display to exec;
  - the PID-0 stuck claim;
  - `Watcher.read` with unbounded `ReadAll`;
  - cwd and environment inheritance;
  - the `_supervise` path the agent can reach.
- [x] Map each finding to a Task 4 or Task 7 step, or a `## Deviations` line.

### Task 3 — `internal/runview`: model, spec, reducer, replay

**Files:**

- Create: `internal/runview/{model,spec,fold,replay}.go`
- Test: `internal/runview/{spec,fold,replay}_test.go`, `testdata/*.toml`, `testdata/*.jsonl` (synthetic, neutral vocabulary)

**Interfaces** (Produces):

```go
type Field struct{ Key, Value string }
type Event struct{ Seq int; Time time.Time; Name, Step string; Fields []Field } // Time zero for desk events
type StepDef struct{ ID, Note string; After []string }
type StepStatus string // pending|running|closed|failed
type StepState struct{ ID, Note string; Status StepStatus; Attempt, Rejects int; Start, End time.Time; Resumed bool }
type CheckState struct{ ID, Label string; Seen, Passed bool }
type LiveState string // live|ended|stale|waiting|running|lost|skipped
type RunState struct {
    Steps []StepState; Edges [][2]string; Checks []CheckState
    Exit *int; ExitMeaning string; Restarts []time.Time
    Live LiveState; LastEvent time.Time
    UnknownSteps, BadExits int
}
func LoadSpec(path string) (*Spec, error)            // error names path + TOML key
func Fold(spec *Spec, defs []StepDef, events []Event, now time.Time) RunState
// defs nil → spec.Steps; Edges from StepDef.After, else the linear chain.
type Folder struct{ /* checkpoints every 1000 events */ }
func NewFolder(spec *Spec, defs []StepDef) *Folder
func (f *Folder) Append(evs ...Event)
func (f *Folder) At(i int, now time.Time) RunState   // == Fold(spec, defs, events[:i], now)
func (f *Folder) Len() int
```

**Dispatch:** Parallel (wave 2) · fresh Opus subagent, because this is the parsing core.

**Steps:**

- [x] Write failing tests, one per row of the reducer table:
  - `start` on the first step, and `close`;
  - `reject` ×3 with no fail, then `fail`;
  - `restart` before any step (0 restarts), and `restart` mid-step (marks `resumed`, clears `Exit`);
  - `end` with a valid exit, a negative exit, and a non-integer exit (`BadExits`);
  - `check` pass and fail;
  - an unmapped event;
  - an unknown step;
  - a linear chain vs `After` edges;
  - each `Live` state against `now`.
- [x] Replay: for every i in a 2 500-event fixture, `At(i)` equals `Fold(events[:i])`, with checkpoints crossed.
- [x] Spec errors, each naming its TOML key:
  - an unknown action;
  - a duplicate step id;
  - `max_rejects < 1`;
  - a bad duration;
  - an absolute path, `..`, or two `*`;
  - an `exits` key that isn't 0–255;
  - an `error_when` with more than one key.
- [x] Run and expect RED. Implement. Run and expect GREEN.
- [x] Mutation sweep: delete each action arm, the initial-start rule, and the `Exit` reset in turn. A test goes red for each. Record the results in the report.
- [x] Commit `feat(runview): run model, experimental TOML run spec and event reducer`.

### Task 4 — desk-core additions for the dashboard

**Files:**

- Modify: `internal/desk/queue_unix.go`, `claim_unix.go`, `events_unix.go`, `desk_unix.go`
- Create: `internal/desk/event.go` (portable parser)
- Test: `internal/desk/dashboard_unix_test.go`, `event_test.go`

**Interfaces** (Produces):

```go
func (d *Desk) Manifest(name string) (*Manifest, error)        // running|done/<name>.manifest via d.root, capped, regular only
func (d *Desk) BatchStatus(name string) ([]StatusRow, error)   // done/<name>.d/status.tsv via d.root, capped
func (d *Desk) Abandon(name, reason string) error              // running item with PID 0 claimed by this process → skipped/
func (d *Desk) Launch(c *Claimed) (int, error)                 // was Launch(name); passes --sha to the supervisor
func ChildEnv() []string                                       // os.Environ() minus bash startup hooks
type ParsedEvent struct{ Key string; Fields map[string]string } // RUN-START, STEP-*, RUN-END, RUN-LOST
func ParseEvent(line string) (ParsedEvent, bool)
```

**Dispatch:** Serial (after Tasks 2 and 3) · fresh Opus subagent, because it modifies the control.

**Steps:**

- [x] Apply the Task 2 findings mapped here (each one checked against the code at `D`):
  - **Bind the supervisor to the approved sha (I1).** `Launch(name)` becomes `Launch(c *Claimed)`. `supervisorArgv` carries `--sha <c.SHA256>`, and `supervise` refuses unless `SHA256Hex(data)` equals both the argv sha and `meta.SHA256`. `Launch` takes the TTY check from `c.Headers`, so its by-path read goes away.
  - **`Claim` requires a full sha (I2).** It returns `ErrRefused` unless `wantSHA` matches `^[0-9a-f]{64}$`. The existing tests that pass `""` pass the queued sha instead.
  - **`Claim` checks kind (M1).** It refuses when `meta.Kind` is set and differs from the kind it found.
  - **Claim stamp (I3, I7).** `Claim` writes `claim_pid`, `claim_start`, `claimed_at`, and the approved sha into the `running/` meta. `Launch` writes the supervisor's `pid` and `pid_start` into meta right after `cmd.Start`.
  - **Supervisor start failures leave a trace (I3).** Any `supervise` error before `BeginRun` moves the item to `skipped/` with `skip_reason` `supervisor: <cause>`.
  - **One capped, non-blocking read helper (I4, M6).** `readCapped(sub, file, max)` opens with `O_NOFOLLOW|O_NONBLOCK|O_CLOEXEC` through the root, `fstat`s for a regular file, and reads through a limit. Use it in `Watcher.read`, `hasRunEnd` (last 4 KiB only), the `ReadFile` sites in `queue_unix.go`, `lastLine`, `fillTimes`, and `readMeta`. `Watcher.read` resets on a dev/ino change or a shrink, reads at most 32 MiB per poll, and drops (and counts) lines over 64 KiB.
  - **`openEvents` uses `O_EXCL` (M5).**
  - **The supervisor catches `SIGHUP` instead of ignoring it (M3),** so children start with the default disposition.
  - **A live pid behind an unreadable `/proc` counts as alive (M8).**
- [x] Lost detection (I3): a running item with PID 0 is `lost` when its claim stamp names a dead process, or, with no stamp, when the item file's mtime is older than 30 s. A non-zero dead PID stays `lost` as today.
- [x] `Abandon(name, reason)` (I3): renames into `skipped/` first, then checks that the moved meta still has PID 0 and that the claimer is this process; otherwise it moves the item back and returns `ErrClaimed`.
- [x] Child process setup (I5): `Launch`, `superviseScript`, batch steps, and the foreground path set `cmd.Dir = $HOME` and use `ChildEnv()`, which is `os.Environ()` minus `BASH_ENV`, `ENV`, `SHELLOPTS`, `BASHOPTS`, `CDPATH`, `GLOBIGNORE`, and every `BASH_FUNC_*` key. It is also passed as `BatchOptions.Env`.
- [x] `ParseEvent` (M9) treats `msg=` as running to the end of the line, and the supervisor writes `log=` relative to the desk dir.
- [x] Tests:
  - the accessors refuse a symlink, a FIFO, a non-regular file, and an oversize file, without blocking;
  - `Abandon` refuses an item with a live pid or another claimer;
  - a PID-0 item with a dead claimer reads as `lost` and can be skipped;
  - a consistent rewrite of the record and the meta after `Claim` runs nothing on the background path;
  - `Claim("")` and a 63-char sha are refused;
  - a `BASH_ENV` marker script never runs, and the item's cwd is `$HOME`;
  - `ParseEvent` round-trips every key the supervisor emits;
  - a hostile value stays inside its field.
- [x] Commit `feat(desk): manifest, status and event accessors; detect orphaned claims`.

### Task 5 — Sources: jsonl, summary, desk

**Files:**

- Create:
  - `internal/runview/source.go` (portable interface)
  - `source_jsonl_unix.go`, `source_summary_unix.go`, `source_desk_unix.go`, `tail_unix.go`
  - `source_other.go` (stub)
- Test: `source_jsonl_unix_test.go`, `source_desk_unix_test.go`, `tail_unix_test.go`

**Interfaces** (Produces):

```go
type RunKind string // log|summary|desk
type RunRef struct{ Source, Name string; Kind RunKind; Updated time.Time; HasSummary bool }
type Metric struct{ Seq int; Values map[string]int64; Error bool }
type KV struct{ Key, Value string } // flattened sorted key paths
type Cursor struct{ Offset int64; Dev, Ino uint64 }
type Delta struct{ Events []Event; Metrics []Metric; Summary []KV; Defs []StepDef; Items []QueueItem
                   Dropped int; Partial, Reset bool; Err error }
type QueueItem struct{ Name, Kind, State, What, Why, SkipReason, SHA256 string; TTY, Stale bool; Body []string; Exit *int }
type Source interface { Name() string; List() ([]RunRef, error); Load(ref RunRef, cur *Cursor) (Delta, error) }
func NewSpecSource(spec *Spec) (Source, error)      // jsonl and summaries; root from spec.RootEnv
func NewDeskSource(d *desk.Desk) Source
```

`QueueItem` is the sanitized copy the queue view and the confirm modal use. `Body` holds the lines through `SafeLine` with no cap. `SHA256` is the full 64-hex value, read from `Meta.SHA256` and asserted equal to the hash of `Content`.

**Dispatch:** Serial (after Task 4) · fresh Opus subagent, because it ingests untrusted files.

**Steps:**

- [x] `tail_unix.go`:
  - open through `os.OpenRoot`;
  - walk each component with `Lstat` and reject any symlink;
  - incremental reads from the cursor;
  - the caps from Global Constraints;
  - a partial last line is held;
  - dev/ino change or a shrink sets `Reset`.
- [x] jsonl source:
  - list run dirs, rejecting symlinked dirs and bad names (at most 500);
  - parse with `UseNumber`;
  - apply the field rules, then `SafeLineMax`;
  - sum the metrics;
  - set `Seq` by line order.
- [x] Summary source: the name is the `*` capture. A matching run dir gets `HasSummary`. A summary with no run dir is a `summary` run.
- [x] Desk source:
  - `Scan` → `Items`;
  - batch `Defs` from `Manifest` (`After` → edges);
  - events from `Watcher.Poll` → `ParseEvent`;
  - durations from `STEP-END dur=` / `BatchStatus`;
  - a script is a one-step run.
- [x] Tests:
  - `termsafetest.Hostile` fixtures leave no ESC or C0/C1 in any `Delta` string, including `Items`;
  - a symlinked intermediate dir pointing at a fixture secret JSON yields nothing;
  - a symlinked log is refused;
  - a 65 KiB line is dropped;
  - a replaced file resets;
  - a partial line is held;
  - invalid JSON, a nested value, and a float are dropped and counted;
  - a 2^60 integer is exact;
  - a `*.summary.json` stem joins its run;
  - a desk batch in `t.TempDir()` gives the right DAG and states.
- [x] Before committing, run `git ls-remote origin refs/heads/feat/desk-core`. If it differs from `D`, record it as a `## Deviations` line for the merging machine and continue on `D`.
- [x] Commit `feat(runview): jsonl, summary and desk run sources`.

### Task 6 — Dashboard TUI, part 1: layout, flow, panes

**Files:**

- Create:
  - `internal/tui/desk_unix.go` (model, Update)
  - `desk_view_unix.go` (header, breakpoints, states)
  - `desk_flow_unix.go` (full and compact flow, DAG bands, scroll-follow)
  - `desk_panes_unix.go` (events, timeline, metrics, summary, queue list)
  - `desk_other.go`
- Modify: `internal/keymap/keymap.go` (desk bindings), `internal/tui/styles.go` (extended glyph set)
- Test: `internal/tui/desk_unix_test.go`, `desk_golden_unix_test.go`, `testdata/desk_*.golden`

**Interfaces:**

- Consumes: Task 5 `Source`/`Delta`/`QueueItem`, the Task 3 `Folder`, and the desk-tui widgets.
- Produces:
  - `RunDesk(ctx, DeskOptions{Sources []runview.Source; Theme theme.Theme; NoIcons bool; Now func() time.Time; Restore *DeskViewState}) (DeskResult, error)`
  - `DeskResult{Action DeskAction; State DeskViewState}`
  - `DeskAction` is either none or `DeskRun{Name, SHA string}`, defined in `desk_unix.go`.

**Dispatch:** Serial (after Task 5) · fresh Sonnet subagent. It renders sanitized data and owns only these files.

**Steps:**

- [x] Model:
  - a 1 s tick loads the selected run's delta, and the run list reloads every 5 s;
  - each source has its own `Folder`;
  - `Fold` runs only on change;
  - selection is kept by identity;
  - `WindowSizeMsg` → breakpoints;
  - the background-color probe works as in the cockpit;
  - `View.MouseMode = tea.MouseModeCellMotion` unless `m` turned it off.
- [x] Views 1–6 per § Architecture: flow (gate text `rej 2/3` beside the loop), events (tag column, follow mode), timeline (`BarSegments`, restart marks), metrics (`Sparkline`, totals, error count), summary (KV tree), queue (rows by state, reason, stale).
- [x] Every state in § States the operator can see, each with its wording.
- [x] `a`/`s`/`u` are bound but no-ops here. Task 7 adds their behavior.
- [x] Goldens at 60×30, 120×30, 120×50, 160×50, and 40×12 (floor), each with a nocolor variant, plus `--no-icons`.
- [x] A hostile-text test scans the rendered output for any ESC sequence that isn't lipgloss SGR (programmatic, not golden-only).
- [x] Commit `feat(tui): desk dashboard layout, flow and run panes`.

### Task 7 — Dashboard TUI, part 2: modals, switcher, replay, queue verbs, foreground runs

**Files:**

- Create: `internal/tui/desk_modal_unix.go`, `internal/cli/desk_run_unix.go`
- Modify: `internal/tui/desk_unix.go`
- Test: `internal/tui/desk_modal_unix_test.go`, `desk_queue_unix_test.go`, `internal/cli/desk_run_unix_test.go`

**Interfaces:**

- Consumes: Task 6 `RunDesk`/`DeskRun`, and desk `Claim`, `Launch`, `AttachScript`, `BeginRun`, `Finish`, `Skip`, `Unskip`, `Abandon`.
- Produces: `runForeground(d *desk.Desk, name, sha string, io IO) (rc int, err error)`.

**Dispatch:** Serial (after Task 6) · fresh Opus subagent, because this is the approval control.

**Steps:**

- [x] Add `Spec() *Spec` to the spec source (as an optional interface in `runview`), have the dashboard use it, and remove `DeskOptions.Specs`.
- [x] Modals: step, event (nested, `esc` closes one level), and the run switcher (`o`). A click selects, and a second click opens. A click outside closes a read modal, never the confirm modal.
- [x] Replay per § Keys.
- [x] Confirm modal on `a`:
  - It freezes a copy of the `QueueItem`.
  - Title: "Run `<name>` now — in the background / in this terminal".
  - It shows kind, WHAT, WHY, and `N lines · M bytes · K escaped runes`.
  - The body is undimmed, with line numbers, soft-wrapped and scrollable, with a "N more lines ↓" hint.
  - For a batch it adds "hash covers the manifest's lines, not the scripts it calls".
  - Footer: `y approve · n/esc cancel`.
  - `y` follows the binding rules in Global Constraints.
  - If a refresh no longer lists the item, or lists a different sha, the modal closes with "changed — re-review".
- [x] The `K escaped runes` count (M2) also counts `termsafe.IsInvisibleRune` hits and non-ASCII runes separately, and the lines holding them are marked.
- [x] Background item: `Claim(name, frozen.SHA256)` → `Launch(claimed)`. On any error after a successful `Claim` and before a successful `Launch`, call `Abandon(name, "launch-failed")` and show the error. Exception: when `Launch` returns a non-zero pid with its error, the supervisor started and owns the item; do not `Abandon`. Error messages:
  - `ErrChanged` from a mismatch: "changed since you opened it", then the modal re-opens on the new bytes.
  - `ErrClaimed`: "another desk claimed it".
  - `ErrRefused`: shown as returned.
- [x] TTY item: `RunDesk` returns `DeskRun{Name, SHA}` with no claim. The CLI loop then:
  1. claims with that SHA;
  2. calls `BeginRun(name, os.Getpid())`; an error here calls `Abandon(name, "launch-failed")`;
  3. starts `cmd := exec.Command("/bin/bash", desk.ScriptFDPath)` (I6: with no operand bash would read the terminal) with `claimed.AttachScript(cmd)`, cwd `$HOME`, `desk.ChildEnv()`, and `SysProcAttr{Setpgid, Foreground, Ctty}`;
  4. forwards INT, TERM, HUP, and QUIT to `-pgid`;
  5. sends KILL after `DefaultGrace`;
  6. takes the terminal back with `tcsetpgrp(tty, getpgrp())`, with `SIGTTOU` ignored around the call (M4). `EIO` on the terminal means the pane is gone: call `Finish` and exit;
  7. calls `Finish(rc, reason)`, with reason `ok`, `failed`, or `interrupted`;
  8. prints `ran <name>: exit N (<meaning>) — press any key`;
  9. re-opens with the restored `DeskViewState` and the new run selected.
- [x] Skip and unskip: `s` on a waiting or `lost` item shows the footer `skipped <name> · u to undo`. `u` works only on reason `operator`; any other reason is shown with its hint.
- [x] Tests:
  - an `a`,`y` burst within 400 ms does not claim;
  - `enter` or a click never confirms;
  - the claimed sha is 64 hex chars and equals the frozen copy; a prefix yields `ErrChanged`;
  - an item edited after the modal opened runs nothing and re-opens the modal;
  - rewriting `RecordPath` after the claim still runs the original bytes (foreground); a rewrite of the record and the meta runs nothing (background);
  - the foreground child's `$0` is `/dev/fd/3` and a marker the item writes exists;
  - an error after `Claim` and before `Launch` or `BeginRun` leaves the item in `skipped/` with reason `launch-failed`;
  - the child receives a forwarded SIGHUP, and `Finish` is written;
  - a SIGKILLed parent leaves an item that reads as `lost`;
  - a double approve from two models leaves one winner;
  - a `Launch` failure moves the item to `skipped:launch-failed`;
  - replay bounds;
  - every key in § Keys.
- [x] Before committing, run `git ls-remote origin refs/heads/feat/desk-core` as in Task 5.
- [x] Commit `feat(desk): approve, skip and run queued items from the dashboard`.

### Task 7b — Fixes from the early security review and the consumer dry run

**Files:** `internal/desk/{events_unix,supervise_unix,claim_unix,queue_unix}.go`, `internal/runview/{fold,model,source,source_desk_unix,source_summary_unix}.go`, the TUI glyph and word for the new step status, and their tests.

**Dispatch:** Parallel with Task 8 code, in a side worktree, then cherry-picked · fresh Opus subagent.

**Steps:**

- [ ] **A finished run never reads as `lost`.** `Watcher.state` returns `WatchEnded` when `done/` holds events with `RUN-END`, including on a resumed watcher.
- [ ] **The 32 MiB cap bounds a lost run's read too.** The lost path in `Poll` stops at the cap and reports the rest on later polls; the desk source sets `Delta.Partial` when the file passes the per-file cap.
- [ ] **A wrong `--sha` or `--kind` never moves a claimed item.** `supervise` refuses without touching the item when the argv sha differs from the claim stamp's `ApprovedSHA256`; the kind and both-kinds branches likewise refuse in place when the claim stamp says a different kind or sha was approved.
- [ ] **Minor items from the review that fit:** `Skip` of a running item uses rename-then-check; `PS4` joins the `ChildEnv` strip list; summary key collisions are counted and the first wins.
- [ ] **Reducer: a restart marks every running step `resumed`.** A resume that never re-sends `start` still shows.
- [ ] **Reducer: the attempt field is read from any event of that step** (`reject`, `close`, `fail`, unmapped), keeping the highest.
- [ ] **Reducer: an ended run turns a still-running step into `interrupted`,** a new status with its own glyph and word.
- [ ] **A summary-only run reads `ended`,** set by the source.
- [ ] **Booleans in summaries and event fields are kept as `true`/`false` text.**
- [ ] Commit `fix(desk,runview): finished runs, lost-run caps, resume and attempt tracking`.

### Task 8 — Module, CLI verbs, docs, ADR

**Files:**

- Create: `internal/cli/desk_unix.go`, `internal/cli/desk_other.go`, `docs/commands/desk.md`, `docs/adr/0010-desk-run-sources.md`
- Modify:
  - `internal/cli/modules.go` and `modules_test.go` (increment the existing `wantCount` by one)
  - `internal/cli/root.go` (drop `newDeskPlumbingCmd`)
  - `internal/cli/desk_supervise.go` (re-parent `_supervise`)
  - `docs/adr/README.md`
- Test: `internal/cli/desk_unix_test.go`

**Interfaces:** Produces these commands:

- `forgectl desk [--spec FILE]... [--no-icons]`
- `forgectl desk runs [--spec FILE]... [--json]`
- `forgectl desk show <run> [--from SOURCE] [--at N] [--json]`

**Dispatch:** Serial (after Task 7) · fresh Opus subagent: CLI paths, the JSON sanitization, and the supervisor path.

**Steps:**

- [ ] Register `deskModule` (`TierExtension`). Move `_supervise` under it, hidden, keeping the exact argv `desk _supervise --dir`. Add a test that `supervisorArgv` (now with the required `--sha` and `--kind`) resolves to a registered command. Delete the plumbing command. Replace the comment claiming a hand call "grants nothing" with the I7 statement: `done/` records what a supervisor ran, not proof of approval.
- [ ] The non-TTY refusal from Global Constraints (exit 1).
- [ ] The `desk` command's loop (`RunDesk` → `runForeground` → re-open).
- [ ] `desk runs` prints a table (run row format) or `--json` objects `{source, name, kind, live, updated, exit, partial}`.
- [ ] `desk show` prints the compact flow as plain text, or `--json` with the `RunState` plus counts. `--at` replays.
- [ ] A name shared by two sources exits 2 with `run "x" is in sources a and b; pass --from`. Both `--json` verbs go through `termsafe.JSONEncoder`.
- [ ] A bad spec fails on its own, with path and key; the other sources still load.
- [x] `docs/commands/desk.md` covers: a quickstart; the legend; keys, breakpoints, and states; the spec reference (experimental, neutral example); exit codes; the agent verbs, noting that `Scan` changes the queue.
- [x] ADR-0010 covers: the run-source model; the experimental spec; the trust and threat model with the agent deny rules, listing the rule shapes needed (any `forgectl … _supervise` form, an absolute binary path, `env forgectl …`) and stating that `done/` is a record of what ran, not proof of approval; cwd `$HOME` and the operator's environment minus the bash startup hooks; polling, not fsnotify; the alternatives declined.
- [ ] Tests:
  - the non-TTY refusal, including stdin-only and stdout-only cases;
  - the JSON shape;
  - an ambiguous name;
  - an isolated bad spec;
  - `GOOS=windows go vet ./...` stays clean (stubs).
- [ ] Commit `feat(desk): register the desk module with runs and show verbs`.

### Task 9 — Verification, polish, patch series

**Files:** none new.

**Dispatch:** In-context · Opus, with reviewer subagents.

**Steps:**

- [ ] Run each check, capturing its exit status to a log:
  - `gofmt -l .` prints nothing;
  - `go vet ./...` under `GOOS=darwin`, `linux`, `windows`, and `freebsd`;
  - `go test ./...`, with counts compared to the Task 1 baseline;
  - `golangci-lint run`;
  - `govulncheck ./...`.
- [ ] Live check against the consumer's real runs. The private copy of this plan, with the appendix, is written to the consumer repo's `docs/plans/` in this step, and the numbers go there only.
- [ ] Run the polish pass, with diff-based reviewers over `git diff "$M"..HEAD` from the worktree: an Opus security reviewer on Tasks 4, 5, 7, and 8, and a code reviewer on all of them. Apply the findings, then re-run the checks above. Known nits from Task 6: the full flow draws a trailing `▼` after the last step, and the 28-column run list cuts an 11-character name (`alph…un-7`). Known nits from Task 3: the `LoadSpec` read error repeats the file path, and `checkPattern` reports any `:` as \"must be a relative path\".
- [ ] Run the redaction scan, a manual identifier check, and a token diff against the appendix, over the diff, the messages, and the cover letter.
- [ ] Write the patches with `git format-patch --cover-letter --base="$D" -o <patches dir> "$D"..HEAD`. The cover letter carries:
  - what, why, and how;
  - `Testing:`;
  - the base: desk-core at `D`, with desk-tui's `912de47` included as patch 1;
  - the producer-tuple block;
  - `BEGIN_COMMIT_OVERRIDE`.

  Assert that the patch count equals `git rev-list --no-merges --count "$D"..HEAD`.
- [ ] Update the frontmatter.

## Deviations

- **desk-tui moved since planning.** `origin/feat/desk-tui` is now `bfbf432`, a merge of `D` into desk-tui. Its only commit not in `D` is still `912de47`, so the series shape is unchanged: patch 1 is `912de47`.
- **Baseline tests need `GOFLAGS=-buildvcs=false` on this machine.** Three `internal/exec` guards (`TestEveryProductionFileBuildsOnAGuardConfiguration`, `TestNoFileReachesPastTheTypeSystem`, `TestNoCallerCodeReceivesAnOpaquePayload`) shell out to `go list`, which fails with `error obtaining VCS status: signal: broken pipe` in this worktree. With VCS stamping off the package passes. The cause was not isolated. Every test run in this plan uses that flag.
- **Local toolchain is Go 1.27.1;** `go.mod` declares `go 1.26.8`.
- **Task 2 found 0 Critical, 7 Important, 9 Minor.** The orchestrator checked I1–I6 against the code at `D` before acting on them. All but M7 are now steps in Tasks 4, 7, and 8. The ones that change desk-core's public surface, which the PR #1059 owner should know about when merging:
  - **`Launch(name)` becomes `Launch(c *Claimed)`, and the supervisor takes `--sha` (I1).** Before this, the background path compared `running/<name>.sh` only with its own meta, so a consistent rewrite of both after `Claim` ran unapproved bytes (a probe confirmed it).
  - **`Claim` refuses an empty or short `wantSHA` (I2) and a kind mismatch (M1).**
  - **Meta gains a claim stamp, and `lost` covers PID-0 items whose claimer died (I3).** A claimed item with PID 0 previously could not be skipped (a probe confirmed it).
  - **Children run with cwd `$HOME` and without `BASH_ENV`, `ENV`, `BASH_FUNC_*`, `SHELLOPTS`, `BASHOPTS`, `CDPATH`, `GLOBIGNORE` (I5).**
  - **Task 7's foreground command gains its `/dev/fd/3` operand (I6).** Without it, bash would open an interactive shell on the operator's terminal.
- **Task 3 added `[log] attempt`.** The `start` row takes the attempt "from the event", but the example spec named no field for it. The optional key names that field; with it unset, each start adds 1. Task 3 also settled these points, each covered by a test: `check` is not an `[on]` action (checks come only from `[[checks]]`); `after` may name only an earlier step, so the graph has no cycles; `close` applies only to a running step; with no `stale_after` a run is never `stale`. The 2 500-event replay fixture is generated in the test from a fixed seed instead of stored under `testdata/`.
- **Task 4 shipped as `ad362bb`, `03b7b8a`, and `f40ce50`.** Choices beyond the plan text:
  - **The supervisor is bound to the claimed kind as well as the sha.** `_supervise` takes a required `--kind`. It refuses before `BeginRun` when the kind differs from the one found or from `meta.Kind`. It also refuses when both `running/<name>.sh` and `running/<name>.manifest` exist, and moves both files and the meta to `skipped/` with a `supervisor: … holds both …` reason (`f40ce50`).
  - **`Launch` can return a pid with an error.** That means the supervisor started but its pid was not written to meta. The caller must not `Abandon` then.
  - **`BeginRun` was reordered.** It opens the events file with `O_EXCL`, writes the owner, then checks the item is still in `running/`. This closes the gap in `Abandon`'s rename-then-check.
  - **`skipUnstarted` leaves an item alone** when another live process owns it, or when the meta cannot be read, so a stray `_supervise` cannot pull a live run out of `running/`.
  - **`BatchOptions` gained `WorkDir` (cwd for steps) and `EventDir` (relative `log=`).** `summary.json` keeps absolute `log` paths.
  - **The `Event*` constants moved to the portable `event.go`.**
  - **The `_supervise` comment (I7) was rewritten in Task 4,** since that file was already open. Task 8 still re-parents the command.
  - **M8 has no test.** It cannot be exercised on darwin; `GOOS=linux go vet` compiles it.
  - **The package's tests run with a scratch `HOME` and `TMPDIR`** (`TestMain`).
- **Task 5 shipped as `3f8ab0c`.** Choices beyond the plan text:
  - **The no-symlink rule opens each component with `O_NOFOLLOW` from the previous descriptor** (`O_DIRECTORY` for dirs, `fstat` on the leaf) instead of `Lstat` then open, so there is no window between check and open. `os.OpenRoot` still opens the root.
  - **`NewDeskSource` has no non-unix stub,** because `desk.Desk` exists only on unix and every caller is unix-only.
  - **`[log] show` is applied by the renderer (Task 6), not at ingest.** Every kept field stays, in file order.
  - **Desk events carry no time.** Step times come from a new `Delta.Timing` (`BatchStatus`, else `STEP-END dur=`, else the item's start and end).
  - **`STEP-END` with a non-zero rc is renamed `STEP-FAIL`,** since `Fold` maps actions by event name only.
  - **Additions:** `Cursor.Metrics` and `Cursor.Summary` (per-file position and identity), `Delta.DroppedFields` (values, apart from `Dropped` lines), `Delta.Live`, `Delta.Timing`, `QueueItem.Refusal` (why an item cannot be approved, such as a hash mismatch), the `QueueSource` interface, `ErrRefused`, `ErrRoot`.
  - **Summaries use the same scalar rule,** so floats are dropped. `[log] time` reads RFC 3339 text or integer epoch seconds. Blank lines are skipped and not counted.
  - **"No log yet" is `errors.Is(Err, fs.ErrNotExist)` with no events ever received;** a vanished summary also sets such an `Err`.
- **Task 6 shipped as `9569d48`.** Choices beyond the plan text:
  - **The exported types live in a portable `desk_types.go`,** so the non-unix stub exports the same names. `DeskAction` is an interface; nil means none.
  - **`DeskOptions` gained `Specs` and `Failed`.** `Failed` carries specs that failed to load, for the `1 source failed` chip. Task 7 replaces `Specs` with a `Spec()` method on the spec source, so the CLI cannot forget to pass it.
  - **The full-flow height rule counts DAG bands, not steps** (`3 + 6·bands + 7`), since a wave shares one band.
  - **Runs other than the selected one load only when new or when `Updated` moved, at most 12 per list reload.** Staleness is judged at draw time from `LastEvent`.
  - **In the wide and mid layouts, view 1 focuses the flow and views 2–6 fill the side pane;** `/` filters events in the mid layout.
  - **A run with no log yet shows `· new`.** Hostile-text tests run with `NoIcons`, because the icon set includes a private-use rune.
  - **Task 7 seams:** `deskHooks` (`open`, `verb`, `switcher`, `replay`), `deskModel.action`, and the `onKey` order.
- **Task 7 shipped as `99ef804`.** Choices beyond the plan text:
  - **`QueueItem` gained `Bytes` and `Marks`,** so the escaped, invisible and non-ASCII counts (M2) come from the raw bytes, not the escaped body.
  - **A prefix sha is refused with `ErrRefused`,** not `ErrChanged`, since Task 4 refuses any sha that is not 64 hex digits.
  - **The dialog re-opens on new bytes only when the item was re-queued with a new hash.** If only the bytes changed, desk-core skips it as `changed`.
  - **The real-terminal path (`Foreground`, `Ctty`, taking the terminal back) has no automated test,** because the repo has no pty module. It needs a hand check in Task 9.
  - **`DeskOptions.Specs` is gone;** the dashboard reads `Spec()` from the spec source.
- **Task 8's docs shipped first as `876e3a7`,** written from the plan before the CLI existed. Task 8's code must match them: `desk runs --json` prints one JSON array; the `desk show --json` shape and the desk directory lookup order are as documented there. `[[steps]] after` is documented though the example does not use it.
- **An early security review of Tasks 4 and 5 (0 Critical, 3 Important, 5 Minor) and a dry run of the consumer's spec found the gaps Task 7b fixes.** Each Important finding and each reducer gap was checked against the code first. Keeping booleans as text changes the Global Constraints JSON rule: strings, int64 integers, and booleans are kept.
- **M7 is left to the desk-core owner.** The batch runner opens `done/<name>.d` paths by absolute name instead of through the pinned root. Only same-uid code reaches it, so it is consistency hardening outside this series.

## Learnings

- `D` = `f48e4f4b03712b5ea652c6cee7ea2c705eadf077` (`fix(desk): close the inherited script fd; skip unreadable files`).
- `M` = `7004ac0d5156169c11593d58dbfa8fb415a05c3d` (desk-tui merged with `--no-ff`). The merge preview's conflict set was empty.
- Baseline at `M`: build `0`, vet `0`, 70 packages `ok` with `-buildvcs=false`.
- Every API cited in Tasks 4–8 resolves at `D` unchanged: `Claim` (`claim_unix.go:86`), `Claimed.RecordPath`, `AttachScript` (func and method), `Launch` (`supervise_unix.go:39`), `BeginRun`, `Run.Finish`, `Skip`, `Unskip`, `Prune`, `Scan`, `NewWatcher`/`Poll`, `LoadManifest`, `Waves`, `supervisorArgv` (`supervise_unix.go:22`), `pickGlyphs` (`tui/styles.go:60`), `DefaultGrace` (5 s), `wantCount = 34`, `SafeLineMax` (`termsafe.go:134`).
