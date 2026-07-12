# 0005 — Module architecture: internal compile-time modules with two extension planes

- **Status:** Accepted (2026-07-11)
- **Context:** forgectl grew to 16 top-level command groups (~63 commands) wired by hand.
  Related: 0001–0004 (workflow DSL); docs/plans/2026-07-11-module-architecture.md.

## Context

forgectl's domain layer is healthy — a clean dependency graph, uniform `New(exec.Runner)`
clients, no god-packages — but the **wiring** is not: `internal/cli/root.go` hand-wires every
registration, seven near-identical `applyXAliases` helpers copy the same loop, the workflow
step vocabulary is hand-maintained in `defaultRegistry()`, and config-section ownership is
implicit. The roadmap queues more domains (board, kickoff, cmux, k8s, mcp…), so the shape must
sort growth before the next arrivals.

The target is a kubectl-like coherence: a small load-bearing core plus explicitly-tiered
extensions — without leaving the single-binary model.

## Decision

**Internal compile-time modules only (Model 1).** One binary. Each command group is a
`module.Manifest` — plain data plus constructors — aggregated in one explicit slice
(`internal/cli/modules.go: allModules()`). No `init()` self-registration, no subprocess
plugins.

### Two extension planes

- **Code plane** — compile-time modules. A module is *CLI surface wiring only*; the domain
  package beneath it stays a plain library, so evicting a module's CLI surface never breaks
  cross-domain library imports (e.g. pr→quarantine).
- **Data plane** — user-authored `*.workflow.toml` files (embedded builtins +
  `<config>/workflows/` overrides) composing step verbs. Modules contribute step verbs via
  their manifests; the data plane's vocabulary is registry-derived, not hand-listed.

### Contract packages

- **`internal/step`** — the neutral step contract (`Context`, `PlanStep`, `Runner`, `Def`,
  `Registry`, `ErrNotYetWired`). Imports only `internal/exec` + stdlib, so modules contribute
  steps without importing `workflow`, and `workflow` never imports modules.
- **`internal/module`** — the manifest contract (`Manifest`, `Deps`, `Tier`). Imports
  config/exec/step/cobra. Only `internal/cli` (and tests) import it.

There is deliberately no `Summary` field on the manifest: cobra's `Short` on the constructed
command is the one-line description; a second surface would drift. Registry docs and tests
read `m.New(deps).Short`.

### Merged step registry serves plan-time AND run-time

`BuildPlan` marks deferred variables (e.g. `${review}`) from the same registry the Executor
runs — that invariant ("a verb's exports live in one place") predates this ADR and survives
it. `workflow.NewRegistry(contributed)` merges built-ins ∪ module contributions and errors on
any collision (module-vs-builtin and module-vs-module); **both** `BuildPlan` and
`NewExecutor` take the merged registry. Wiring contributions into only the Executor would
break plan-time deferral: `workflow run clean-room-review --dry-run` would fail
"unresolvable reference" because the `launch` verb (which exports `${review}`) lives in a
module. A builtin-coverage test parses every embedded workflow and asserts each `uses` verb
and each consumed export is satisfied by built-ins ∪ default-module contributions.

### Config: static struct + reflection-checked ownership

`config.Config` stays a static struct with tolerant decode and the `IsZero()` pattern. Each
manifest's `ConfigKey` claims one toml section; the registry test enforces the claims by
reflecting over `Config`'s toml tags — every claimed key exists, and every **struct-kind**
field (`reflect.Kind() == Struct` discriminates the domain sections from host scalars like
`no_icons`/`log_level`/`log_file`) is claimed by exactly one module. This buys config-ownership
*legibility*, not decoupling — `Deps` still carries the whole `config.Config`; the two-phase
`toml.Primitive` decode that would decouple was declined because it serves an out-of-tree
model this ADR excludes.

## Tier policy

Every manifest declares `TierCore` or `TierExtension`. **The registry test's pinned count and
pinned core set are THE gate** — prose here is policy, the test is enforcement:

> A new domain enters as `TierExtension` and requires editing the registry test's count pin
> in the same diff. Promotion to `TierCore` requires evidence of daily load-bearing use, a
> tier-pin test edit, and a note here. Core may import extension domain packages; core
> data-plane builtins may depend on extension step verbs only where the builtin-coverage test
> names the pairing.

Honesty note: tiering *sorts* growth, it does not *cap* it. The count pin is a disclosed
speed bump — a deliberate test edit in the same diff — not a ceiling.

Core set at adoption: **tmux, projects, launch, workflow, pr, config**. Everything else is
`TierExtension`.

## Eviction mechanics

Removing a module's CLI surface = deleting its entry from `allModules()` (and its manifest
var). The domain package stays importable by other domains. A `//go:build` tag per module
file is possible for compile-time eviction, but is not exercised. The builtin-coverage test
names which embedded workflows a step-contributing module's eviction would break.

## Alternatives considered

- **Model 2 — kubectl-style `forgectl-foo` PATH subprocess fallback now.** Nothing exists to
  use it; documented below as future rung B instead.
- **Model 3 — full kubectl-parity plugin platform.** Serves extenders who aren't the owner.
- **`init()` self-registration.** Serves third-party packages; the explicit slice gives
  determinism, greppability, and a meaningful completeness test.
- **Two-phase `toml.Primitive` config decode.** Enables out-of-tree config types the chosen
  model excludes; scatters the tolerant-decode + `IsZero()` semantics.
- **Manifest-derived TUI menu.** The TUI menu is a tmux session jumper (six tmux actions),
  not a command palette; abstraction with one implementer. It stays hand-maintained and
  tmux-owned.
- **Workflows as top-level verbs now.** The one behavior addition in an otherwise
  user-invisible refactor; dropped to future rung A.

## Future work (documented, not built)

The dispatch pipeline's rungs, in order: normalize argv → `launch` intercept → known verb? →
*(rung A)* → *(rung B)* → TUI.

- **Rung A — workflow-name dispatch.** `forgectl <name>` probes `workflow.Resolve(name)`
  (exact, un-normalized match only) before falling into the TUI; ~30 LOC at the
  `shouldLaunchTUI` seam. Makes data-plane workflows feel like verbs without registering them
  in cobra.
- **Rung B — `forgectl-<verb>` PATH subprocess fallback.** Contract: argv passthrough
  untouched, exit-code propagation, no env mutation. Only worth building when an out-of-tree
  consumer exists.
- **TUI `Menu` hook.** An optional manifest field for modules to contribute menu entries —
  deferred until a second module actually wants one.
- **Argv forgiveness for all modules.** `normalizeArgs` is registry-driven but only tmux
  declares `ArgvTokens`; extending forgiveness to every module is a flagged opt-in follow-on
  with its own behavior review.

## Consequences

- `newRoot` collapses to a loop over `allModules()`; adding a domain is one manifest + one
  slice entry + one test-pin edit.
- One shared `applyAliases` replaces seven clones; alias maps migrate from `forgive`'s
  package-level vars into manifests (`forgive` keeps `Normalize` and gains a
  data-parameterized `Resolver`).
- The workflow step vocabulary becomes registry-derived; `strip` moves to the quarantine
  module (inverting the workflow→quarantine import), `launch`'s stub moves to the launch
  module, proving the seam with two contributors.
- The refactor is 100% user-invisible: every flag, verb, alias, and help string is preserved,
  pinned by the existing test suite plus the new registry tests.
