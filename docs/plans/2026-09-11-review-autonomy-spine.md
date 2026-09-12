---
status: in-flight
next: "Task 2 is committed and pushed on feat/pr-lifecycle-phases; the orchestrator runs polish and opens its PR → Task 3 (fresh Sonnet subagent, branch feat/pr-admission-everywhere) → Task 4; merge this plan PR (#495) whenever"
branch: plan/review-autonomy-spine
pr: cameronsjo/forgectl#495
updated: 2026-09-11
approved_session_id: c2d13fd6-30bc-409a-989c-cd5ad22073fd
date: 2026-09-11
session_id: c2d13fd6-30bc-409a-989c-cd5ad22073fd
model: claude-fable-5-1
harness: claude-code 2.1.269
machine: cf6e768835c7
---

# Review-autonomy spine: #299 → #472 → #473

Design: [`2026-09-11-review-autonomy-spine-design.md`](2026-09-11-review-autonomy-spine-design.md). Roadmap: [`2026-09-05-forgectl-roadmap.md`](2026-09-05-forgectl-roadmap.md). Tracker: #474.

## Goal

A backlog of review sessions drains itself under the concurrency cap, and a crash at any point leaves a record `pr repair` can settle rather than a breadcrumb that lies.

## Alternatives declined

- **Full Stage A v5 program (nine mechanisms)** — quarantine move-graph persistence, the identity codec, shell nonces, and transaction subdirectories are not on the path from "human starts every review" to "drainer starts them"; the roadmap already flagged #299 for splitting.
- **Drainer-first, no substrate** — fastest to a demo, but a drainer killed mid-run double-launches on today's non-atomic, phase-less breadcrumb; that is the failure the roadmap names as worse than no drainer.
- **Substrate and phases in one PR** — declined for review size; the writer and lock are testable without any phase semantics, and a reviewer can hold the crash matrix in their head for one PR but not both.

## Panel

Panel: plan-reviewer (conflict lens), plan-reviewer (buildability lens), red-team-reviewer, operability-reviewer, user-experience-reviewer, security-posture-reviewer (routed round, 2 lines) ran — 68 findings, 66 folded in, 2 declined (see Panel review — findings declined)

## Panel review — findings declined

- **[red-team CRITICAL-3 / fix part 2] `pr list` exits nonzero when a record cannot be read** — declined. `pr list` is a survey verb that scripts pipe; one torn file exiting the whole listing 1 breaks every reader. Folded instead as: `pr list` prints the `N record(s) could not be read` line on stderr and exits 0, and every arm that **counts** records (admission, drain) refuses on a nonzero skip count. The refusal lands where the count is acted on.
- **[operability advisory] Task 4's dogfood as an `Isolated` real-tmux test** — declined. `Prepare` needs a live `gh pr view` and a real clone, which the hermetic `Isolated` suite cannot supply. Folded as a committed `scripts/dogfood-drain.sh` instead, so the procedure survives the session.

## Architecture

`internal/pr/` gains `lifecycle_unix.go` + `lifecycle_other.go` (lock, holder body, timeout), `record.go` (atomic writer, `recordFS` seam), `phase.go` (`Phase` type, transitions), `repair.go`, `drain.go`, `repairlog.go` (intent/audit rows). `breadcrumb.go` keeps the single decoder and validator and gains the v2 fields; `workspace_state.go` gains `workspaceAvailabilityNone`; `manage.go`, `teardown.go`, `session.go`, `local.go`, `launch.go`, `admission.go`, `discover.go` are modified. `internal/cli/` gains `pr_repair.go`, `pr_queue.go`, `pr_drain.go`; `pr.go`, `pr_local.go`, `pr_pick.go` are modified. `internal/config/config.go` gets one comment change. Docs: `docs/commands/pr.md`, `README.md`. Scripts: `scripts/verify-v2-list-surfaces-unreadable.sh`, `scripts/dogfood-drain.sh`.

## Tech Stack

