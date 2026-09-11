---
provenance:
  date: 2026-09-11
  session_id: c2d13fd6-30bc-409a-989c-cd5ad22073fd
  model: claude-fable-5-1
  harness: claude-code 2.1.269
  machine: cf6e768835c7
  recommended_model: opus
---

# Review-autonomy spine — design

The path from "a human starts every review" to "the backlog drains itself": #299 (durable session lifecycle), #472 (the admission cap on every launch path), #473 (the drainer). Spine of [`2026-09-05-forgectl-roadmap.md`](2026-09-05-forgectl-roadmap.md), tracker #474.

## Problem

`forgectl pr` review sessions are recorded by a breadcrumb JSON file under `config.PrSessionsDir()`. Today that record is written with `os.WriteFile` under an in-process mutex only (`internal/pr/breadcrumb.go:127-146`): no advisory lock across processes, no temp-and-rename, no fsync. A crash or a concurrent invocation between "clean room prepared" and "tmux window created" leaves a breadcrumb that says nothing about whether a window exists, and `pr list` reports it as active forever (`admission.go:88-95` names this exact gap).

The admission cap (`internal/pr/admission.go`) is consulted by `pr pick` (`internal/cli/pr_pick.go:175`) and by nothing else. `pr <ref>` and `pr local` launch unbounded, and the bypass is documented as deliberate (`pr_pick.go:42-43`). There is no queue: at the cap, `pr pick` defers the remainder and tells the human to come back later. And the count `Admit` reads is a live `tmux list-windows`, taken before a network-bound clone: two `pr pick` runs at one free slot both pass and both launch.

So a drainer built today would have to trust a record that cannot say what phase a session is in, would have no queue to drain, and would race every other launcher for the last slot.

## What #299's plan of record asked for, and what this design keeps

The Stage A v5 plan for #218 (nine mechanisms, 351 lines) is the plan of record #299 cites. The roadmap flags it as wanting a split. This design keeps the four mechanisms on the drainer's critical path and leaves the rest filed:

| v5 mechanism | Kept | Why |
|---|---|---|
| Advisory lock across record reads and writes | yes | the cap and the drainer both read-then-write session state; without cross-process exclusion two launchers take one slot |
| Durable phase record, fsynced before the mutation it authorizes | yes, six phases | the drainer must not discover session state is not crash-safe |
| Atomic record writer (temp, fsync, rename, dir fsync) | yes | a torn breadcrumb is a hand-edit today |
| `pr repair` | yes | the drainer's recovery verb; without it a `launching` record with no window blocks a slot forever |
| Quarantine move-graph persistence | no | quarantine already tears down on failure inside `sandboxAndQuarantine`; its crash window is not on the drainer's path |
| `PRSessionInstance` persistence, v2 identity codec | no | #218 landed the identity layer; the breadcrumb gains a `windowId` field in the spelling `newDispatch` already produces |
| Eight-attempt shell nonce for `pr open` | no | independent of the drainer; stays in #299 |
| Transaction subdirectories, per-record revision files | no | one record per session with a `revision` counter gives compare-and-write without a second directory |
| Bulk batch records | no | the queue is the set of `queued` breadcrumbs; FIFO by `createdAt` needs no batch record |

## Design

### Lifecycle lock

One file, `<sessionsDir>/.pr-session-lifecycle.lock`, taken with `flock(LOCK_EX)`. **The lock is held only around record reads and writes and the admission decision. It is never held across a clone, a `gh` call, or `tmux new-window`.** The phase record, not the lock, is what bridges those long operations.

