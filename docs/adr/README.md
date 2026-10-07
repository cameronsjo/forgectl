# Architecture decision records

Records of significant design decisions made in forgectl development.

| Number | Title | Status | Date |
|--------|-------|--------|------|
| 0001 | [Workflow DSL is a TOML step list](0001-workflow-dsl-toml-step-list.md) | Accepted | 2026-07-01 |
| 0002 | [Workflow execution model: parse → resolve → verify → plan → execute, with a Verifier seam](0002-workflow-execution-model-and-verifier-seam.md) | Accepted | 2026-07-01 |
| 0003 | [Clean-room sandbox is a git worktree (local) / clone (remote) into a temp dir](0003-workflow-sandbox-worktree-into-temp.md) | Accepted | 2026-07-01 |
| 0004 | [Workflow DSL versioning is dual-axis: `dsl_version` (grammar) + `version` (workflow)](0004-workflow-dsl-dual-axis-versioning.md) | Accepted | 2026-07-01 |
| 0005 | [Module architecture: internal compile-time modules with two extension planes](0005-module-architecture.md) | Accepted | 2026-07-11 |
| 0006 | [Workflow blessing: user-presence signing, not author signing](0006-workflow-blessing-user-presence-signing.md) | Accepted | 2026-07-12 |
| 0007 | [Workflow checkpoint/resume: run-state sidecar](0007-workflow-checkpoint-resume.md) | Accepted | 2026-07-15 |
| 0008 | [Agent contract: every verb must be drivable without a TTY](0008-agent-contract.md) | Accepted | 2026-08-01 |
| 0009 | [Credentialed HTTP client posture: keychain-sourced, host-pinned, written to only by separate grant](0009-credentialed-http-client-posture.md) | Accepted | 2026-09-07 |
| 0010 | [Worker `auto` permission mode: per-machine opt-in behind a hardening floor](0010-worker-auto-permission-mode.md) | Accepted | 2026-10-05 |
| 0011 | [Worker PR merge policy: a gate check GitHub enforces, off by default](0011-worker-pr-merge-policy.md) | Accepted | 2026-10-05 |
| 0012 | [Desk threat model: defend against accidents, not against a same-uid process](0012-desk-threat-model.md) | Accepted | 2026-10-05 |
| 0013 | [Desk run sources and the run visualizer](0013-desk-run-sources-and-visualizer.md) | Proposed | 2026-10-06 |
| 0014 | [Log lenses: teaching the run view to read an app's log](0014-log-lenses.md) | Proposed | 2026-10-07 |
| 0015 | [One exit-code table for every verb](0015-exit-code-table.md) | Accepted | 2026-10-06 |
