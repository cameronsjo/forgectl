## Why

`BuildPlan` substituted params into each step, and `Executor.Run` then interpolated that output a second time to resolve exports. A `--param` value was therefore read as template text at run time. `cost ${` passed `--dry-run` and then failed as unterminated, `${foo}` failed as an unknown variable, and `${workspace}` silently became the sandbox path. The plan `--dry-run` printed was not the plan that ran.

## What Changes

- The plan keeps each step's template, as written, beside its resolved form. Execute resolves the template once against params plus exports, so a param value is never scanned for references.

## Impact

- `internal/workflow/plan.go` (`Plan`, `BuildPlan`), `internal/workflow/exec.go` (`Executor.Run`) and their tests.
- Checkpoint hashing and the bless guard are unchanged: both still read the plan-time step and the workflow file.
