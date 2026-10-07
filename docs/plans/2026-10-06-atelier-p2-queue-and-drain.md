---
status: planned
next: "Proposed, awaiting Cameron's approval and his answer to the autonomy question. On approval: T8.1 (cobra-free worker ops), T8.2 (queue), T8.3 (drain), T8.4 (notify, review, live check)."
branch: plan/atelier-p2-drain
pr: "—"
updated: 2026-10-06
approved_session_id: "—"
date: 2026-10-06
session_id: 30dd3ebb-3720-463f-aa15-9570c1ff88a9
model: claude-opus-5-5
harness: claude-code 2.1.289
machine: cf6e768835c7
source_plan: "cadence-ecosystem docs/plans/2026-10-05-atelier-a-herdr-work-queue-cockpit-for-claude-code.md § P2; docs/plans/2026-09-28-forgectl-herdr-coordinator.md T8"
---

# atelier P2: queue and drain (forgectl T8)

## Goal

The coordinator session puts tasks on a queue (`surface enqueue`), and a detached `surface drain` launches them as herdr claude workers, at most 3 at a time and 1 per repo. The drain watches each worker and records when it reports, fails, goes quiet, or stops at a permission prompt. A worker stopped at a prompt is `needs-you`: the normal checkpoint, not an error. The operator answers it in the worker's pane.

Workers stay capped at `acceptEdits` and run as the operator (ADR-0010, 2026-10-06 amendment). P2 is built around that and does not depend on any change to it.

## Question for Cameron

**Should P2 allow more autonomy?** Nothing below depends on the answer; each option other than the first is a later, separate PR.

Today's cost, stated plainly: workers start with `--setting-sources ""` (`docs/herdr.md`), so none of your user allow rules load. Under plain `acceptEdits`, `go test`, `git commit` and `make` prompt, not only `git push` and `gh`. A worker will sit at `needs-you` several times per task.