- **Non-reentrant, by construction.** `flock` is per open file description, so a second acquisition from the same process blocks; Go's `sync.Mutex` is not reentrant either. Every verb that takes the lock is split into a locked shell and an unlocked core (`Teardown` / `teardownLocked`, `List` / `listLocked`, `transition` / `transitionLocked`), and composite verbs (`repair`, `drain`, `cleanup`) take the lock once and call the `*Locked` cores. The contract is stated in `lifecycle_unix.go` and a test drives `repair --rollback`, `drain --once`, and `cleanup` to completion without a timeout.
- **`sessionsMu` is retired.** The flock replaces it: every site that took `c.sessionsMu` takes the lifecycle lock instead, so there is one lock order, not two.
- **Bounded blocking wait.** `flock` has no timeout, so the acquisition polls `LOCK_EX|LOCK_NB` every 100ms up to `lockWait` (default 10s). The existing `usage_store_unix.go:41` pattern is non-blocking-drop and is **not** reused; this is new code.
- **The lock file body names the holder.** While held, the holder truncates and writes `pid=<n> verb=<pr|pick|drain|repair|teardown|cleanup> started=<rfc3339> host=<hostname>`. The timeout error reads that body: `lifecycle lock busy after 10s: <path> (held by pid 4821, verb=drain, since 03:02:11) — check with 'forgectl pr list'; the lock releases when that process exits`. A kernel close releases the lock, so a crashed holder never wedges the next process; stale body text is harmless because the message says "held by" from the file and the operator checks the pid.
- **Files.** `lifecycle_unix.go` and `lifecycle_other.go`, matching `internal/config/lock_unix.go` / `lock_other.go`. The `_other` build **refuses** every locked verb with a named error: a cap with no cross-process exclusion is not a cap.
- **Assumption on the record.** Two hosts sharing one `$HOME` (NFS, a synced volume) share the sessions dir, and `flock` over NFS is advisory at best. Out of scope; stated so it is not implied.

### Breadcrumb v2

```go
type Phase string

const (
    PhaseQueued      Phase = "queued"       // intent only; no workspace, no slot
    PhasePreparing   Phase = "preparing"    // slot reserved; clone in flight
    PhasePrepared    Phase = "prepared"     // workspace exists; not launched
    PhaseLaunching   Phase = "launching"    // fsynced before tmux new-window
    PhaseActive      Phase = "active"       // windowId set and validated
    PhaseNeedsRepair Phase = "needs-repair" // a transition could not prove its outcome
)

type Breadcrumb struct {
    // existing fields unchanged
    Version      int    `json:"version,omitempty"`      // 2 on every record this build writes; absent on legacy
    Phase        Phase  `json:"phase,omitempty"`        // required when Version == 2
    Revision     int    `json:"revision,omitempty"`     // required when Version == 2; starts at 1
    WindowID     string `json:"windowId,omitempty"`     // required when Phase == active
    RepairReason string `json:"repairReason,omitempty"` // required when Phase == needs-repair, forbidden otherwise
    Attempts     int    `json:"attempts,omitempty"`     // drain launch attempts on a queued record
    LastError    string `json:"lastError,omitempty"`    // termsafe-quoted; set with Attempts
    LastAttempt  time.Time `json:"lastAttemptAt,omitempty"`
}
```

- **`omitempty` is for legacy byte-identity only.** A v2 record always carries `version`, `phase`, and `revision`; the validator refuses a `version: 2` record missing any of them, so `phase: ""` cannot masquerade as absent.
- **Legacy (no `version`)** loads as before and is treated in memory as `active` with `WindowID` unknown. The v2 rules apply only when `Version == 2`, so "`active` requires `windowId`" and "legacy is active with no `windowId`" live in different layers. The only transitions a legacy record accepts are teardown and `repair --adopt-window`, which converts it to v2.
- **`windowId` is the exact string `newDispatch` produces** (`launch.go:35-41`): pid, server start, and `@N` joined by `tmux.FieldSep`. Validation splits on `tmux.FieldSep` into three non-empty parts with the third matching `^@[0-9]+$`. No second spelling.
- **`queued` and `preparing` records have no workspace.** `validateBreadcrumbRecord` allows an empty workspace for exactly those two phases; `classifyWorkspace` gains `workspaceAvailabilityNone` for them so `List` shows the row and `Teardown` can remove it.
- **Old binaries do not fail closed on `pr list`.** `decodeBreadcrumb` uses `DisallowUnknownFields` (`breadcrumb.go:195`), so a pre-upgrade binary refuses to *decode* a v2 record, but `List` skips undecodable records with a `slog.Warn` (`manage.go:41-46`) that a default install discards (`config.go:929-933`). A downgraded `forgectl pr list` prints fewer rows and exits 0. `pr teardown` on the same record refuses loudly. The new binary fixes the survey side: `List` returns its skip count, `pr list` prints `N record(s) could not be read — run forgectl pr repair` on stderr, and any arm that **counts** records (admission, drain) refuses on a nonzero skip count rather than proceeding on a short count. The downgrade behaviour is a release note in the Task 1 commit body and in `docs/commands/pr.md`.

