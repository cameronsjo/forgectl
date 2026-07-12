# forgectl Module Architecture

> Panel: plan-reviewer ×2 (conflict, underspecification) + owner-review ran — 2 Critical (shared root), 5 Important, 5 advisory, plus owner-lens recommendations; all folded in, 0 declined (4 owner questions routed to Cameron and answered: Phase 6 dropped, Phase-4 ship gate, count-pin growth cap).
>
> Execution note (2026-07-11): PR #77 (review dashboard) merged after this plan was authored —
> the live tree has **16** top-level groups (a `review` group) and **8** struct-kind config
> sections (`[review]`). Per this plan's own tier policy, `review` enters as `TierExtension`
> in the Phase 3 sweep and the completeness pins land at 16, not 15.

## Context

Cameron's concern: forgectl feels cobbled together and is growing without control (15 top-level command groups at plan time, ~63 commands, 14.5k prod LOC; roadmap queues more domains: board, kickoff, cmux, k8s, mcp...). He wants a kubectl-like shape: coherent core + plugin-style extensibility, explicitly including the DSL-driven workflow system.

Exploration finding: the domain layer is already healthy (clean dependency graph, uniform `New(exec.Runner)` clients, no god-packages). The incoherence lives in the **wiring**: `internal/cli/root.go` hand-wires the registrations; seven near-identical `applyXAliases` helpers; the workflow step vocabulary (`defaultRegistry()`) hand-maintained; config ownership implicit.

**Decisions made with Cameron:**
- **Model 1 — internal compile-time modules only.** Single binary. No subprocess plugins; the kubectl-style `forgectl-foo` PATH fallback is *documented as a future dispatch rung*, not built.
- **Tiering:** every manifest declares `core` vs `extension`. The enforcement is a **test, not prose**: the registry test pins the total module count and the exact core set, so any new module or tier change forces a deliberate test edit in the same diff (disclosed speed bump). Honesty note: tiering *sorts* growth, it does not *cap* it.
- **Workflow DSL is a first-class extensibility pillar**: two planes — code-plane (Go modules) and data-plane (user-authored `*.workflow.toml` composing module-contributed step verbs).
- **Phase 6 (workflows as top-level verbs) dropped** to ADR future-work, same treatment as the PATH fallback: this refactor is 100% user-invisible.
- **Phase 4 is an explicit ship gate** — a first-class mergeable landing that cures the wiring pain; Phase 5 (data-plane inversion) continues in the same effort but slips cleanly to a follow-on session if needed.

Honesty note (panel): `Deps` still carries the whole `config.Config` — this refactor buys config-ownership *legibility* (test-enforced claims), not decoupling; the two-phase decode that would decouple was declined as serving an out-of-tree model we excluded.

## Architecture

### Two extension planes