- Go 1.26 (go.mod), cobra, `golang.org/x/sys/unix` for `Flock` (already a dependency: `go.mod:25`, used by `internal/launch` and `internal/config`)
- Tests: `go test ./...`, `go vet ./...`, `golangci-lint run` per `.golangci.yml`; real-tmux tests gated by the existing capability check and run as `go test -run Isolated ./internal/tmux`
- CI: `.github/workflows/ci.yml` (`go test ./...` on macOS and Linux)
- Changelog: `CHANGELOG.md` is release-please generated from conventional-commit subjects; it has no `[Unreleased]` section and is not hand-edited. Each task's `feat(pr):` subject is its entry.

## Global Constraints

- ADR-0008: every new verb takes `--json`, works with no TTY, and exits with an honest code; no interactive prompt off a TTY (a destructive verb requires `--yes` there). Field order of existing human output is append-only; JSON change is additive.
- Breadcrumbs are hostile input on the way back in (`breadcrumb.go:27-33`): every new field is validated in `validateBreadcrumbRecord`, and no persisted value is ever authority for a tmux action without live revalidation by derived name under the client's session.
- A refusal asserts zero mutation: no window created, no record rewritten, no workspace touched, no lock body written.
- The lifecycle lock is non-reentrant and is never held across a clone, a `gh` call, or `tmux new-window`. Every locked verb has an unlocked `*Locked` core; composites call the core. `c.sessionsMu` is retired in Task 1.
- Any arm that counts records (admission, drain) refuses on a nonzero unreadable-record count.
- Every failure path has a `slog` line naming the broken expectation, and every user-facing refusal names the next command.
- No new runtime dependency. `debt:` markers carry a ceiling and a trigger; `TODO` needs an issue number.
- Every task updates the three hand-maintained verb lists it touches: `internal/cli/pr.go` `Long` (lines 65-75), `prKeysText` (`pr.go:411-431`), and `README.md`'s `pr` section (lines 94-107).

## Orchestrator

**Driver:** fable — crash-safety design with a fault-injection matrix; the session model drives Task 1 in-context and dispatches later tasks from the committed plan.

---

## Tasks

### Task 1 — Substrate: lifecycle lock, atomic writer, breadcrumb v2, list surfaces unreadable records

**Files:**
- Create: `internal/pr/lifecycle_unix.go`, `internal/pr/lifecycle_other.go`, `internal/pr/lifecycle_test.go`, `internal/pr/record.go`, `internal/pr/record_test.go`, `internal/pr/phase.go` (type + constants + `validPhase`), `scripts/verify-v2-list-surfaces-unreadable.sh`
- Modify: `internal/pr/breadcrumb.go` (v2 fields; `validateBreadcrumbRecord` v2 rules; `writeBreadcrumb` through the writer under the lock; `sessionsMu` retired), `internal/pr/breadcrumb_test.go`, `internal/pr/manage.go` (`List` returns `(summaries, unreadable int, err)`; `listLocked` core), `internal/pr/teardown.go` (`sameBreadcrumbRecord` compares the v2 fields; `Teardown`/`teardownLocked` split; `Cleanup` takes the lock once), `internal/pr/workspace_state.go` (`workspaceAvailabilityNone` for `queued`/`preparing`), `internal/cli/pr.go` (`pr list` prints the unreadable line to stderr; `--json` row gains `phase`; human table appends `PHASE` as field 5; test pins field 3 as the breadcrumb path), `docs/commands/pr.md` (downgrade note)

**Interfaces:**
- Produces: `type Phase string` and the six constants; `func (c *Client) withLifecycleLock(ctx context.Context, verb string, fn func() error) error` (non-reentrant; polls `LOCK_EX|LOCK_NB` every 100ms up to `c.lockWait`, default 10s; writes the holder body; timeout error carries the holder body and the next step); `type recordFS interface { OpenExclusive(path string) (recordFile, error); Rename(old, new string) error; SyncDir(path string) error; Remove(path string) error; Open(path string) (fs.File, error); Stat(path string) (fs.FileInfo, error) }` as a `Client` field with `osRecordFS` default and a `WithRecordFS` constructor option; `func writeRecordAtomic(fs recordFS, dir, name string, data []byte, expectRevision int) error` (`0` = create-only, `>0` = must exist at that revision, absent destination is a mismatch; `errRecordRevisionMismatch`); `Breadcrumb.Version/Phase/Revision/WindowID/RepairReason/Attempts/LastError/LastAttempt` with the validator rules in the design § Breadcrumb v2 (a v2 record requires `version`, `phase`, `revision`; `active` requires `windowId` in the `newDispatch` spelling split on `tmux.FieldSep`; `needs-repair` requires `repairReason`, every other phase forbids it; `queued` and `preparing` allow an empty workspace, every other phase requires an absolute one); legacy records (no `version`) load unchanged and skip v2 rules