### Atomic writer

`writeRecordAtomic(fs recordFS, dir, name string, data []byte, expectRevision int)`: open a private temp in the same directory with `O_CREAT|O_EXCL|O_NOFOLLOW` and mode 0600, full write with a short-write check, `Sync()`, `Close()`, revalidate the destination basename, `Rename`, then `Sync()` on the directory handle. This is new code; neither `internal/preflight/settings.go:180-209` nor `internal/env/write.go:34` does the directory sync, and that sync is the durability step the spine rests on.

Compare-and-write: `expectRevision == 0` means create-only (destination must not exist); `expectRevision > 0` means the destination must exist and decode to that revision, and **an absent destination is a mismatch, not a create**, so a record torn down by a peer cannot be resurrected. A mismatch refuses with `errRecordRevisionMismatch`; the user-facing text says another `forgectl pr` command changed this session while we were working and to retry.

`recordFS` is a `Client` field (`osRecordFS` by default, set through a constructor option): `OpenExclusive(path) (recordFile, error)`, `Rename(old, new)`, `SyncDir(path)`, `Remove(path)`, `Open(path)`, `Stat(path)`. Tests inject a fault-and-log fake that fails at any one step and records every call, so the crash matrix (open, write, sync, rename, dirsync, cleanup) and the ordering assertions in Task 2 are deterministic and in-process. Two cells stay outside the seam and are accepted gaps, stated here: a process killed while holding the flock (kernel-release path), and a real `new-window` followed by a failing transition (reachable with the fake tmux client plus a seeded rename failure, added in Task 2).

### Phases and transitions

```text
                 human: pr <ref>                       drain: pr drain
                       │                                     │
                       ▼                                     ▼
                  [admit+reserve]  ◀───────────────── queued ──(attempts ≥ 3)──▶ needs-repair
                       │                                                             ▲
                       ▼                                                             │
                   preparing ──clone──▶ prepared ──▶ launching ──new-window──▶ active│
                       │                   │              │                          │
                   teardown            teardown       crash ────▶ repair ────────────┘
```

- **Reservation is the admission.** Under one lock hold, the launcher reads live `pr-` windows (tmux truth, unchanged `LiveReviews`) plus records in `preparing`, `prepared`, or `launching` whose derived window name is not among the live windows, refuses if the sum is at the cap, and otherwise writes a `preparing` record. The clone runs outside the lock against an already-reserved slot. Every launch path does this: `pr <ref>`, `pr local`, `pr pick` (per prepared PR), and `drain`. A two-process test drives two launchers at one free slot and asserts exactly one reservation. `Admit` keeps its signature and gains the record term; `pr pick`'s existing call site is updated, not duplicated.
- **`prepared` is written into the same record** (`Prepare` and `PrepareLocal` gain a reuse-this-path option); the filename stays `breadcrumbFilename(ref, createdAt)` from the reservation.
- **`launching` is fsynced before `tmux new-window`; `active` with `windowId` after** the returned `WindowIdentity` validates. A crash between the two leaves `launching`, which `repair` resolves by looking the window up by its derived name. A `Launch` error before `new-window` transitions to `needs-repair` with reason `launch failed: <err>`.
- **`needs-repair` always carries a reason** written at the throw site: `rename after window created: <err>; window @7 exists`, `windowId failed validation: got <quoted>`, `launch failed: <err>`, `drain: 3 attempts, last: <err>`. Both crash producers (rename-after-window, identity validation) are constructed in tests.
- **`transition(ctx, path, from, to, mut)`** takes the lock, re-reads the record, refuses if the on-disk phase is not `from`, applies `mut`, increments `Revision`, and writes with `expectRevision` set to what it read. On a revision mismatch it re-reads and retries once; a second mismatch surfaces to the caller as a skip with the reason in the report.
- **Slots.** Occupied = live `pr-` windows + `preparing` + `prepared` + `launching` records whose window is not live. `queued` and `needs-repair` do not occupy: the first has reserved nothing, the second is visible to `pr repair` and is the operator's call.
- **Dedup.** `--queue`, `pr pick`'s queue write, and drain refuse a ref that already has a record in any phase other than `needs-repair`, naming the existing record path.
- **`pr pick`'s span** is two separate holds bridged by the record: one reservation per PR before `PrepareMany`, then `prepared → launching → active` per launch. Nothing holds the lock across `wg.Wait()`.

