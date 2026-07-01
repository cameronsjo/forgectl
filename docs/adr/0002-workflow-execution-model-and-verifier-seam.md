# 0002 — Workflow execution model: parse → resolve → verify → plan → execute, with a Verifier seam

- **Status:** Accepted (2026-07-01)
- **Context:** #9 workflow DSL design spike. Related: 0001, 0004; enables #10.

## Context

The executor must turn a workflow file into running commands while (a) staying testable through
the existing `exec.Runner`/`FakeRunner` seam, (b) supporting a side-effect-free `--dry-run`, (c)
exposing an integration point for signature verification (#10), and (d) handling both a
synchronous "wait + collect the review" path and a fire-and-forget "launch into a surface" path.

## Decision

A five-stage pipeline: **parse → resolve → verify → plan → execute.**

- **parse** — TOML → `Workflow`; `dsl_version` gate runs first (0004).
- **resolve** — merge CLI `--param` with defaults, enforce required params, interpolate `${}`.
- **verify** — call an injectable `Verifier.Verify(file)` before any planning. The spike ships a
  no-op `AllowAllVerifier`; #10 drops in an Ed25519/minisign implementation. The interface is
  **scheme-agnostic** — the spike does not pre-commit #10's signing scheme.
- **plan** — build `Plan []PlanStep` with all variables resolved. `--dry-run` stops here and
  prints the plan; zero commands run.
- **execute** — iterate `PlanStep`s; a `StepRegistry` maps `uses` → a `StepDef{Runner, Exports}`,
  co-declaring a verb's `StepRunner` and the variables it exports so the two can't drift apart;
  every runner receives one constructor-injected `exec.Runner`. Steps export variables into a
  shared `Context` so later steps compose on earlier output.

Execution supports **both** modes, default **synchronous**: a `launch` step with `mode = "sync"`
runs and captures output (`${review}`); `mode = "surface"` hands the session to `internal/tmux`
/cmux and detaches.

## Alternatives considered

- **Verify as a fixed scheme baked into the executor.** Rejected: couples #9 to a signing choice
  that is #10's to make, and makes the spike un-shippable until the scheme is settled. A no-op
  interface lets #9 land now and #10 slot in without touching the executor.
- **Synchronous-only execution.** Rejected: can't hand a claude session to a live tmux/cmux
  surface (the #6/#8 integration). **Fire-and-forget-only.** Rejected: breaks clean-room-review's
  "collect the review then teardown." Supporting both, default sync, covers both acceptance paths.
- **Verify after plan (or after execute).** Rejected: verification must gate *before* any resolved
  step is built, so a tampered file never reaches the planner.

## Consequences

- The executor is `FakeRunner`-testable exactly like `tmux`/`projects`: assert the composed argv
  sequence, assert `--dry-run` issues zero `Runner` calls.
- #10 integrates by supplying a real `Verifier` — no executor change.
- The shared `Context` + `${}` interpolation is new machinery the spike must implement and test.
