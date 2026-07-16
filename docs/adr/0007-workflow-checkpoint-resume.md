# 0007 — Workflow checkpoint/resume: run-state sidecar

- **Status:** Accepted (2026-07-15)
- **Context:** Related: 0001, 0002 (adds a stage to the pipeline's execute leg), 0003 (sandbox
  teardown is why exports are not rehydrated), 0006 (resume must not weaken blessing).

## Context

A workflow is an ordered step list (0001) resolved into a Plan and executed step by step (0002).
Until now a run was all-or-nothing: a five-step workflow that failed at step three re-ran steps
one and two on the next attempt. For a workflow whose early steps are slow or effectful (a clone,
a long build) that is wasteful and, for non-idempotent steps, wrong.

Resume must not weaken the blessing ceremony (0006): the adversary is the local agent, and a
resume path is a new place an agent-writable artifact could feed execution.

## Decision

**A per-workflow run-state sidecar records step completion; `--resume` replays the plan and skips
the leading run of already-completed, unchanged steps.**

- **Location:** `<config-dir>/workflows/.state/<name>.state.toml` (`config.WorkflowStateDir`),
  one file per workflow name — so "resume the last run" and "status of the last run" need no
  run-id enumeration. The leading-dot directory keeps state out of the `*.workflow.toml` glob.
- **Schema:** a `definition_hash` (sha256 of the workflow file's exact bytes) plus, per completed
  step, an `input_hash` (sha256 of the step's **plan-time** resolved fields — params baked in,
  prior-step exports still the literal `${name}`) and a completion timestamp.
- **Resume algorithm:** walk the plan from the start; skip each leading step whose checkpoint is
  present **and** whose `input_hash` still matches; execute from the first gap through the end. A
  step after a gap is never skipped — once an earlier step re-runs, everything downstream re-runs.
- **Durability (recovery-path invariant):** each checkpoint write is temp-file + fsync + atomic
  rename, so a crash leaves either the old state or the new one, never a truncated file and never
  a window with neither. Checkpoints are written only *after* a step succeeds; a fully successful
  run clears the sidecar.

### Blessing is re-verified, and definition drift refuses resume

A resumed run of a user workflow re-runs the exact same verify path as a fresh run (0006) — the
blessing is checked on every run, resume included. On top of that, resume compares the current
file's `definition_hash` against the checkpointed one and **refuses** on any mismatch: a changed
(or re-blessed) definition invalidates every checkpoint, so it must be run fresh. The two guards
are independent — an edited file fails the blessing *and* the definition-hash check.

### Exports are not persisted or rehydrated — resume refuses when it would need one

The hard question is a step that consumes an earlier step's export (`worktree` exports
`${workspace}`; a later `run` consumes it). If step 0 is skipped on resume, `${workspace}` is not
in the execution context. Two options were rejected:

1. **Rehydrate the export from the sidecar.** Rejected on security grounds: the sidecar is
   agent-writable and unsigned. A blessing signs the file's bytes, but a guarded field may
   legitimately reference an export (`run.cmd = "${workspace}/x"`). Feeding that export from an
   unsigned file would let an attacker-writable sidecar inject an attacker-chosen value into a
   blessing-guarded field on resume — exactly the ceremony-weakening 0006 forbids. It is also
   semantically broken: a failed run tears its sandbox down (0003), so the persisted path is gone.
2. **Silently fail at execute-time interpolation** (`unknown variable ${workspace}`). Rejected as
   a confusing error that reads like a bug.

**Chosen:** persist no exports, and at resume pre-flight refuse with a clear message when any step
to be executed references a `${export}` that only a skipped step produces (`MissingResumeExport`).
Resume is therefore sound for workflows of independent/idempotent steps (the common case — a
sequence of `run` steps) and honestly refuses the sandbox-composition case, pointing the user to a
fresh run.

## Consequences

- `forgectl workflow run <name> --resume` and `forgectl workflow status <name>` are new surfaces;
  plain `workflow run` is unchanged except that it now writes (and, on success, clears) checkpoints.
- Resume across an edited definition, or one that needs a checkpointed step's export, is refused —
  by design, not a limitation to fix later.
- A future export-composing resume would require authenticating the sidecar (a signed checkpoint),
  which is out of scope here.