- **No change (recommended for P2's first ship).** Every shell command waits for you in the worker's pane. The drain still does the launch, the brief, the watching, and the per-repo cap. Measure how often workers stop before choosing an allow list.
- **Pre-approve a short list of local commands** (for example `go test`, `git commit`) in the worker's `--settings` `permissions.allow`. `git push` and `gh` typed by the model still prompt, but a pre-approved `go test` or `make` runs the repo's own code, which can push with your identity. Under the "accidents, not adversaries" boundary that may be fine. It loosens a default, so it gets its own small PR and a security review.
- **`auto` mode.** Needs the deferred sandbox and worker App (forgectl#1134) first.

## Loop

| Thing created | Opened by | Closed by | Who closes |
|---|---|---|---|
| Queue row | `surface enqueue` | `surface dequeue` (any state without a live worker); drain marks `expired` after 7 days queued; terminal rows pruned after 30 days | operator, or the drain |
| Worker (workspace, worktree, ledger row) | the drain, through the existing launch path | existing `surface close` (operator); its queue row then reads `closed`. P4 adds close-after-merge | operator |
| Drain process and its lock | `surface drain start` | `surface drain stop`; a crash releases the flock and the next `start` takes over | operator, or the kernel |
| `drain-events.jsonl` | the drain | renamed to `.1` at 1 MiB, replacing the previous `.1` | the drain |
| `needs-you` notification | the drain, once per entry into `needs-you` | the row leaves `needs-you` on the next tick after the worker moves on | operator, by answering in the pane |

## Alternatives declined

- **Reuse `internal/pr`'s drain code.** It is bound to PR session records, tmux windows, and `*pr.Client` (`internal/pr/drain.go:128-300`). P2 copies its rules instead: claim oldest-first under one lock hold, launch outside the lock, retry only a failure that happened before anything was created (`drain.go:318-333`). This is the "or the T8 plan records why it cannot" branch of the coordinator plan's amendment.
- **Reuse the desk queue (`internal/desk`).** Desk items are scripts the operator approves one by one and runs once; queue rows are briefs that launch long-lived workers under a cap. The desk's supervisor pattern and pid check are reused; its queue is not.
- **Adopt rota.** The 2026-10-05 prior-art study (`docs/research/2026-10-05-herdr-orchestrator-prior-art.md`, meta-repo) read it. It owns its own worker launch and state, so it would bypass forgectl's worker floor, ledger, and readiness predicates, which are the parts P0–P5 hardened.
- **`queue.jsonl` (append-only).** The ledger's file helpers do whole-document rewrites with verified open, flock, and atomic rename (`internal/surface/worker/ledger_unix.go`). A bounded `queue.json` reuses them unchanged. Events stay append-only.
- **Auto-close a stopped worker after 60 minutes (parent plan).** Under `acceptEdits` a stopped worker is waiting for the operator, often for hours; closing it would throw its work away. The parent's "no progress → needs-you" rule stays, as the idle rule below.
- **Worker rows in the desk TUI.** The desk has no source interface (`internal/tui/desk_model_unix.go:78` is a test seam), and the P6 atelier pane is the cockpit. P2 sends notifications only.
- **`lumberjack` for event rotation.** One rename at 1 MiB is a few lines; a dependency is not worth it.
- **A heartbeat file.** The status file records pid, process start time, and last tick.

## Design

### Queue (`queue.json`)

- Location: `$XDG_STATE_HOME/forgectl/surface/queue.json`, through the ledger's `privdir` pin and verified open (0600 file, 0700 dir, no symlink follow, flock on `queue.lock`). One queue per machine; `name` is unique machine-wide. `drain.lock`, `drain.json` (status), and `drain-events.jsonl` use the same pin and open helpers.
- Size: the document stays under 768 KiB, below the ledger's 1 MiB read cap. `enqueue` refuses, before writing, a row that would push it past that, naming the current size. Terminal rows drop their brief text and keep `brief_sha256`.
- Row: `name`, `repo` (top-level path), `brief` (text, 64 KiB cap), `brief_sha256`, `batch`, `state`, `attempts`, `last_error`, `launch_id`, `enqueued_at`, `state_at`, and the ledger key once launched.
- States: `queued → claimed → launched ⇄ needs-you → reported | failed | closed`; `queued → expired | dequeued`. `claimed` is set under the queue lock only from `queued`.
- `enqueue --repo <path> --name <slug> --brief <file> [--batch <id>] --json`: claude only. Idempotent on `name`: the same brief hash is a no-op that prints the row's current state; a different brief is an error naming both hashes. The file is read once, checked with `worker.CheckBrief(..., ViaLaunch)`, and stored as text. A leading `@` is refused.
- `dequeue <name>` removes a row whose worker is not live (any state except `claimed`, `launched`, `needs-you`). After `dequeue`, the same name can be enqueued again; that is the way back from `failed`.
- `queue --json` lists rows with their age in state.

### Slots

A slot is held by a row in `claimed`, and by any row whose ledger row is at `pending`, `worktree`, or `launched`. Holding is read from the ledger, not from the queue state, so a `failed` row with a live worker still holds its repo's slot until the operator closes it. Caps: `cap` (default 3, max 10) and `per_repo` (default 1).

### Drain process

- `drain start` re-runs a hidden `forgectl surface _drain` with `Setsid`, `/dev/null` stdin, and `context.Background()`, as `internal/desk/supervise_unix.go:73` does. The child keeps `drain start`'s environment (PATH, `gh` auth, herdr socket); workers still get only the ADR-0010 allowlist. `start` resolves and pins the herdr server and session the same way `surface launch` does, and records them in `drain.json`. `start` then waits up to 5 s for `drain.json` to show `running`; otherwise it exits 1 and prints the last events line.
- `_drain` takes `drain.lock` with `LOCK_EX|LOCK_NB`; a second start exits 1 naming the holder's pid and start time.
- `drain status --json`: `running | paused | stopped | stale` (stale: last tick older than 3 intervals), the pause reason, pid, herdr session, last tick, counts per state, and the rows in `needs-you` or `failed` with their `last_error`.
- `drain stop`: SIGTERM only when the recorded start time is non-zero and matches the live process's readable start time. A copy of `desk.processAlive` with that stricter rule lives in a small shared helper. The drain finishes its current step and exits.
- Config: `[surface.drain]` (`interval` default 15 s, floor 5 s; `cap`; `per_repo`; `notify` default `true`; `idle_minutes` default 10). The drain reloads the config file each tick with the normal loader. An invalid present value pauses claiming with the reason in status and events; it never falls back to a default.

### Tick

Each step is a pure decision over (queue rows, ledger rows, probe results, clock), wrapped by thin I/O.

1. **Watch** each row with a live worker, using one screen read through the readiness predicates (no waiting):
   - Report line for the row's marker → `reported`.
   - `blocked` (permission prompt or any blocking screen) → `needs-you`, notify once.
   - Any other verdict while in `needs-you` → `launched`.
   - At the prompt (`ready`) with no report for `idle_minutes` → `needs-you` with reason `idle without report`, notify once. This also catches a report that scrolled off the screen.
   - Ledger row `closed` → `closed`. Ledger row `failed`, or workspace `gone` → `failed`.
   - `unreadable` → unchanged; one event per row per state change, not per tick.
2. **Expire** `queued` rows older than 7 days.
3. **Claim**, unless paused: oldest `queued` rows first, under the queue lock, while slots allow. Write `launch_id` into the row, then set `claimed`.
4. **Launch,** outside the lock, through the T8.1 in-process launch with the row's brief text. At launch, re-check `brief_sha256` and `CheckBrief`. On failure, the decision reads the ledger row the attempt left:
   - No ledger row, or one that created nothing (`createdNothing`, `ledger.go:147`; the GitHub base lookup fails here) → back to `queued`, `attempts+1`; at 3 → `failed`.
   - Anything else (a worktree, workspace, or ref exists) → `failed` at once, with `last_error` naming the error, the worktree path, and the ledger stage. Retrying would hit `ErrNameTaken`. The operator runs `surface close`, then `dequeue` and `enqueue` to retry.
   - A GitHub auth error, or herdr unreachable → the row goes back to `queued` with no attempt counted, and claiming pauses with that reason. An auth pause clears on the next `drain start`; a herdr pause is retried each tick.
5. **Prune** terminal rows older than 30 days.

**Startup reconcile** of `claimed` rows, by the ledger row with the same name:

| Ledger row | Queue row becomes |
|---|---|
| none, or `launch_id` does not match | `queued` (no attempt counted) |
| created nothing | `queued` (no attempt counted) |
| `pending` or `worktree`, no ref | `failed`, `last_error` names the worktree; operator closes |
| `launched` | `launched` |
| `failed` | `failed` |
| `closed` | `closed` |

### Events

One JSON line per state change and per pause or resume in `drain-events.jsonl` (`O_APPEND`): `{v:1, ts, seq, kind, name, repo, state, attempt, error}`. `seq` counts from 1 for each drain process and is in `drain.json`; a reader's cursor resets on a restart. `drain events [--since <seq>] --json` prints them. Error text names the expected value and the value seen.

### What P2 does not do

No merge, PR status, or close after merge (P4). No intake from GitHub or the board (P3). No pane (P6). No codex workers (forgectl#1092 first). No change to worker posture or identity.

## Global Constraints

- The drain acts only on queue rows and ledger rows whose `launch_id` it wrote.
- Briefs come only from `surface enqueue` by the operator or the coordinator session. Nothing untrusted enters the queue in P2.
- The drain types nothing into a pane and never answers a prompt. The brief goes in at launch as the harness argument (T3).
- Every drain launch goes through `buildWorkerInvocation` and the `!built.Worker` refusal (`internal/cli/surface_worker.go:94-96`), so the worker floor and env allowlist apply.
- Notifications name the worker's pane from its ledger ref, never the drain's own `HERDR_PANE_ID`.
- An errored herdr read is `unreadable`, never `gone`.
- Commits carry the producer-tuple trailers; release-please writes the changelog.

## Known boundary (ADR-0010 amendments; recorded, not designed against)

- A worker can print its own report line early. Its row goes to `reported`, its repo's slot frees, and a second worker can start in the same repo. P4 must check a report against git before acting on it.
- The drain's unattended git calls in the main checkout (base lookup, `worktree add`) run with config an earlier worker could have planted (amendment 1, I1).
- The binary-override env var and `PATH` are read once, at `drain start`.

## Orchestrator

**Driver:** opus. Trigger: a background process that launches workers unattended under the operator's identity.

## Tasks

### T8.1: cobra-free worker operations (one PR)

- [ ] Give launch, screen read, and ledger open an `io.Writer` in place of `cmd` (`surface_ready.go:144`, `surface_list.go:118`, `surface_worker.go:137`).
- [ ] Split `launchBrief` so an in-process caller passes brief text directly; `readBriefArg`'s `@path` handling stays CLI-only.
- [ ] Return the ledger row the attempt left alongside the error, so a caller can tell a failure before creation from one after.
- [ ] Keep `buildWorkerInvocation` and the `!built.Worker` refusal on the in-process path. Test: a non-worker invocation is refused, and a sentinel env var set on the caller does not reach the worker's `Invocation.Env`.
- [ ] No behavior change for the CLI; existing tests stay green.

### T8.2: queue store and verbs (one PR)

- [ ] `queue.json` store in package `worker`, reusing `openVerified`, `readAt`, `writeAt` with its own name and lock.
- [ ] `surface enqueue`, `dequeue`, `queue --json` with the rules above.
- [ ] Tests: same brief no-op; different brief refused with both hashes; leading `@` refused; size cap refused before write; `dequeue` of a live row refused; `dequeue` then `enqueue` of a failed row works; unknown version refused; two claimers of one row, one wins.

### T8.3: the drain (one PR)

- [ ] `surface drain start|stop|status|events`, `_drain`, `drain.lock`, `drain.json`, events file with its one-file rotation.
- [ ] Config section, reload each tick, pause on an invalid value.
- [ ] The tick as pure decision functions plus I/O.
- [ ] Tests: caps (global, per repo, a `failed` row with a live ledger row holds a slot); retry only when nothing was created; `failed` at once with the worktree named otherwise; 3-attempt cap; pause on auth and herdr errors with no attempt counted; idle-without-report rule; `needs-you` clears on any non-blocked verdict; ledger `closed` maps to `closed`; expiry; prune; every startup-reconcile row in the table; a kill at each launch stage then restart gives the table's outcome; second `start` refused; `stop` refuses a zero or mismatched start time; `start` reports a child that never reached `running`.
- [ ] Mutation sweep over the slot rule, the claim compare-and-set, the retry rule, and the reconcile table.

### T8.4: notify, security review, live check (one PR or folded into T8.3)

- [ ] Split a generic notifier out of `deskSignal` (`internal/cli/desk_signal_unix.go`): macOS notification through `internal/notify`, and herdr pane state on the worker's pane from its ledger ref.
- [ ] Security review (an Opus-tier security reviewer) of the control's file set before the first `drain start`: the T8.1 diff, `internal/cli/surface_worker.go`, `internal/launch/invocation.go` (`applyWorkerFloor`, `workerBaseEnv`, `buildWorkerInvocation` callers), `internal/surface/worker/{ledger,ledger_unix,brief}.go`, `internal/herdr/ready/`, `internal/desk/proc_unix.go`, `internal/cli/desk_signal_unix.go`, `internal/notify/notify.go`, and the new queue and drain files.
- [ ] Live check on sjomba with a real batch (open forgectl issues small enough for one worker): enqueue 3 tasks across 2 repos; the per-repo cap holds; a worker at a permission prompt shows `needs-you` and notifies; answering it returns the row to `launched`; a report marks it `reported`; `drain status` lists rows needing attention. Record how often workers stopped, for the autonomy question.

## Verification

- `go test ./...` per PR; the T8.3 mutation sweep.
- The T8.4 live check, with commands and output recorded in Deviations.

## Panel

Panel: plan-reviewer, security-posture-reviewer (Opus), operability-reviewer, cameron-review ran — 5 Critical, 13 Important, 1 High (security) folded in; 3 declined (see below)

## Panel review findings declined

- **[Owner lens] Move `drain events` and rotation to P6.** Kept: the events file is how the operator sees a failure or pause before the pane exists (operability I3).
- **[Owner lens] Lower the security review bar given "doesn't have to be bulletproof".** Kept: this is the first unattended launcher, and the coordinator plan's T0 amendment requires the review. Raised as a heads-up to Cameron instead.
- **[Plan reviewer] Split T8.3 further.** T8.4 takes notify, review, and the live check; the drain core stays one PR because its parts are tested together.

## Deviations from the parent plans

- **`queue.json`, not `queue.jsonl`;** no heartbeat file; no auto-close at 60 minutes (see Alternatives declined).
- **`enqueue` drops `--base` and `--source`.** The base is always GitHub's default head (forgectl#1061); `source` arrives with P3 intake.
- **No `--harness codex` in P2** until forgectl#1092 isolates codex workers.
- **Event kinds** are the ones P2 emits (state changes, pause, resume). P4 and P6 add theirs (`verdict`, `merged`, …) when they ship.
- **"intent → act → confirm" per step** became a launch-stage reconcile table plus a kill-at-each-stage test, because the existing launch already writes its ledger row before each step.
- **Retry cap is 3 per row, not per step,** and only for failures that created nothing.
- **P7a (skill enqueue step) ships separately** in `cameronsjo/cadence` after T8.2; until then the coordinator calls `surface enqueue` directly.

## Learnings