- **Code plane** — compile-time modules: a `module.Manifest` per domain, aggregated in one explicit slice. A module is *CLI surface wiring only*; the domain package beneath stays a plain library (evicting a module's CLI surface never breaks cross-domain library imports like pr→quarantine).
- **Data plane** — `*.workflow.toml` (embedded builtins + `<config>/workflows/` overrides) composing step verbs. Modules contribute step verbs via manifests; the data plane's vocabulary becomes registry-derived.

### New contract packages

**`internal/step`** — neutral step contract (so modules contribute steps without importing `workflow`, and `workflow` never imports modules). Imports only `internal/exec` + stdlib.

```go
package step
type Runner func(ctx context.Context, run exec.Runner, wctx *Context, s PlanStep) error
type Def struct {
    Runner  Runner
    Exports []string // context vars this verb sets (${workspace}, ${review})
}
type Registry map[string]Def // uses-token → definition
var ErrNotYetWired = errors.New(...) // sentinel moves HERE
```

`Context` and `PlanStep` move here from `internal/workflow`. **workflow keeps compatibility aliases under the OLD names**:

```go
// in internal/workflow — old names alias renamed step types:
type Context      = step.Context
type PlanStep     = step.PlanStep
type StepRunner   = step.Runner
type StepDef      = step.Def
type StepRegistry = step.Registry
var  ErrNotYetWired = step.ErrNotYetWired
```

**`internal/module`** — the manifest contract (plain struct — modules are data + constructors). Imports `config`, `exec`, `step`, cobra. Only `internal/cli` (and tests) import it.

```go
package module
type Tier int
const (
    TierCore      Tier = iota // load-bearing daily verbs
    TierExtension             // conveniences; individually evictable
)
type Deps struct {
    Cfg    config.Config
    Runner exec.Runner // OSRunner in Execute; FakeRunner in tests
}
type Manifest struct {
    Name         string              // canonical top-level verb: "tmux", "pr", "y"
    Tier         Tier
    ConfigKey    string              // toml section owned ("net", "launch"); "" = none
    GroupAliases []string            // cobra Aliases on parent cmd (launch→"cl", workflow→"flow")
    ArgvTokens   []string            // pre-cobra argv spellings converged by normalizeArgs (tmux→"tm")
    SubAliases   map[string][]string // canonical subverb → aliases (absorbs forgive's seven maps)
    New          func(Deps) *cobra.Command
    Steps        func(Deps) step.Registry // workflow verbs contributed; nil for most
}
```

No `Summary` field (cobra's `Short` on the constructed command already is the one-line description; registry docs/tests read `m.New(deps).Short`). No `Init`, no lifecycle, no capability flags.

### Registration: explicit slice (not `init()` self-registration)

`internal/cli/modules.go` holds `allModules() []module.Manifest` — deterministic, greppable, compile-error on constructor drift. Manifest instances live next to their command files (`var netModule = module.Manifest{...}` in `internal/cli/net.go`).

`newRoot(deps module.Deps)` collapses to a loop: `cmd := m.New(deps); cmd.Aliases = append(cmd.Aliases, m.GroupAliases...); applyAliases(cmd, m.SubAliases); root.AddCommand(cmd)`.

`Execute()` builds `deps := module.Deps{Cfg: cfg, Runner: exec.OSRunner{}}`. `newRoot`'s `tmuxClient/projClient/quarantineClient` params disappear (constructors build clients from `Deps.Runner`). **Execute retains its own `tmux.New(exec.OSRunner{})` client for the bare-invoke TUI/`runAction` path** (clients are stateless wrappers, two instances are free). Test seams (`newXCmdForClient`) untouched.

**Future dispatch rungs (ADR-documented, not built):** normalize argv → `launch` intercept → known verb? → *(rung A: exact-match workflow-name dispatch — future)* → *(rung B: `exec.LookPath("forgectl-"+verb)` subprocess — future)* → TUI.

### Config: static struct + reflection-checked ownership

`config.Config` stays exactly as is (tolerant-decode `Load()`, `IsZero()` pattern preserved). Manifest `ConfigKey` claims a toml section; the registry test enforces via reflection over `Config`'s toml tags: every claimed key exists as a field; every **struct-kind** field (`reflect.Kind() == Struct` discriminates the domain sections from host scalars `no_icons`/`log_level`/`log_file`) is claimed by exactly one module.

### Alias unification

One `applyAliases(parent, subAliases)` in `internal/cli/aliases.go` replaces all seven near-identical helpers (~70 LOC deleted). Details:
- The helpers are near-identical, NOT byte-identical: tmux's uniquely skips `alias == "-"` (`TmuxAliases["last"] = {"-"}` — `"-"` is an argv-normalization spelling, not a cobra alias). The shared helper carries **both** skips: `if a == "-" || a == sub.Name() { continue }`, with a test case pinning the tmux `last` behavior.
- Deleting the seven clones requires rewiring all seven `newXCmd` call sites **in the same commit** to `applyAliases(cmd, forgive.XAliases)` — maps still sourced from forgive until each module converts.

forgive's alias maps migrate into manifests as modules convert. `forgive` stays pure-stdlib, becomes data-parameterized: keep `Normalize`; replace package-level `Canonical` with `type Resolver` built via `forgive.NewResolver(aliases)`.

`normalizeArgs` becomes registry-driven; **only tmux declares `ArgvTokens`** initially → behavior byte-identical (pinned by `dispatch_test.go`). Forgiveness-for-all is a flagged opt-in follow-on.

### TUI: deliberately unchanged

The menu is six tmux actions bound to internal modes — a tmux session jumper, not a command palette. Stays hand-maintained and tmux-owned. ADR documents the future hook (optional `Menu` manifest field) without adding it.

### Workflow steps: one merged registry for plan-time AND run-time (Critical fix)

**Panel Critical (both seats):** `BuildPlan` reads export vocabulary from its own `defaultRegistry(nil)` to mark deferred vars (`${review}` — exported only by `launch`, consumed by the shipped `clean-room-review` builtin's `collect` step). Wiring contributions only into the Executor breaks plan-time deferral → `workflow run clean-room-review --dry-run` fails "unresolvable reference".

**Fix — preserve the code's own invariant ("exports live in one place — the same registry the Executor runs") by making the merged registry that one place:**
- `workflow.NewRegistry(contributed step.Registry) (step.Registry, error)` — merges built-ins ∪ contributions; returns an error on any collision (module-vs-builtin **and** module-vs-module).
- `BuildPlan` and `NewExecutor` **both take the merged registry** (signature change; call sites: `internal/cli/workflow.go` + workflow tests).
- Verb redistribution: `run`, `worktree`, `clone`, `teardown`, `collect` stay built-ins (generic/sandbox-backed). **`strip` → quarantine module** (`internal/quarantine/steps.go`, built from `quarantine.DefaultTargets`; deletes the workflow→quarantine import — dependency inverts as intended). **`launch` stub → launch module** (`internal/launch/steps.go`, returns `step.ErrNotYetWired`; proves the seam with a second contributor).
- `internal/cli/workflow.go` aggregates `m.Steps(deps)` across `allModules()` → `workflow.NewRegistry(...)` → passed to both BuildPlan and NewExecutor. `WithDefaultStripGlobs` deleted (quarantine's `Steps(deps)` closure reads `deps.Cfg.Workflow.StripGlobs`).
- **Data-plane safety test:** parse every embedded builtin workflow; assert every `uses` verb AND every consumed export is satisfied by built-ins ∪ default-module contributions. Pins the eviction story — names which builtins a module eviction would break.

## Migration path (every commit green: `go build ./... && go vet ./... && go test ./...`)

`newRoot` runs hybrid during migration — registry loop + remaining hand-wired `AddCommand`s — converting one module per commit.

- **Phase 0 — ADR** (~150 lines): `docs/adr/0005-module-architecture.md`.
- **Phase 1 — contracts** (~250 LOC): `internal/step/`; `internal/module/module.go`; `internal/cli/aliases.go` shared helper (with `"-"` skip) + delete seven clones + rewire seven call sites same-commit. Existing tests pass unmodified; verify this phase alone with vet + full suite (type-alias risk isolated here).
- **Phase 2 — registry scaffold + templates** (~200 LOC): `modules.go`, hybrid loop, Deps threading. Convert **net** (exercises ConfigKey) then **y** (exercises SubAliases). **Registry tests land here in two tiers:** *dynamic invariants* effective immediately over whatever `allModules()` contains — cross-module namespace uniqueness, config claims are a valid subset, command-tree smoke, **no-duplicate-root-command guard** (catches hybrid double-registration; cobra silently keeps both siblings); *completeness pins* (count, name set, core-tier set {tmux, projects, launch, workflow, pr, config}, full bijection "every struct section claimed") land in the final conversion commit (end of Phase 5).
- **Phase 3 — extension sweep**, one commit each (~30-50 LOC delta): pip → branch → clean → docker → bench → sessions → review → quarantine (manifest only; steps in Phase 5).
- **Phase 4 — core sweep, part 1** (~40-80 LOC each; workflow + pr complete the core in Phase 5): projects → config (ConfigKey `""` — displays, doesn't own) → tmux (ArgvTokens `["tm"]`; registry-driven `normalizeArgs` lands here, `dispatch_test.go` pins byte-identical behavior) → launch (`cl` GroupAlias; pre-cobra `launchIntercept` stays hardcoded, ADR-documented as host-owned). **← SHIP GATE: branch is mergeable here; the wiring pain is cured.**
- **Phase 5 — workflow step plane** (~200 LOC moved, net negative): `NewRegistry` + BuildPlan/NewExecutor signature change; `quarantine/steps.go` + `launch/steps.go`; workflow→quarantine import removed; workflow + pr manifests convert (**pr last**); completeness pins land; strip-glob tests relocate `workflow/exec_test.go` → `quarantine/steps_test.go` (assertions unchanged).

Rough total: ~800 LOC added, ~450 deleted, ~19 commits.

## Verification

- Existing test LOC pass unmodified except: `newRoot`/`BuildPlan`/`NewExecutor` signature call sites, strip-step test relocation.
- New `internal/cli/modules_test.go` (two-tier landing per Phase 2):
  - **Cross-module namespace uniqueness** over `Name ∪ GroupAliases ∪ ArgvTokens` — **deduped within a module first** (tmux legitimately carries `"tm"` in both GroupAliases and ArgvTokens; only cross-module collisions are defects — cobra's `findChild` resolves first-match-wins silently).
  - Config claims ↔ struct-kind toml-tag fields (bijection at completeness time; subset during migration).
  - Command-tree smoke: `m.New(Deps{Runner: FakeRunner})` non-nil, `cmd.Name() == m.Name`; root has no duplicate command names (hybrid guard).
  - Completeness pins (final commit): count, name set, core set.
  - Step registry: `NewRegistry` collision error (module-vs-builtin, module-vs-module); builtin `uses` + export coverage.
- Per-commit: `go build ./... && go vet ./... && go test ./...`.
- End-to-end at ship gate and at completion: build binary; spot-check `forgectl tmux ls`, `forgectl y copy/paste`, `forgectl workflow list`, `forgectl workflow run clean-room-review --dry-run` (proves merged registry serves plan-time deferral), `forgectl launch which`, aliases `forgectl tm` / `cl` / `flow`, and bare `forgectl` (TUI still opens).

## What does NOT change

Domain package internals + public APIs (additions only: `quarantine/steps.go`, `launch/steps.go`); `exec.Runner` seam + FakeRunner + `newXCmdForClient` seams; all flags/verbs/aliases/help text (including tmux `last`'s alias surface — the `"-"` spelling stays argv-only); `launch` intercept; `shouldLaunchTUI`; the TUI entirely; `config.Load()` semantics + `IsZero()`; the workflow TOML surface (ADRs 0001–0004 stand); `forgive.Normalize`; tmux-only argv-forgiveness scope. Zero behavior additions (Phase 6 dropped).

## Risks

1. **Import cycles** — `step` imports only `exec`; `module` imports config/exec/step/cobra; manifests live in `cli`; `workflow` never learns about `module`. Review rule: nothing under `internal/` imports `internal/cli`.
2. **Type-move breakage** — old-name aliases specified explicitly; Phase 1 verified alone.
3. **Plan-time/run-time registry drift** — the merged-registry design + the dry-run spot-check + export-coverage test pin it.
4. **Silent alias collisions** — cross-module uniqueness test.
5. **Hybrid double-registration** — no-duplicate-root-command guard.
6. **Scope creep via forgiveness-for-all** — fenced opt-in follow-on; behavior pinned.

## Critical files

- `internal/cli/root.go`, `internal/cli/execute.go`, new `internal/cli/modules.go` + `aliases.go`
- `internal/workflow/exec.go` + `plan.go` (`NewRegistry`, BuildPlan signature, verb redistribution)
- `internal/forgive/forgive.go` (maps → manifests; `Resolver`)
- `internal/config/config.go` (unchanged; reflection test targets its tags)
- New: `internal/step/`, `internal/module/`, `internal/quarantine/steps.go`, `internal/launch/steps.go`, `docs/adr/0005-module-architecture.md`

## Alternatives declined

- **Model 2 — subprocess PATH fallback now**: nothing exists to use it; documented as rung B.
- **Model 3 — full kubectl-parity plugin platform**: serves extenders who aren't Cameron.
- **`init()` self-registration**: serves third-party packages; explicit slice gives determinism + a meaningful completeness test.
- **Two-phase `toml.Primitive` config decode**: enables out-of-tree config types the model excludes; scatters tolerant-decode + `IsZero()` semantics.
- **Manifest-derived TUI menu**: the menu is a tmux jumper; abstraction with one implementer.
- **Workflows as top-level verbs now (was Phase 6)**: the one behavior addition in a user-invisible refactor; dropped to ADR rung A.
- **Structure-only / evict-now scope**: Cameron chose structure + tiering policy.