### `pr repair`

`forgectl pr repair` (no argument) inspects: one row per record in `preparing`, `prepared`, `launching`, or `needs-repair`, with phase, reason, whether the derived window is live, and whether the workspace exists; `no records need repair` when empty; `--json` supported. `pr list --json` gains a `phase` key and the human table appends `PHASE` as field 5 after the existing four, with the header `WINDOW` on the observed-liveness column so the recorded-versus-observed distinction is in the header.

`forgectl pr repair <breadcrumb> --apply` requires the argument and exactly one of:

- `--adopt-window` — **takes no operand.** An operator-supplied tmux target is the unqualified authority #218 removed and a `-t` grammar injection site, so adoption re-derives the window exactly as `resolveReviewWindow` does: exact name `ReviewWindowName(ref)` under the client's session native id (`launch.go:136-154`). It refuses when no such window exists, when the resolved window's session is not `c.tmuxSession` (window ids are server-global), and when the record's workspace does not classify LIVE, because writing `active` promotes the record to something `pr teardown` will `RemoveAll` and kill. It writes `active` with the generation-qualified id it resolved.
- `--rollback` — **refuses when the derived window is live, and refuses when the window list is unreadable** (unreadable is not absent; `admission.go:80-90` already states the rule). Then writes a durable intent row, tears down through `teardownLocked`, and only after teardown returns success removes the record and completes the intent row. A rollback that dies mid-way therefore leaves the workspace path recoverable from disk; a test injects a failure between the two and asserts that. On a TTY it prints what it will remove and asks; off a TTY it requires `--yes`. `--dry-run` prints the same lines and touches nothing.
- `--forget-if-absent` — proves no window and no workspace exist, then removes only the breadcrumb.

`--apply` with two modes refuses naming the set: `--apply takes exactly one of --adopt-window, --rollback, --forget-if-absent (got --rollback and --forget-if-absent)`. The `Long` text shows the four invocation lines.

