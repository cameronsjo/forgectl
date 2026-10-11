---
status: in-flight
next: "T8.5 built on feat/worker-profile-model (profiles, --model, claude-slots gate); needs its PR and review. Then the full-harness + scoped-SendMessage PR"
branch: plan/atelier-p2-drain
pr: cameronsjo/forgectl#1137
updated: 2026-10-08
approved_session_id: "— (approved on forgectl#1137 by chief-of-staff on Cameron's go-ahead, 2026-10-07)"
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

Workers stay capped at `acceptEdits` and run as the operator (ADR-0010, 2026-10-06 amendment), with a fixed pre-approved command list (Autonomy decision, T8.0). `needs-you` is what remains: commands off the list, such as merges.

## Autonomy decision (2026-10-07)

Approved on forgectl#1137 by chief-of-staff on Cameron's go-ahead: **more autonomy by an allow list, not a broader mode.** Workers keep `acceptEdits`. The worker launch adds one static `permissions.allow` list: the repo's build and test commands (`go test`, `go build`, `make`), `git add`, `git commit`, `git push` to the worker's own branch, and `gh pr create` / `gh pr view`. Pushing to `main` and every merge stay off the list; merges keep going through the review-marker gate. No per-repo config until a second repo needs a different list. This is task T8.0.

What the list does and does not hold, stated plainly:

- **Push to `main` is refused by GitHub, not by the list.** A Claude Code prefix rule matches the command's start, so `Bash(git push:*)` also allows `git push origin HEAD:main`. On forgectl and cadence the `estate-main` rulesets refuse a direct push to `main` (pull request required, bypass in pull-request mode only). A repo without such a ruleset has no block.
- **Pre-approved build commands run repo code.** `go test` and `make` execute the repo's own code, which can push or merge with the operator's identity without a prompt. This sits inside ADR-0010's "accidents, not adversaries" boundary.
- `gh pr merge`, `gh api`, and anything else off the list still prompt.

Before the decision, under plain `acceptEdits`, `go test`, `git commit` and `make` all prompted (workers start with `--setting-sources ""`, so no user allow rules load).

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
- States: `queued → claimed → launched ⇄ needs-you → reported | failed | closed`; `queued → expired`; `dequeue` removes a row rather than marking it. `claimed` is set under the queue lock only from `queued`.
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