**Dispatch:** In-context (this session) · branch `feat/pr-lifecycle-substrate` off `origin/main`

**Report:** —

**Steps:**
- [x] Failing tests: lock excludes a second holder (two `Client`s on one dir, separate fds); a held lock times out with a message carrying the holder body and the lock path; a killed-holder fd released by close lets the next acquire (in-process: close the fd, acquire); `withLifecycleLock` nested from inside `fn` fails the test by timing out (documents non-reentrancy); writer leaves no temp on each injected failure (open, write short, sync, rename, dirsync); writer calls `SyncDir` on the success path (assert via the fake's call log); writer refuses on revision mismatch and on absent-destination-with-expectRevision>0; v2 validation rejects a missing `phase`/`revision`, an unknown phase, `active` without `windowId`, a `windowId` not in the `FieldSep` spelling, `needs-repair` without a reason, a reason on any other phase, an empty workspace on `prepared`; `queued` with an empty workspace loads and lists; legacy record still loads and lists; `List` reports one unreadable record for a file with an unknown key and still returns the readable rows; `pr list` prints the stderr line and exits 0; `pr list --json` rows carry `phase`; `pr list` human field 3 is still the breadcrumb path
- [x] Run — expect RED
- [x] Implement `lifecycle_unix.go` / `lifecycle_other.go` (other: refuse with a named error), `record.go`, `phase.go`; route `writeBreadcrumb` through the writer under the lock; retire `sessionsMu`; split `Teardown`/`teardownLocked`, `List`/`listLocked`; `Cleanup` takes the lock once and calls `teardownLocked`; extend `sameBreadcrumbRecord`; add `workspaceAvailabilityNone`
- [x] Run — expect GREEN; `go vet ./...`; `golangci-lint run`
- [x] Write `scripts/verify-v2-list-surfaces-unreadable.sh`: `HOME=<mktemp -d>` (the real lever — `configDir()` is `os.UserConfigDir()/forgectl`, `config.go:1032-1038`; there is no `FORGECTL_CONFIG_DIR`, and `HOME` also moves gh/git config, which the script states), write one breadcrumb with an unknown key into `$HOME/Library/Application Support/forgectl/pr-sessions/` (Linux: `$HOME/.config/forgectl/pr-sessions/`), run the freshly built `forgectl pr list`, assert stderr contains `1 record(s) could not be read` and exit is 0; exit non-zero otherwise. Run it and record the measured output inline in the PR body — measured: `VERDICT: PASS pr list exits 0 and names the record it could not read`
- [x] `docs/commands/pr.md`: a note that an older forgectl hides v2 records from `pr list` silently and refuses them loudly on `pr teardown`; upgrade rather than downgrade; there is no migration back. Same text in the commit body as the release note
- [x] Commit: `feat(pr): lifecycle lock, atomic breadcrumb writer, and v2 record fields (#299)` with the producer tuple — `d147641`
- [x] run `cadence-forge:polish` (diff-based arms from the worktree; `cadence-forge:security-reviewer` on the diff since it touches the hostile-input validator); fold findings; open PR — security clean + 3 nits, code review 2 Important + nits, all folded in `a9a4f27`

### Task 2 — Phases, reservation, and `pr repair`

**Files:**
- Create: `internal/cli/pr_repair.go`, `internal/cli/pr_repair_test.go`, `internal/pr/repair.go`, `internal/pr/repair_test.go`, `internal/pr/repairlog.go`, `internal/pr/repairlog_test.go`
- Modify: `internal/pr/phase.go` (`transition`), `internal/pr/admission.go` (`Admit` adds the record term; `reserve`), `internal/pr/session.go` and `internal/pr/local.go` (reserve → `preparing` before the clone; `prepared` written into the same record via `PrepareOpts.RecordPath`), `internal/pr/launch.go` (`launching` before `new-window`, `active` + `windowId` after; `needs-repair` with reason on a pre-window failure), `internal/pr/discover.go` (`PrepareMany` reserves per PR under one hold before the goroutines), `internal/cli/pr.go` (register `repair`; `Long`; `prKeysText`), `internal/cli/pr_pick.go` (uses the reservation; no other behaviour change here), `docs/commands/pr.md` (phase table; repair runbook: evidence `pr repair --json`, `tmux list-windows -a`, workspace `git status`; default `--rollback` when no window and no findings file; replaces the stale teardown recipe at line 73), `README.md` (`repair` in the verb list)

**Interfaces:**
- Consumes: Task 1's lock, writer, `Phase`, `recordFS`
- Produces: `func (c *Client) transition(ctx context.Context, path string, from, to Phase, mut func(*Breadcrumb) error) error` (takes the lock; re-reads; refuses if on-disk phase ≠ `from`; increments `Revision`; writes with the read revision; on mismatch re-reads and retries once, second mismatch returns `errRecordRevisionMismatch` for the caller to report as a skip); `func (c *Client) reserve(ctx context.Context, ref Ref, cfgMax int, opts PrepareOpts) (path string, err error)` (one lock hold: occupied = live `pr-` windows + `preparing`/`prepared`/`launching` records whose derived window is not live; refuses at the cap with `errReviewCapReached{Max, Live}`; refuses on any unreadable record; refuses a ref with an existing record in any phase but `needs-repair`, naming it; writes the `preparing` record); `func (c *Client) Admit(ctx, cfgMax int) (max, live, free int, ok bool)` keeps its signature and counts the record term; `RepairReport{Items []RepairItem}` with `RepairItem{Ref, RecordPath, FromPhase, ToPhase, WindowLive, WorkspaceExists, Outcome, Error}`; `func (c *Client) Repair(ctx, opts RepairOpts) (RepairReport, error)`; `repairlog.Append(intent) / Complete(outcome)` on `<sessionsDir>/repair.jsonl`

**Dispatch:** Serial (after Task 1 merges) · fresh Opus subagent — the transition ordering is the crash-safety property; branch `feat/pr-lifecycle-phases`

**Report:** `/tmp/forgectl-spine/reports/task-2.md`

**Steps:**
- [x] Failing tests: `launching` is on disk before the fake tmux `new-window` is called (fake `recordFS` call log ordered against the fake tmux call log); `active` carries the `newDispatch` string; a `new-window` error leaves `needs-repair` with reason `launch failed: …`; a rename failure after a successful `new-window` (fake tmux returns a valid identity, seeded rename fault) leaves `needs-repair` with reason naming the window; an invalid returned identity leaves `needs-repair` with the validation reason; two `reserve` calls at one free slot from two `Client`s yield one `preparing` record and one `errReviewCapReached`; `reserve` refuses when `List` reports an unreadable record; `reserve` refuses a ref that already has a `queued` record and names its path; `transition` refuses a wrong `from`; `transition` retries once on a mismatch and returns the error on the second; a legacy record refuses every transition except teardown and adopt; `PrepareMany` holds the lock once for N reservations and not across `wg.Wait()` (assert via lock call log); `repair` inspect lists `preparing`/`prepared`/`launching`/`needs-repair` and prints `no records need repair` when empty; `--json` shape; `--apply` without a breadcrumb argument refuses naming the inspect command; `--apply` with two modes refuses naming the set and the two given; `--adopt-window` refuses when no window carries the derived name, when the window with that name is in another session, and when the workspace is not LIVE, each with zero tmux mutation; `--adopt-window` on a legacy record converts it to v2 `active`; `--rollback` refuses on a live window and on an unreadable window list; `--rollback` off a TTY without `--yes` refuses; `--rollback --dry-run` prints and touches nothing; `--rollback` writes the intent row before teardown and completes it after (call-log order), and an injected failure between them leaves the workspace path readable from `repair.jsonl`; `--forget-if-absent` refuses when a window or workspace exists; `repair --history --json` returns the rows; `repair`, `drain`-shaped composites, and `cleanup` complete without a lock timeout (non-reentrancy test)
- [x] Run — expect RED
- [x] Implement; the four invocation lines in `pr repair`'s `Long`; `pr list` human column header `WINDOW` for observed liveness and one sentence in `pr list --help` distinguishing recorded phase from observed window
- [x] Run — expect GREEN; vet; lint; `go test -run Isolated ./internal/tmux`
- [x] `docs/commands/pr.md` phase table + repair runbook; `README.md` and `pr.go` verb lists
- [x] Commit: `feat(pr): durable launch phases, slot reservation, and pr repair (#299)` with the producer tuple; the body names which #299 items landed (1, 2, 3, 6) and that the rest stay open — no closing keyword
- [ ] run `cadence-forge:polish` (diff-based arms; security-reviewer on the diff: it adds a destructive verb); fold findings; open PR

### Task 3 — The cap on every launch path, `--queue`, `queued` records end to end

**Files:**
- Modify: `internal/cli/pr.go` (reserve before `Prepare`; the three-line cap refusal, exit 1; `--queue`; `sessionStatus` renders `queued` ahead of the missing-workspace branch; `Long`; `prKeysText`; `teardown` `Short` becomes `Discard a review session or queue entry`), `internal/cli/pr_local.go` (reserve before `PrepareLocal`; same refusal; no `--queue`, with the reason in `Long`), `internal/cli/pr_pick.go` (both at-cap branches write `queued`; new `Long` sentence `PRs past the cap are queued for 'forgectl pr drain', not discarded.`; runtime line `N PR(s) queued by the concurrency cap (max 4) — start them with 'forgectl pr drain --once'`; exit 0), `internal/pr/session.go` (`Queue`), `internal/pr/teardown.go` (a `queued` record: remove the file, touch no workspace), `internal/config/config.go` (comment at line 90: the cap governs every launch path), `README.md`, `docs/commands/pr.md`
- Test: `internal/cli/pr_test.go`, `internal/cli/pr_local_test.go`, `internal/cli/pr_pick_test.go`, `internal/pr/session_test.go`, `internal/pr/teardown_test.go`

**Interfaces:**
- Consumes: Task 2's `reserve`, `transition`, `errReviewCapReached`
- Produces: `func (c *Client) Queue(ctx context.Context, ref Ref, opts PrepareOpts) (path string, err error)` — under the lock, dedup against every phase but `needs-repair`, write a `queued` v2 record with no workspace, persisting `Agent` and `Provenance` (never `FindingsDir`); refuses a local ref

**Dispatch:** Serial (after Task 2) · fresh Sonnet subagent; branch `feat/pr-admission-everywhere`

**Report:** `<reports-dir>/task-3.md`

**Steps:**
- [ ] Failing tests: `pr <ref>` at the cap exits 1 with the three-line message naming max and live; the same path below the cap launches (positive control, asserts one `preparing` then `active` record); `pr <ref> --queue` at the cap writes a `queued` record, creates no workspace, exits 0 printing the path; `--queue` twice on one ref refuses naming the first record; `pr local` at the cap refuses and does not accept `--queue`; `pr pick` at `free == 0` writes N `queued` records and prints the queued line with exit 0; `pr pick` with a truncated remainder writes that remainder `queued`; `pr list` shows a `queued` row with status `queued`, not `workspace missing`; `pr teardown` on a `queued` record removes the file and calls no sandbox teardown; `pr pick`'s `Long` no longer contains "bypasses admission"
- [ ] Run — expect RED
- [ ] Implement
- [ ] Run — expect GREEN; vet; lint
- [ ] Commit: `feat(pr): admission cap on every launch path, --queue defers to the drainer (#472)` with the producer tuple
- [ ] run `cadence-forge:polish`; fold findings; open PR with plain-text `Closes #472`

### Task 4 — `pr queue` and `pr drain`

**Files:**
- Create: `internal/cli/pr_queue.go`, `internal/cli/pr_drain.go`, `internal/pr/drain.go`, tests for each, `scripts/dogfood-drain.sh`
- Modify: `internal/cli/pr.go` (register; `Long`; `prKeysText`), `README.md` (`queue`, `drain`, and the `log_level`/`log_file` note for a watcher), `docs/commands/pr.md` (drain triage checklist in order: cap readable, lock held by whom, `preparing`/`launching` occupancy, unreadable records, queue depth)

**Interfaces:**
- Consumes: Task 3's `Queue`, Task 2's `reserve` and `transition`, Task 1's lock and `List`
- Produces: `DrainOpts{Once bool, Watch bool, Interval time.Duration, DryRun bool, MaxAttempts int}` (default 3); `DrainReport{Pass int, Free, Queued, Launching, Launched, Failed int, Items []DrainItem, Refusal string}` with `DrainItem{Ref, RecordPath, FromPhase, ToPhase, Outcome, Error}`; `func (c *Client) Drain(ctx context.Context, cfg config.Config, opts DrainOpts) (DrainReport, error)`

**Dispatch:** Serial (after Task 3) · fresh Sonnet subagent; branch `feat/pr-drain`

**Report:** `<reports-dir>/task-4.md`

**Steps:**
- [ ] Failing tests: drain with `free=2` and three queued claims the two oldest to `preparing` under one lock hold (lock call log shows one hold for the claim, none across the fake clone) and launches them; a `launching` record consumes a slot; a `preparing` record consumes a slot; an unreadable cap refuses with exit 1 and launches nothing; an unreadable record refuses the pass; a launch failure returns the record to `queued` with `attempts=1` and `lastError` set; a third failure moves it to `needs-repair` with reason `drain: 3 attempts, last: …` and the next pass skips it; two `Drain` calls from two `Client`s against one `queued` record launch exactly once; a mixed pass (one success, one failure) exits 1 with both items present and the succeeded item `active`; `--once` exits after one pass; `--watch` loops until ctx cancel and prints one pass line per pass on stdout even with the slog handler discarded; `--watch` retries a whole-pass refusal three times with backoff then exits non-zero; `--interval` without `--watch` refuses; `--dry-run` prints the would-launch refs and creates nothing; `pr queue --json` shape and `[]` when empty; `pr queue` prints `no queued reviews` when empty; `pr drain` on an empty queue exits 0 with `nothing queued`; `pr drain --json` emits the report object
- [ ] Run — expect RED
- [ ] Implement
- [ ] Run — expect GREEN; vet; lint
- [ ] Write `scripts/dogfood-drain.sh`: queue two named PRs with `--queue`, run `pr drain --once --json`, assert two `launched` items, print the report; run it against two real PRs and record the measured output inline in the PR body
- [ ] `docs/commands/pr.md` triage section; `README.md`; `pr.go` lists
- [ ] Commit: `feat(pr): queue and drain verbs (#473)` with the producer tuple
- [ ] run `cadence-forge:polish`; fold findings; open PR with plain-text `Closes #473`

---

## Deviations

- **2026-09-11 — Task 1: the writer does not validate the record it writes.** The plan implied the atomic writer validates before writing. The loader is the boundary (a breadcrumb is hostile input on the way back in), and the existing tests that prove that boundary seed forged records through the package-level writer, so validating on the way out would make those tests unable to stage the very file they exist to reject. Reality-forced; validation stays at load.
- **2026-09-11 — Task 1: `List` takes a context and the sessions-dir check is writable-only.** Both from the polish pass. `List(ctx)` so `pr list` is cancellable while waiting on the lock; the mode assertion refuses group/other-**writable** (`0o022`), not accessible (`0o077`), because a stricter check refused every `t.TempDir` and the threat is a writer. Chosen improvement.
- **2026-09-11 — Task 1: `sameBreadcrumbRecord` also compares `Provenance`.** Not in the Task 1 file list; strictly more conservative for the stale-unlink identity check. Chosen improvement, declared in the commit body.

- **2026-09-11 — Task 2: `pr list` gains no header ROW; the column names live in `pr list --help`.** The step asked for a `WINDOW` header on the observed-liveness column. `pr list` has never printed a header, so adding one would put a non-row on line 1 of output the plan's own constraint calls append-only, breaking every caller that reads line 1 as a record. The naming and the recorded-versus-observed sentence went into a new `Long` for `pr list` (`REF CREATED PATH WINDOW PHASE`) and into `docs/commands/pr.md`. Reality-forced.
- **2026-09-11 — Task 2: `Prepare` and `PrepareLocal` write a v2 `prepared` record even when nothing reserved.** The Files list only asked them to complete a reservation. Leaving the unreserved path writing legacy records would mean `pr <ref>` (which Task 3 reserves, not Task 2) produced phase-less records that `Launch` could not bracket — so the crash-safety property would be absent on the most common launch path until Task 3. Two Task-1 `pr list` assertions that pinned field 5 as `-` now pin `prepared`; the legacy `-` rendering stays covered by the seeded pre-phase records in `pr_dash_test.go`. Chosen improvement.
- **2026-09-11 — Task 2: admission now reads `tmux list-windows` TWICE per `pr pick`, not once.** `Admit` sizes the batch and the reservation claims the slots, and only the second is race-free — that separation is the design's point, but four cost-contract tests in `internal/cli` pinned the old single read. Their counts moved from 1→2 and 2→3 with the reason recorded in each. Occupancy itself was collapsed onto ONE `ListWindows` call (`reviewWindowSnapshot`) rather than the two a naive `LiveReviews` + `WindowsLive` pair would cost, so the increase is one fork per invocation, not per ref.
- **2026-09-11 — Task 2: the "invalid returned identity" test splits into two.** `internal/tmux` refuses a malformed `new-window` reply at its own boundary, so a forged identity never reaches `completeLaunch`'s `validWindowID` gate through `Launch`. One test pins the reachable behaviour (the record still lands in `needs-repair` naming the identity failure); a second drives `completeLaunch` directly so the inner gate is not mistaken for dead code. Reality-forced.
- **2026-09-11 — Task 2: `teardown.go` gained `discardRecordOnly`, which the Task 2 file list does not name.** `repair --rollback` and `--forget-if-absent` both have to settle a `preparing` record with no workspace, and `classifyWorkspace("")` is Invalid, so `teardownLocked` refused them. The new branch runs the same pinned-handle identity protocol as `discardStale` minus the workspace re-classification, and re-proves the record still carries no workspace immediately before the unlink. Reality-forced; Task 3's `pr teardown` on a `queued` record now has its core already built.
- **2026-09-11 — Task 2: `RepairItem` carries a `Reason` field the Interfaces list omits.** The design's inspect output requires the `needs-repair` reason per row and the listed field set had nowhere to put it; folding it into `Error` would conflate "why the session broke" with "why this repair failed". Additive.
- **2026-09-11 — Task 2: `Client` gained an unexported `onLock` hook.** "`PrepareMany` holds the lock once … assert via lock call log" needs a lock call log, and there was none. It is unexported and set only from in-package tests, so no production surface changes. Chosen improvement.

## Learnings

- **A legacy record's phase renders as `-`, not `active`.** The design says legacy is *treated* as active for slot counting; the presentation layer shows the record said nothing. The two live in different layers on purpose (Task 2's admission reads the design rule; `pr list` reads the record).
- **`t.TempDir` leaves are `0777` under umask.** Any test-facing check on directory mode must be about writability by others, or every Go test dir fails it, and a lock test that waits on a goroutine that already returned hangs to the package timeout rather than failing.
- **The lifecycle lock file is counted by anything that counts directory entries.** `pr_pick_integration_test.go` asserted "0 breadcrumbs after a refusal" from `len(os.ReadDir(sessionsDir))`, and went red the moment `Admit` started taking the lock — reporting a leaked breadcrumb where the only new entry was `.pr-session-lifecycle.lock`. Any future check about session-dir contents has to filter on `.json`.
- **A transition validates on the way OUT; the breadcrumb writer deliberately does not.** The two rules look contradictory and are not. `writeBreadcrumbFS` stays unvalidated so tests can stage forged records for the loader to reject (Task 1's deviation); `transitionOnce` composes its record from one this build already accepted plus a mutation it wrote itself, so a rejection there is a caller bug that must never reach disk.
- **`--adopt-window` needs no "window is in another session" check of its own.** `ResolveWindowExact` already scopes by the parent session's native id, so a same-named window under a different session simply does not resolve. The test for that case pins the property rather than a branch — which is the right shape, since the day someone loosens the resolver is the day the test should go red.
- **A record parked in `needs-repair` does NOT block a fresh launch of the same ref.** `recordForRef` skips it on purpose: refusing would make a crashed session permanently un-relaunchable until a human ran `pr repair`, which is the opposite of what the phase is for. It also does not occupy a slot, so the two rules agree.