**Intent and audit rows.** Every `--apply` appends one line to `<sessionsDir>/repair.jsonl` (0600; the `.jsonl` extension keeps it out of `List`'s `.json` enumeration at `manage.go:37`) **before** the mutation and completes it after: `ts`, `actor` (OS user, plus `$CLAUDE_CODE_SESSION_ID` when set), `ref`, `record_path`, `from_phase`, `mode`, `window_id`, `workspace`, `outcome`, `error`. `forgectl pr repair --history [--json]` reads it. The append is under the lifecycle lock, fsynced, never dropped on contention. The security seat's ruling: as an audit trail this is operability, because the dir is 0700 same-uid and a forger of breadcrumbs can forge rows; as the **write-ahead intent for `--rollback`** it is required, because it is the only pointer to a clean room once the record is gone.

### The cap on every launch path (#472)

`pr <ref>` and `pr local` reserve under the lock before `Prepare`. At the cap they refuse with exit 1:

```text
review cap reached (max 4, 4 running) — nothing prepared.
  see them:   forgectl pr list
  queue it:   forgectl pr <ref> --queue   (then: forgectl pr drain --once)
  raise it:   [pr] max_concurrent in config.toml
```

`pr <ref> --queue` writes a `queued` record and exits 0 with its path. `pr local` gets the cap and **no `--queue`**: `FindingsDir` is deliberately not persisted (`session.go:28-33`) and `Launch` refuses a reloaded local session (`launch.go:197-203`), so a queued local review could never launch. `pr pick` writes `queued` for both at-cap branches (`free == 0` at `pr_pick.go:180-183` and the truncated remainder at `:185-188`), prints `N PR(s) queued by the concurrency cap (max 4) — start them with 'forgectl pr drain --once'`, and exits 0 because it did do something. Its `Long` text and the runtime message at `:222` both change. The cap value stays `[pr].max_concurrent` (`config.go:457`); the config comment at `config.go:90` says it now governs every launch path.

### The drainer (#473)

- `forgectl pr queue` lists `queued` records FIFO by `createdAt`, `--json`; `no queued reviews` and `[]` when empty. `pr teardown` handles a `queued` record (removes the file, touches no workspace) and its `Short` becomes `Discard a review session or queue entry`.
- `forgectl pr drain` (`--once` default; `--watch --interval 60s`; `--dry-run`; `--json`). One pass: take the lock, count occupied, refuse the pass if `List` reported any unreadable record, then for each free slot claim the oldest `queued` record by transitioning it to `preparing` under the same hold. Release. Prepare and launch each claimed record through the same `Prepare` → `Launch` path a human's `pr <ref>` uses. A launch failure records `attempts`, `lastError`, `lastAttemptAt` on the record and returns it to `queued`; at three attempts it becomes `needs-repair` with the reason and stops consuming passes.
- **Output.** Every pass prints one line to stdout, because `log_level` is off by default and slog is discarded on a default install: `pass=3 free=2 queued=5 launching=1 launched=2 failed=0 next=60s`; the same object under `--json`; a `slog.Info` twin. Entry prints the interval and the cap. `--dry-run` prints `3 queued, 2 free — would launch owner/repo#41, owner/repo#42` and creates nothing. `--interval` without `--watch` is refused.
- **Exit.** `--once`: 0 when the queue is empty or every launch succeeded; 1 with the reason when the cap was unreadable, the lock timed out, records were unreadable, or any launch failed, and the report lists every item with `ref`, `record_path`, `from_phase`, `to_phase`, `outcome`, `error` so a partial pass is legible. `--watch`: a per-record failure logs and continues; a whole-pass refusal retries with backoff three times, then exits non-zero with the reason.
- **Resumability** comes from the phase record: a drainer killed mid-run leaves `preparing` or `launching` records, which occupy their slots until `repair` settles them, so the next run cannot double-launch.

## Sequencing

Four PRs, each mergeable alone:

1. **Substrate** — lock (unix + other), v2 fields and `Phase` type with the full validator, atomic writer with the fs seam, `List` skip-count surfacing, crash tests. No behavior a human sees changes except the new stderr line when a record cannot be read.
2. **Phases, reservation, `pr repair`** — closes #299's items 1, 2, 3, 6; the rest stay filed. Includes `teardown.go` and `cleanup` under the lock, the runbook in `docs/commands/pr.md`, and the `pr.go` help lists.
3. **The cap on `pr <ref>` and `pr local`, `--queue`, `queued` records end to end** — closes #472.
4. **`pr queue` and `pr drain`** — closes #473, with the drain triage section in `docs/commands/pr.md` and a committed `scripts/dogfood-drain.sh`.

Changelog: this repo's `CHANGELOG.md` is generated by release-please from conventional-commit subjects and has no `[Unreleased]` section, so the `feat(pr):` subjects are the entries; there is no hand-written changelog step.

## Out of scope, still filed

The v5 mechanisms marked "no" above stay under #299. #192 (notify) and #32 (poll daemon) follow the drainer per the roadmap.
