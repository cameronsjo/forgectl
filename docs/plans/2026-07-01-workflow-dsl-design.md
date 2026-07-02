# forgectl `workflow` — DSL design spike (2026-07-01)

> Design artifact for the roadmap's spine head (#9). Doubles as the implementation spec for the
> walking skeleton. Companion ADRs: `docs/adr/0001`–`0004`. Roadmap:
> `docs/plans/2026-07-01-forgectl-forge-roadmap.md`.

## Context

forgectl has primitives — `launch` (#2, shipped v0.5.0), `tmux`, `projects` — but no way to
*compose* them. #9 adds the composition layer: a declarative **workflow DSL** that forgectl
parses into a step plan and executes against the local toolset (git, claude, tmux/cmux), so
orchestration lives in data, not one-off shell scripts. This turns forgectl from a toolbox into a
small platform — the keystone of the forge roadmap.

The canonical first workflow is **clean-room review**: sandbox a repo, strip its embedded
agent-instruction files (`CLAUDE.md`/`AGENTS.md`/…), launch a `claude` review in the sandbox,
collect, teardown. Stripping is a **security control** — it yields an unbiased review *and*
defends against prompt injection from a hostile `CLAUDE.md`.

This is a **spike**, not the full command. Deliverable: this design + ADRs, plus a walking
skeleton (parser + planner + `--dry-run` + a trivial-workflow executor) that proves the grammar
executes cleanly through the existing `exec.Runner` seam. The full clean-room path is the
follow-on.

## Decisions (settled 2026-07-01)

| Decision | Choice | ADR |
|---|---|---|
| File format | TOML step list | 0001 |
| Execution model | parse → resolve → verify → plan → execute; both sync + surface, default sync | 0002 |
| Sandbox mechanism | `worktree` into a temp dir (local) / `clone` (remote) | 0003 |
| Versioning | dual axis: `dsl_version` (grammar gate) + workflow `version` (provenance) | 0004 |
| Signing hook | scheme-agnostic `Verifier` interface, no-op default; #10 implements the scheme | 0002 |

## The grammar

A workflow file is a name + typed params + an ordered `[[step]]` array. `${var}` interpolates
params and prior-step exports.

```toml
# <config-dir>/workflows/clean-room-review.workflow.toml
dsl_version = 1                 # grammar contract — parser refuses an unknown version
name        = "clean-room-review"
version     = "1.0.0"           # this workflow's own semver (provenance for signing / registry)
description = "Sandbox a repo, strip agent-instruction files, launch a claude review, collect."

[params]
repo    = { required = true,          help = "owner/repo or local path" }
branch  = { default  = "main",        help = "ref to review" }
skill   = { default  = "code-review", help = "claude skill to run" }
posture = { default  = "",            help = "launch posture/model pin override" }

[[step]]
uses = "worktree"          # exports ${workspace}; clone fallback for a remote repo
repo = "${repo}"
ref  = "${branch}"

[[step]]
uses  = "strip"            # the clean-room control; omit globs → configured default set
globs = ["CLAUDE.md", "AGENTS.md", ".claude/", ".cursor/rules", ".github/copilot-instructions.md"]

[[step]]
uses    = "launch"         # exports ${review}; mode sync = wait+collect, surface = fire into tmux/cmux
skill   = "${skill}"
posture = "${posture}"
mode    = "sync"

[[step]]
uses = "collect"
from = "${review}"
to   = "${workspace}/../review.md"

[[step]]
uses = "teardown"          # rm the sandbox
```

### Step vocabulary

Every step delegates to a domain that already exists — the parser/planner/executor is the *only*
new machinery.

| `uses` | Delegates to | Exports | Notes |
|---|---|---|---|
| `worktree` / `clone` | git via `exec.Runner` | `${workspace}` | `worktree`: worktree into `os.MkdirTemp` (local) or `git clone` fallback (remote). An explicit `clone` **always clones, even locally** — full isolation on request, no `.git` back-pointer to the source checkout |
| `strip` | `os.RemoveAll` on resolved globs in `${workspace}` | — | the clean-room control; only ever mutates the sandbox, never the real checkout |
| `run` | `exec.Runner.Run(cmd…)` | stdout (optional capture) | arbitrary command escape hatch |
| `launch` | `internal/launch` | `${review}` (sync) | sync = Run + capture; surface = hand to `internal/tmux`/cmux |
| `collect` | write captured output → path | — | |
| `teardown` | remove the sandbox dir | — | idempotent |

## The execution model

```
 workflow.toml
     │ parse            (dsl_version checked FIRST — unknown → typed error, no execution)
     ▼
 Workflow{dslVersion, name, version, params, steps}
     │ + CLI --param → resolve (defaults, required-check, ${} interpolation)
     ▼
 Verifier.Verify(file)      ← #10 hook: scheme-agnostic interface, no-op (AllowAll) default
     │ ok
     ▼
 Plan []PlanStep            ── `--dry-run` prints the fully resolved plan; zero side effects
     │ execute — one exec.Runner threaded through every step; steps export into a shared Context
     ▼
 each PlanStep → its domain (table above)
```

Steps export variables into a shared `Context` (worktree → `${workspace}`, launch → `${review}`)
so later steps compose on earlier output. `--dry-run` stops after building `Plan` and prints it —
the resolved, side-effect-free step plan a user reviews before trusting a workflow.

`Context` resolves in two tiers: a variable already set (a param, or an earlier step's export)
interpolates to its value; a variable a *later* step will export (e.g. `${workspace}` referenced
before the `worktree` step has run) is marked deferred and renders as the literal `${workspace}`
at plan time, left for `execute` to resolve once that step's export lands. Any other unresolved
`${}` reference — a typo, a param that was never declared — is a plan-time error.

At execute time, the Executor **re-interpolates every step's fields against the live Context**
just before dispatch. Nothing is deferred at that point, so a forward reference — consuming an
export before the step that produces it has run — fails loudly, and no command ever receives a
literal `${...}` string as an argument.

(#10 note: the real `Verifier` runs on **raw file bytes** and should be hoisted ahead of `parse`
— authenticate-before-parse. `Verify` already takes the file, so it's a plumbing change, not an
interface change; see ADR-0002.)

### Versioning (dual axis)

- **`dsl_version`** — the grammar contract. The parser reads it before anything else and gates on
  a `SupportedDSLVersions` set; an unknown version is a typed refusal *before* planning, so a
  tampered file claiming a newer grammar can't slip unparsed steps past an older executor. New
  step verbs / fields bump the DSL version; existing signed files keep parsing under their
  declared version. The decode is **strict**: an unknown key is a parse error, so a typo'd field
  (`glob` for `globs`) can't silently no-op, and a newer grammar's fields can't be silently
  ignored under an older `dsl_version`.
- **`version`** — the workflow's own semver, author-bumped. It's the provenance handle: #10's
  attestation signs over `name@version` + file hash, #17's registry pins it, #16's dogfood
  reconciles against it on re-run. Editing a signed workflow bumps `version` and re-signs.

Both versions sit inside the signed content, so both are integrity-protected by #10.

## Package layout (house pattern: ops free of cobra)

`internal/workflow/` — ops, **zero cobra/bubbletea**:

- `parse.go` — TOML → `Workflow`; `dsl_version` gate; `SupportedDSLVersions`.
- `plan.go` — `Workflow` + params → `Plan`; `${}` resolution, required-param check.
- `exec.go` — `Plan` → execute; holds a constructor-injected `exec.Runner`; a `StepRegistry`
  maps `uses` → a `StepDef{Runner, Exports}`, co-declaring a verb's `StepRunner` and the
  variables it exports so the two can't drift apart. `Executor` is built via
  `NewExecutor(run exec.Runner, opts…)`, mirroring `tmux.New`, so `FakeRunner` asserts composed
  argv in tests.
- `verify.go` — `Verifier` interface + `AllowAllVerifier` default (#10 swaps in Ed25519/minisign).
- `context.go` — the shared variable `Context` (param + export map, `${}` interpolation).

`internal/cli/workflow.go` — `forgectl workflow run <name> [--dry-run] [--param k=v]…`, `list`
stub; registered in `root.go` via `AddCommand`; `flow` alias through the `forgive` registry
(`WorkflowAliases`).

`internal/config` — a `[workflow]` section for the default strip-list (and, later, #10's trust
store), added as one tagged struct field per the existing `LaunchConfig` pattern.

Workflow files: `<config-dir>/workflows/*.workflow.toml` — the same `os.UserConfigDir()` base as
`config.toml` (macOS `~/Library/Application Support/forgectl`, Linux `~/.config/forgectl`);
shipped built-ins embedded via `go:embed`; `run <name>` resolves name → file, user dir overriding
a built-in of the same name.

## Spike scope (what the walking skeleton builds)

**In:**
- `internal/workflow/`: `parse.go` (+ `dsl_version` gate), `context.go`, `plan.go`, `verify.go`
  (no-op `Verifier`), `exec.go` with a `StepRegistry` and these step runners implemented for the
  skeleton: `run`, `worktree`, `strip`, `teardown`. `launch`/`collect` are registered but may
  return a `not-yet-wired` sentinel.
- `internal/cli/workflow.go`: `workflow run <name> --dry-run`, `--param`; `flow` alias.
- `FakeRunner` tests asserting a trivial workflow (`worktree → strip → teardown`, or `run`-only)
  produces the exact composed argv, and that `--dry-run` executes zero commands.
- Reference file `clean-room-review.workflow.toml` (embedded), used by a parse+plan test.

**Out (follow-on, not this spike):**
- End-to-end clean-room execution (full `launch` + `collect` wiring, real claude session).
- #10 signing scheme (only the `Verifier` seam ships here).
- #20 quarantine as the source of the strip-list (the skeleton uses the config default set).

## Acceptance (spike)

- `forgectl workflow run <name> --dry-run` parses a TOML workflow, resolves params, and prints the
  fully resolved step plan without running any command (asserted: zero `Runner` calls).
- A workflow with an unsupported `dsl_version` is refused with a typed error before planning.
- A trivial workflow executes through `FakeRunner` with the expected argv sequence.
- A new workflow file requires **no Go changes** to parse + dry-run.

## Risks

- **Pre-#10 trust model** — until signing lands, a workflow file is exactly as trusted as a shell
  script (`run` is arbitrary command execution): run only workflows you authored or reviewed. The
  user-dir-overrides-builtin resolution belongs in #10's threat model too — a same-name user file
  silently replaces a shipped (eventually signed) built-in.
- **Grammar churn** — if the executed grammar proves wrong, #10/#16 churn. The skeleton's job is to
  execute a real (if trivial) workflow so the grammar is validated, not just parsed.
- **Sandbox safety** — `strip` deletes files; it must resolve globs *inside* `${workspace}` only. A
  path-escape guard (reject `..`/absolute globs) is a correctness-and-security requirement of the
  `strip` runner, spike or not.
