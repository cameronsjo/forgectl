# Architecture decision records

Records of significant design decisions made in forgectl development.

| Number | Title | Status | Date |
|--------|-------|--------|------|
| 0001 | [Workflow DSL is a TOML step list](0001-workflow-dsl-toml-step-list.md) | Accepted | 2026-07-02 |
| 0002 | [Workflow execution model: parse → resolve → verify → plan → execute, with a Verifier seam](0002-workflow-execution-model-and-verifier-seam.md) | Accepted | 2026-07-02 |
| 0003 | [Clean-room sandbox is a git worktree (local) / clone (remote) into a temp dir](0003-workflow-sandbox-worktree-into-temp.md) | Accepted | 2026-07-02 |
| 0004 | [Workflow DSL versioning is dual-axis: `dsl_version` (grammar) + `version` (workflow)](0004-workflow-dsl-dual-axis-versioning.md) | Accepted | 2026-07-02 |
| 0005 | [Module architecture: internal compile-time modules with two extension planes](0005-module-architecture.md) | Accepted | 2026-07-11 |
| 0006 | [Workflow blessing: user-presence signing, not author signing](0006-workflow-blessing-user-presence-signing.md) | Accepted | 2026-07-12 |
| 0007 | [Workflow checkpoint/resume: run-state sidecar](0007-workflow-checkpoint-resume.md) | Accepted | 2026-07-19 |
| 0008 | [Agent contract: every verb must be drivable without a TTY](0008-agent-contract.md) | Accepted | 2026-08-01 |