No merge, PR status, or close after merge (P4). No intake from GitHub or the board (P3). No pane (P6). No codex workers (forgectl#1092 first). No other change to worker posture, and none to identity.

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

### T8.0: worker command allow list (one PR, first)

- [x] Add the static `permissions.allow` list from the Autonomy decision to the worker's inline `--settings` (`workerClaudeEditSettings`, `internal/launch/invocation.go`), for `acceptEdits` workers only. Rules use the word-boundary form: `Bash(go test *)`, `Bash(go build *)`, `Bash(make *)`, `Bash(git add *)`, `Bash(git commit *)`, `Bash(git push *)`, `Bash(gh pr create *)`, `Bash(gh pr view *)`.
- [x] Update the ADR-0010 amendment and `docs/herdr.md` ("What a claude worker loads") to name the list and the two limits above.
- [x] Tests pin the settings JSON. Live check: a worker runs `go test` and `git commit` with no prompt, and `gh pr merge` still prompts.
- [x] Security review (Opus) of the diff before merge; this loosens a default.

### T8.1: cobra-free worker operations (one PR)

- [x] Give launch, screen read, and ledger open an `io.Writer` in place of `cmd` (`surface_ready.go:144`, `surface_list.go:118`, `surface_worker.go:137`).
- [x] Split `launchBrief` so an in-process caller passes brief text directly; `readBriefArg`'s `@path` handling stays CLI-only.
- [x] Return the ledger row the attempt left alongside the error, so a caller can tell a failure before creation from one after.
- [x] Keep `buildWorkerInvocation` and the `!built.Worker` refusal on the in-process path. Test: a non-worker invocation is refused, and a sentinel env var set on the caller does not reach the worker's `Invocation.Env`.
- [x] No behavior change for the CLI; existing tests stay green.

### T8.2: queue store and verbs (one PR)

- [x] `queue.json` store in package `worker`, reusing `openVerified`, `readAt`, `writeAt` with its own name and lock.
- [x] `surface enqueue`, `dequeue`, `queue --json` with the rules above.
- [x] Tests: same brief no-op; different brief refused with both hashes; leading `@` refused; size cap refused before write; `dequeue` of a live row refused; `dequeue` then `enqueue` of a failed row works; unknown version refused; two claimers of one row, one wins.

### T8.3: the drain (one PR)

- [x] `surface drain start|stop|status|events`, `_drain`, `drain.lock`, `drain.json`, events file with its one-file rotation.
- [x] Config section, reload each tick, pause on an invalid value.
- [x] The tick as pure decision functions plus I/O.
- [x] Tests: caps (global, per repo, a `failed` row with a live ledger row holds a slot); retry only when nothing was created; `failed` at once with the worktree named otherwise; 3-attempt cap; pause on auth and herdr errors with no attempt counted; idle-without-report rule; `needs-you` clears on any non-blocked verdict; ledger `closed` maps to `closed`; expiry; prune; every startup-reconcile row in the table; a kill at each launch stage then restart gives the table's outcome; second `start` refused; `stop` refuses a zero or mismatched start time; `start` reports a child that never reached `running`.
- [x] Mutation sweep over the slot rule, the claim compare-and-set, the retry rule, and the reconcile table.

### T8.4: notify, security review, live check (one PR or folded into T8.3)

- [x] Split a generic notifier out of `deskSignal` (`internal/cli/desk_signal_unix.go`): macOS notification through `internal/notify`, and herdr pane state on the worker's pane from its ledger ref.
- [x] Security review (an Opus-tier security reviewer) of the control's file set before the first `drain start`: the T8.1 diff, `internal/cli/surface_worker.go`, `internal/launch/invocation.go` (`applyWorkerFloor`, `workerBaseEnv`, `buildWorkerInvocation` callers), `internal/surface/worker/{ledger,ledger_unix,brief}.go`, `internal/herdr/ready/`, `internal/desk/proc_unix.go`, `internal/cli/desk_signal_unix.go`, `internal/notify/notify.go`, and the new queue and drain files.
- [x] Live check on sjomba with a real batch (open forgectl issues small enough for one worker): enqueue 3 tasks across 2 repos; the per-repo cap holds; a worker at a permission prompt shows `needs-you` and notifies; answering it returns the row to `launched`; a report marks it `reported`; `drain status` lists rows needing attention. Record how often workers stopped, for the autonomy question.

### T8.5: forgectl as the launcher: worker profiles, model, session cap (one PR)

- [x] `[surface.profiles.<name>]` with `config_dir` (absolute or `~/`, checked at config load; `main` is reserved for the launcher's own `CLAUDE_CONFIG_DIR`). `surface launch --worktree ... --profile <name>` and `surface enqueue ... --profile <name>` set the worker's `CLAUDE_CONFIG_DIR` through `InvocationRequest.ConfigDir`, applied after the env merge, so the transcript path follows it. A queue row stores the name only; the drain resolves it at launch, and an unknown name is a launch-config failure (requeue, pause).
- [x] `--model <name>` on `surface launch --worktree` and `enqueue`, stored on the row, applied as `InvocationRequest.Model` (worker only; effort derived again), checked as a plain token (`config.CheckModelName`) at the CLI, in the store, in the drain's re-check and in `BuildInvocation`.
- [x] claude-slots gate: `_drain` resolves `claude-slots` on `PATH` at start (`drain.json` `claude_slots_path`); after the claim and re-check it runs `claude-slots check <N>` (5 s cap; N counts this tick's launches, see Deviations). Exit 0 launches; exit 1 requeues quietly with no attempt and stops claiming that tick, with one `slots-held` event per entry; anything else, and a missing binary (one `note` at start), launches. `drain.DecideSlots` is the pure decision.
- [x] Tests and staged breaks (Deviations); docs in `docs/herdr.md` and `docs/configuration.md`.

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

- **Workers are full harnesses (Cameron, 2026-10-08).** Claude workers drop the settings isolation (`--setting-sources ""`, `--strict-mcp-config`, `--no-chrome`, `--safe-mode`) and the `SendMessage`/`RemoteTrigger` deny rules, and load what an ordinary session loads: user settings and allow rules, plugins, skills, hooks, MCP servers, `CLAUDE.md`, project memory. The planned launcher-scoped `SendMessage` hook is dropped (nothing is denied, and herdr-bridge reaches every pane anyway). Kept: the permission-mode ceiling, the environment allowlist, `useAutoModeDuringPlan = false`, the `acceptEdits` allow list. ADR-0010 2026-10-08 amendment. pi and codex workers are next.

- **`queue.json`, not `queue.jsonl`;** no heartbeat file; no auto-close at 60 minutes (see Alternatives declined).
- **`enqueue` drops `--base` and `--source`.** The base is always GitHub's default head (forgectl#1061); `source` arrives with P3 intake.
- **No `--harness codex` in P2** until forgectl#1092 isolates codex workers.
- **Event kinds** are the ones P2 emits (state changes, pause, resume). P4 and P6 add theirs (`verdict`, `merged`, …) when they ship.
- **"intent → act → confirm" per step** became a launch-stage reconcile table plus a kill-at-each-stage test, because the existing launch already writes its ledger row before each step.
- **Retry cap is 3 per row, not per step,** and only for failures that created nothing.
- **Autonomy allow list (T8.0) added at approval,** per the Autonomy decision; it is the only change to worker posture in P2.
- **T8.0 live check (sjomba, herdr 0.9.3, Claude Code 2.1.289).** A worker in the trusted forgectl repo ran `go test ./...` and `git add note.txt && git commit -m probe` with no prompt; `gh pr merge --help` stopped at "This command requires approval", and `surface wait` returned `blocked: showing the permission prompt`. A first probe in an untrusted scratch repo stopped at the folder-trust dialog, which the coordinator does not answer.
- **T8.0 security review: the list does not bound a worker.** `go build -toolexec` and `git push --receive-pack`/`--exec` run any command through a pre-approved rule, as do `go test` (`-exec`, test code) and `make`. Docs and the code comment now say so; no flag denylist was added, because it could never be complete. The `--receive-pack` path is from git's manual, not run.
- **`auto` allowed for workers (2026-10-07, chief-of-staff, forgectl fix PR).** Cameron's work machine reported forgectl refusing `auto`; the worker cap is now `auto` (ADR-0010 amendment). A classifier block does not show a prompt: the worker gets the reason and tries another way. Claude Code falls back to a prompt after 3 blocks in a row or 20 in total, and only then does the drain see `needs-you`. The drain's design is unchanged; `[launch.worker]` stays `acceptEdits` by default.
- **T8.0: the allow list reaches `acceptEdits` workers only.** The first cut put it in the one settings value every worker gets; the worker-floor test showed `plan`, `default` and `manual` workers receiving it, which would let a plan worker commit and push unprompted. A second constant carries the list, chosen by the leading `--permission-mode` value. Rules use `Bash(<cmd> *)`, which enforces a word boundary, rather than the `:*` form.
- **P7a (skill enqueue step) ships separately** in `cameronsjo/cadence` after T8.2; until then the coordinator calls `surface enqueue` directly.
- **T8.1 exports `worker.CreatedNothing`** (was `createdNothing`) so the drain can apply the retry rule to the row an attempt left; the in-process launch is `launchWorker(ctx, warn, deps, workerSpec, briefText) (workerAttempt, error)`, and `workerAttempt.createdNothing()` is the rule. A row that cannot be read back after an attempt counts as having created something, so the drain fails it rather than retrying.
- **T8.1 routes the CLI launch through the same `attemptWorker` core as the drain,** so both paths share the `!built.Worker` refusal. The only CLI-side difference is one ledger read after the attempt, which changes no output or exit code.
- **T8.2: the row's "ledger key" is `session`,** the herdr session name, set at launch. With `repo` and `name` it names the ledger row (`worker.Open(repo, session)`), which the hashed ledger key alone cannot reopen.
- **T8.2: a name is refused for another repository too,** not only for another brief: `name` is unique machine-wide, so a same-brief enqueue aimed at a second repository is an error naming the first.
- **T8.2: the leading-`@` rule applies twice.** The brief text may not start with `@` (after leading whitespace), and `--brief` refuses an `@path` argument, since it takes a path and `surface launch --brief @file` would otherwise read the same.
- **T8.2: every queue write drops the brief text of terminal rows** (`TrimTerminal`), so the drain needs no separate trim call. `--batch` takes the worker-name character set, up to 64 characters. `surface queue --json` omits brief text as the text output does.
- **T8.3: a ledger row carries `launch_id`.** The plan matches ledger rows by `launch_id` but the ledger had no such field; `worker.Row.LaunchID` (omitempty, written by `Begin`) holds it, so a CLI launch's ledger file is unchanged. A row with the name but another or no `launch_id` is not the drain's.
- **T8.3: the claim records the herdr session too** (`Queue.ClaimFor`), in the same locked write as `launch_id`, so a restart always knows which ledger to read.
- **T8.3: claimed rows are settled at the start of every tick,** not only at startup. The drain launches every row it claims within the claiming tick, so a claimed row at a tick's start is always a dead launch's; the startup case is the first tick. An unreadable ledger leaves the row claimed (holding its slot) and is retried next tick, with one event per change: `Reconcile` takes the row's `Memo` as `Watch` does (review I4).
- **T8.3: claim then launch, one row at a time.** Each claim takes the queue lock on its own, and the launch follows outside it, so a pause raised by one launch (auth, herdr) stops the next claim in the same tick.
- **T8.3: herdr readiness is checked before claiming** (`herdradapter.Adapter.CheckReady`), whenever a row could be claimed or a herdr pause is held. A herdr failure inside a launch happens after `git worktree add`, so it would leave a worktree and a retry would hit `ErrNameTaken`; such a row is `failed` (and claiming pauses). Only a herdr or auth failure that created nothing goes back to `queued`.
- **T8.3: slot details the plan left open.** A `reported` row frees its slot even with a live ledger row (Known boundary). An unreadable ledger holds the slot of a `launched`, `needs-you` or `failed` row, since nothing proves its worker ended. `dequeue` (T8.2) and the 30-day prune remove a `failed` row even while its ledger row is live, which frees that slot before `surface close`; the Slots rule's "until the operator closes it" holds only while the row stays in the queue.
- **T8.3: the needs-you reason lives in `last_error`** (`blocked: <screen>` or `idle without report`). A blocked needs-you row clears on any non-blocked verdict; an idle one clears only when the worker is working again (`not-ready`), since a `ready` verdict would flap it back and forth each tick. The idle clock is kept in memory and restarts with the drain.
- **T8.3: an invalid config keeps the last valid values for pacing and the idle rule;** claiming pauses. `drain start` refuses an invalid `[surface.drain]` (exit 2) before starting anything, and checks `drain.lock` before resolving herdr, so a second start is refused even with herdr down.
- **T8.3: drain worker branch is `worker/<name>`;** the drain never passes `--allow-path-binary`, so the claude binary must be named in `[launch]`.
- **T8.3: GitHub auth failure is recognized from `gh`'s stderr** (`HTTP 401`, `Bad credentials`, `gh auth login`, ...), read from `exec.CommandError.Stderr` because `Error()` redacts it. Marked `debt:` until `internal/githubauth` classifies `gh` failures.
- **T8.3: who enqueued or dequeued is not recorded.** Only the drain appends to `drain-events.jsonl` (under its lock); a CLI writer would need its own lock and a seq outside the drain's run. Left for P6 or a follow-up.
- **T8.3: extra event kinds and status fields:** `start`, `stop`, `unreadable`, `error`, and a `pruned` state; `drain.json` also records `herdr_path`, `started_at`, `interval_seconds` and the last tick's I/O `error`. `drain status` counts rows from the queue at the time it runs, not from the last tick. It also reports `held_slots` and `holding`, the rows holding a slot by `HoldsSlot` (review I5).
- **T8.3: `internal/procstart`** is the shared strict copy of `desk.processAlive`; `internal/desk` is unchanged.
- **T8.3 review (forgectl#1172): a `reported` or `failed` row closes with its worker.** The Loop row "its queue row then reads `closed`" covered only rows closed while `launched` or `needs-you`. `drain.Settle` now moves a `reported` or `failed` row to `closed` when its own ledger row is `closed`, removed, or replaced by another launch's, each tick, with no screen read. A row failed with nothing of its launch left (refused at re-check, `ErrNameTaken`, three empty attempts) drops its `launch_id` so `Settle` never reads someone else's row, or none, as its close.
- **T8.3 review: a launch configuration that cannot build a worker pauses claiming** (`ErrLaunchConfig`, `PauseLaunchConfig`). The build step runs after `git worktree add`, so a bad `[launch]` (worker posture, harness profile, a claude found only on `$PATH`) would fail every queued row in turn, each leaving a worktree. The build-step error is marked `errLaunchConfig` without changing its text; the row is still `failed` naming its worktree, and the pause lasts until the next `drain start`.
- **T8.3 review: smaller fixes.** A `ProbeGone` row's `last_error` ends "run surface close <name>". `drain status` text escapes its state fields. `worker.SameLaunch` is gone (`SameRead` replaced it). A `procstart.Of` failure at drain start is recorded as an `error` event, so a later `stop` refusal is explainable. The startup-only herdr pin is stated as such in `docs/herdr.md`.
- **T8.3 mutation sweep** (each a working-tree edit restored by `cp`): failed rows stop holding a slot → `TestHoldsSlot`, `TestPlanClaims`, `TestDrainTickCaps` red; claim without the `queued` check → `TestQueueClaimOneWinner`, `TestQueueClaimRefusesAnUnqueuedRow` red; retry a launch that created something → `TestDecideLaunch`, `TestDrainTickFailsAtOnce` red; drop the `ErrNameTaken` rule → the same two red; reconcile `pending`/`worktree` to `queued` → `TestReconcileTable`, `TestDrainKillAtEachLaunchStage` red; ignore `launch_id` when matching → only `TestMatchLedger` red (no tick-level test pins a mismatched id).
- **T8.3 review staged breaks** (working-tree edits restored by `cp`), each red: drop `launch_id` from `drainSpec` or from `workerSetup.steps` → `TestDrainLaunchWiring`; harness `""`, `allowPATH: true`, or no repo-top check → `TestDrainLaunchWiring`; notify ignoring the setting → `TestDrainNotifyOffSuppressesTheNotification`; `Settle` a no-op → `TestSettle`, `TestDrainSettlesClosedTerminalRows`; keep the launch id on `ErrNameTaken` → `TestDecideLaunch`, `TestDrainTickFailsAtOnce`; no launch-config class, wrapper or pause → `TestLaunchConfigErrorsClassify` or `TestDecideLaunch` and `TestDrainPausesOnLaunchConfig`; `Reconcile` noting every tick → `TestReconcileNotesUnreadableOnce`, `TestDrainReconcileUnreadableIsOneEvent`; no held-slot count → `TestDrainStatusShowsHeldSlots`; no close hint → `TestProbeGoneNamesTheWayOut`; adopt any launch id → `TestDrainDoesNotAdoptAnotherLaunch` (closing the tick-level gap above).
- **T8.4 notify: what was split out of `deskSignal`.** The macOS half is `sendMacSignal` (`internal/cli/signal.go`), which the desk and the drain both call over `internal/notify`. The pane half is the argv: `herdr.ReportBlockedArgs` and `herdr.ReleaseAgentArgs` take the pane as an explicit operand and a source (`forgectl-desk` or `forgectl-drain`). The desk keeps calling them through `herdr.PaneReportBlocked` on its own pane; the drain reaches them through new `herdradapter.Adapter.ReportBlocked`/`ReleaseBlocked`, which add the session pin and the `writablePane` ownership checks the desk's path has no part in.
- **T8.4 notify: one switch.** `[surface.drain] notify` gates both halves. The desk's `notify_macos`/`notify_herdr` are `[desk]` keys about the desk's own pane, so reusing them would tie two unrelated features together; a second drain key waits for someone to want only one half.
- **T8.4 notify: the drain releases the pane state when a row leaves `needs-you`.** The plan named only the entry. A `forgectl-drain` blocked state stays on the pane until released, so without a release the pane would read needs-you after the operator answered. The release (`drainNotifier.Cleared`) is not gated on `notify`, as the desk's clear is not; a gone workspace is nothing to release.
- **T8.4 notify: the drain uses its own herdr source, `forgectl-drain`,** so a drain release never clears a desk signal on the same pane.
- **T8.4 notify: an unverifiable pane is an `error` event too,** not a silent skip, in the same single event as any macOS failure (`needs-you notification: …`); a failed release is `clear the needs-you pane state: …`.
- **T8.4 staged breaks** (working-tree edits restored by `cp`), each red: `Adapter.ReportBlocked` targeting `os.Getenv("HERDR_PANE_ID")` instead of the ref's pane → `TestPaneState/reports_blocked_on_the_owned_root_pane`; releasing on any write out of `needs-you`-or-not (dropping the `q.State == needs-you` check) → `TestDrainClearsOnlyOnLeavingNeedsYou`.
- **T8.4 live check (sjomba, forgectl 0.34.0, herdr 0.9.3).** 3 tasks across forgectl and cadence-hooks. The per-repo cap held: the third task waited and launched in the same tick its repo's slot freed. A worker at a permission prompt read `needs-you` within about 15 s and went back to `launched` once answered. Reports checked against git: draft PRs forgectl#1185 and forgectl#1186; the cadence-hooks worker made no change, correctly, because cadence-hooks#1353 had already fixed cadence-hooks#1351, and it declined to unblock `gh pr create --web`. Stops per task: 1, 1, 0 (the allow list carried the third through test, commit, push and `gh pr create`). `surface close` moved each row to `closed`. Two bugs, fixed on `fix/drain-live-run`: drain workers read as `identity-mismatch` to a CLI without `HERDR_SESSION` (forgectl#1187), and a TMPDIR-less drain failed every row instead of pausing (forgectl#1188). Notifications were not delivered on this desktop for any channel (osascript exit 0, herdr `shown:true`); Cameron parked that as forgectl#1189.

- **T8.5 added to P2 (scope addition, operator decision relayed 2026-10-08):** retire ad-hoc `/tmp` launch scripts by making forgectl the launcher. Named worker profiles (`[surface.profiles]`, `--profile`), a per-worker `--model`, and a `claude-slots check 1` gate in the drain. This relaxes "What P2 does not do" on identity in one way only: a profile picks the worker's Claude account (its config dir), never its git or GitHub identity. Decisions: the profile's `config_dir` is applied after the env merge, so it outranks the inherited value and a launch profile's `env`; `enqueue` refuses an unknown profile name as well as the drain; a `--model` re-derives effort as a `--harness` override does; the slots check runs after the claim and re-check, and a held row's requeue writes no `state` events, so only the one `slots-held` event marks the hold; `claude-slots` is resolved in the `_drain` child at startup (same inherited `PATH` as `drain start`), not passed down as a flag; the queue keeps format version 1, since `profile` and `model` are `omitempty`.
- **T8.5 staged breaks** (working-tree edits restored by `cp`), each red: `enqueue` storing the resolved `config_dir` as the profile → `TestSurfaceEnqueueStoresTheProfileName`; the store dropping the profile name → `TestQueueEnqueueLaunchStoresTheProfileName`; the drain ignoring the resolved dir → `TestDrainResolvesTheProfileAtLaunch`; exit 1 not holding → `TestDecideSlots`, `TestDrainSlotsHoldIsQueuedAndQuiet`; an event every tick → `TestDrainSlotsHoldIsQueuedAndQuiet`, `TestDrainSlotsBrokenToolDoesNotBlock`; a held row left claimed → `TestDrainSlotsHoldIsQueuedAndQuiet`.
- **T8.5 review (forgectl#1195): the gate asks for N slots.** The drain runs `claude-slots check <N>`, N = 1 plus the launches this tick already made (any attempt that may have started a session), because a session launched moments ago may not be registered yet; `check 1` per row could overshoot the cap by up to `cap - 1`. Smaller fixes: a tick with nothing to claim ends the held condition, so a later hold records its own `slots-held` event; a check cut short by the drain stopping holds the row with no event (no "launching without the session cap"); a profile `config_dir` of the home directory or the root is refused at load and again after `~` expansion; `--profile` is refused before the worktree exists when the launch config, not only `--harness`, makes the worker codex. Staged breaks, each red: always `check 1` → `TestDrainSlotsAsksForEachLaunchThisTick`; no empty-plan reset → `TestDrainSlotsHoldEndsWithAnEmptyPlan`.

- **pi and codex drain workers (operator request, 2026-10-08).** The operator wants workers to be ordinary harness sessions ("full harnesses", ADR-0010 2026-10-08 amendment), named pi and codex as worker harnesses too, and asked for it kept simple. So the worker floor accepts pi (it has no posture flag to cap; it runs with pi's own config) and `--harness` accepts pi; `surface enqueue --harness claude|codex|pi` (default claude) stores the harness on the row, and the drain launches each row with it instead of pinning claude (T8.3). `--profile` stays claude-only; `--model` passes through as each harness's `--model`. `CheckClaimed` refuses a harness outside the three and a profile on a non-claude row. A `[harness.pi]` predicates table reads pi panes, from live Pi 1.0.4 captures on herdr 0.9.3. Two decisions beyond the request: a pi launch refuses a brief that starts with `@` (pi reads it as a file to attach, after `--` too), and the claude-slots gate applies to claude rows only. A row written before the field runs claude, and the queue keeps format version 1 (`harness` is `omitempty`). Staged break, red: the drain pinning claude again → `TestDrainLaunchesWithTheRowsHarness`.

## Learnings

- A reference's server source records how the session was chosen, not which server it is. The drain pins `HERDR_SESSION=default`, so its references said `herdr-named-session` and a plain CLI (`herdr-default-session`) refused them as a different server. The adapter now compares the session each side resolved: a default-session reference needs an adapter on the session named `default`, and a named-session reference, which does not record its name, is decided by the ServerID (it digests the session's socket path).

- An environment problem that fails every launch after `git worktree add` must pause, not fail row by row: three rows failed in about 30 s in the live run, each leaving a worktree and a `worker/<name>` branch. `surface.ErrRunDir` and `surface.ErrSocketPathTooLong` now classify as `ErrLaunchConfig`, and `drain start` makes one run directory before spawning the child and exits 2 naming `TMPDIR` when it cannot (an unset TMPDIR on macOS is `/tmp`, a symlink privdir refuses).

- T8.4 file-set security review (Opus, at 3cdc3fa5 = main with the drain plus the notifier): 0 Critical, 0 Important; the first supervised `drain start` on sjomba may proceed. Start it from a plain terminal, not a Claude Code session, so the drain does not inherit that session's environment (its own `gh` and herdr calls still see it; workers do not). Past the boundary: a worker can enqueue rows the drain then launches unattended. Fixed from its nits: the notifier gets only the row's own ledger row, so a release is never aimed at another launch's pane.

- T8.2 reviews, for T8.3: the drain re-checks each queue row it launches from (clean absolute repo, `CheckQueueBrief`, brief hash), since the store checks only version, fields and state on read; the 30-day prune needs a conditional remove (a row can be dequeued and re-enqueued between read and remove); the 768 KiB cap applies to `enqueue` only, so the drain's claim and failure writes are never refused by a queue the operator filled; `last_error` is capped at 1 KiB. `drain-events.jsonl` should record who enqueued and dequeued, which the queue row does not.

- `worker.AddWorktree` used to return an empty `Worktree` for a failure after `git worktree add` succeeded (path resolve, root check, `rev-parse HEAD`, a cancelled context). The attempt then read as having created nothing, so a drain would retry into a taken path and the ledger would lose the orphan. It now returns the path, and the failed row records it (T8.1 security review). T8.3: an `ErrNameTaken` attempt also reads as having created nothing, but the drain must mark the row `failed`, not retry.

- A launch that fails in setup after `git worktree add` (here, the `$PATH` binary refusal) leaves a `failed` ledger row with a worktree, and the same name is then refused. Seen live during T8.0; it is the case T8.3's retry rule handles by failing at once.

- `exec.CommandError.Error()` redacts stderr, so a classifier that matches error text (here, `gh`'s auth failure) never sees the words it looks for; the first test of the classifier went red for exactly that. Match `CommandError.Stderr` and never render it.

- A worker launch with a zero `backend.Ref` fails at `ref.MarshalJSON` before the ledger reaches `launched`, so a kill-at-stage test with a fake launch step must return a real ref (`testHerdrRef`) or its last stage is never reached; the test now asserts the kill call actually happened.
