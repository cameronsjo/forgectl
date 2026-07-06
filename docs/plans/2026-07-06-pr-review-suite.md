# forgectl pr-review suite — implementation plan

> **Plan PR (draft).** This document is the sole contract for a fresh implementer session with zero context from planning day.
> Refs cameronsjo/forgectl#3 (sub-epic umbrella) · Refs cameronsjo/forgectl#19 · Refs cameronsjo/forgectl#20 · Refs cameronsjo/forgectl#29 · Refs cameronsjo/forgectl#30 · Refs cameronsjo/forgectl#31 · Refs cameronsjo/forgectl#32.
> **Implementer note:** each Phase is its own branch + PR off `main`. Do not batch phases. Use "Refs cameronsjo/forgectl#N" in commits/PRs — never "Closes". Planned 2026-07-06.

---

## Context — the epic's shape, the codebase it lands in

**The epic (#3).** `forgectl pr` is forgectl's flagship next feature: an *isolated, clean-room AI review session* for a PR. The umbrella sub-epic #3 decomposes it into four buildable units (#29 core, #30 local, #31 discovery, #32 poll) resting on two foundational primitives (#19 `net`, #20 `quarantine`) plus the already-shipped launcher (#2, in `internal/launch`, v0.5.0). The forge roadmap (`docs/plans/2026-07-01-forgectl-forge-roadmap.md`) frames #29 as "the first marquee workflow" atop the platform spine. **This plan builds the `pr` command family as a dedicated `internal/pr` domain package** that *reuses* the sandbox/strip primitives already proven by the workflow DSL — see Decision D1.

**The codebase it lands in.** forgectl is a Go 1.25 Cobra CLI (`github.com/cameronsjo/forgectl`). The architecture is a strict two-layer "house pattern," stated verbatim in every issue's *House pattern* section and visible across `internal/`:

- **Ops layer** — `internal/<domain>/` (e.g. `tmux`, `projects`, `launch`, `workflow`). No Cobra, no Bubble Tea. Every external process goes through the `internal/exec` `Runner` seam. Constructor is `New(run exec.Runner, opts ...Option) *Client` with functional `Option`s.
- **Thin verb layer** — `internal/cli/<domain>.go`. Parses flags, calls ops, holds no domain logic. Each domain registers its parent command in `internal/cli/root.go` `newRoot()` via `root.AddCommand(...)`.
- **exec seam** — `internal/exec/exec.go`: `Runner` interface = `Run(ctx, name, args...) (string, error)` (captures stdout) + `RunInteractive(ctx, name, args...) error` (hands over the tty). Production is `OSRunner{}`; tests inject `exec.FakeRunner` (records every `Call{Name, Args, Interactive}`, cans output via `RunFunc`). **All `gh`/`git`/`tmux`/`claude` shell-outs route through `Runner`** — this is the testability contract #3 and #29 both mandate.
- **Config** — `internal/config/config.go`: TOML at `os.UserConfigDir()/forgectl/config.toml` (macOS `~/Library/Application Support/forgectl/`, Linux `~/.config/forgectl/`). Sections map to typed structs with `IsZero()` guards; missing file is never an error. `config.WorkflowsDir()` / `config.ConfigPath()` centralize path derivation — add new state paths there.
- **Logging** — global `slog`; the house narrative is `"Preparing to X."` / `"Successfully X."` / `"Failed to X."` with structured k/v. Quiet by default (`log_level = "off"`).
- **Tests** — table-driven, each file opens with a `// Test plan for <file>.go` comment enumerating cases by classification (see `internal/cli/projects_list_test.go`). Assert on `FakeRunner.Calls` argv.
- **Security idioms already in-tree** (reuse verbatim): `rejectOptionLike(field, value)` rejects a leading `-` before a value becomes a positional git arg; `git ... -- <positional>` ends option parsing; `validateStripGlob` / `withinWorkspace` reject `..`/absolute/symlink-escape before any `os.RemoveAll`. All in `internal/workflow/exec.go`.

**The machinery already built for #29 (do not rebuild).** The workflow DSL spike (PR #40, merged) shipped a working sandbox + clean-room control that ADR-0003 explicitly predicted would be "reusable later by #29 `pr`":

- `internal/workflow/exec.go` `newSandboxStep(alwaysClone)` — `git worktree add -- <tmpdir> <ref>` for a local repo, `git clone` for a remote, into `os.MkdirTemp("", "forgectl-workflow-*")`. Exports `${workspace}`.
- `teardownStep` / `cleanupSandbox` — idempotent `os.RemoveAll` of the sandbox.
- `newStripStep(defaultGlobs)` — the destructive clean-room control (`os.RemoveAll` on globs resolved **only inside `${workspace}`**), default list `["CLAUDE.md","AGENTS.md",".claude/",".cursor/rules",".github/copilot-instructions.md"]`.
- `internal/workflow/builtins/clean-room-review.workflow.toml` — the reference pipeline (worktree → strip → launch → collect → teardown). The `launch` and `collect` step runners are registered but **stubbed** (`notYetWiredStep` → `ErrNotYetWired`).
- `internal/config/config.go` `WorkflowConfig.StripGlobs` carries a `// #20 will source this from quarantine instead` TODO — i.e. #20 is *expected* to become the canonical owner of the instruction-file target list.

**The launcher (#2, shipped).** `internal/launch`: `Resolve(lc, cwd) Profile` (longest-prefix project match), `SessionArgs`/`BuilderArgs`/`AgentsArgs` (posture → claude argv), `ClaudePath(defaults)` (validated binary resolution — the "gateway auth, never a stale raw key" path #29 requires), `MergeEnv`, `Exec` (syscall.Exec — process replacement; **`pr` will NOT use `Exec`**, it dispatches claude into a tmux window via `Runner` — see Phase 1). `IsInteractiveTTY()` gates interactive UX. `huh`-based `Interview` is the model for the human-approval gate.

**Repo facts the implementer needs.**

- **Go version:** `go.mod` declares `go 1.25.0`; CI (`.github/workflows/release.yml`) uses `1.25`. `.mise.toml` says `go = "1.24"` — a stale drift; **use 1.25**. (Optionally fix `.mise.toml` in Phase 0; not required.)
- **CI:** there is **only `release.yml`** (goreleaser on tag push). **There is no test/lint CI workflow.** Verification is therefore *local* — see the Verification section. Adding a `ci.yml` is Decision D7 (recommended, but out of this suite's scope).
- **Open PRs:** `gh pr list --state open` → **none** at plan time. No in-flight overlap; the branch is clean.
- **Deps available:** `charmbracelet/huh` (forms/multiselect), `bubbletea`+`bubbles` (TUI lists), `lipgloss` (styling), `cobra`, `BurntSushi/toml`. No new heavy deps needed until the Phase 4 tray (cgo).

---

## Issues covered — per-issue disposition and phase

| Issue | Title | Disposition | Phase |
|---|---|---|---|
| **#3** | `pr` clean-room review + merge-driving (sub-epic) | Umbrella. Not a code unit — its checklist items (`teardown`/`close`, `cleanup`, `keys`, `list`/`attach`/`open`) land distributed across Phases 1–3. The original *merge-loop* facet is **out of scope** here (Decision D6 / Out of scope). | — |
| **#19** | `net` — cached internal-network reachability | Build first. Standalone `internal/net` ops + `forgectl net` verb + in-process API. | **0** |
| **#20** | `quarantine` — hide CLAUDE.md/AGENTS.md (two schemes) | Build first (parallel to #19). `internal/quarantine` ops + `forgectl quarantine` verb; becomes canonical owner of the instruction-file target list. | **0** |
| **#29** | `pr <ref>` — clean-room worktree review | The core. `internal/pr` package reusing sandbox+quarantine+net+launch. Includes `pr list/attach/open`, `pr teardown`/`close`, `pr cleanup`, `pr keys`. | **1** |
| **#30** | `pr local` — offline clean-room review | Reuses Phase-1 core; narrower isolation (read-only git, no network CLI, single write escape-hatch). | **2** |
| **#31** | `pr prs`/`dash`/`pick` — dashboards + reviewed-dimming | Discovery surfaces + `reviewedAt` timestamp store + `pr reviewed mark/unmark/sync`. | **3** |
| **#32** | `pr poll` — auto-review daemon + LaunchAgent + tray | Two-layer launch funnel, no-TTY logging, `pr poll install` LaunchAgent, optional cgo tray. | **4** |

---

## Approach — the phase architecture

Five phases, each an independently shippable PR off `main`. Phase 0 delivers the two foundations in parallel; Phases 1→4 stack on the `pr` core.

```
Phase 0  ┌── #19 net ────────┐   (independent, parallelizable)
         └── #20 quarantine ─┘
                │
Phase 1        ▼  #29  pr <ref> clean-room core  (needs net + quarantine + launch[shipped])
                │
        ┌───────┼────────────────┐
        ▼       ▼                 ▼
Phase 2 #30   Phase 3 #31       (#30 and #31 both need only Phase 1; independent of each other)
 pr local      pr prs/dash/pick
                        │
Phase 4                 ▼  #32  pr poll  (needs Phase 1 core + Phase 3 dash + net)
```

**Reuse-not-rebuild is the governing principle (Decision D1).** Phase 1 does not re-implement worktree/teardown. It **promotes** the sandbox helpers out of `internal/workflow` into a shared `internal/sandbox` package that both `internal/workflow` and `internal/pr` consume. The destructive `strip` stays in `internal/workflow`; the *reversible* clean-room control for `pr` lives in `internal/quarantine` (rename-with-suffix, not delete — see Decision D5 and Phase 0B).

Only two design axes are genuinely undecided — captured in **Decision points** below, not pre-baked here:

| Axis | Options | Recommendation |
|---|---|---|
| Sandbox helper location | (a) promote to `internal/sandbox`; (b) duplicate a small helper in `internal/pr` | **(a) promote** — ADR-0003 predicted the reuse |
| `net` cache lifetime | (a) on-disk TTL file (survives across `forgectl` invocations); (b) in-process only | **(a) on-disk** — "repeated calls are cheap" only holds cross-process with a file cache |

---

## Task breakdown

### Phase 0 — Foundations

#### 0A · `net` (Refs cameronsjo/forgectl#19)

**Scope.** A cached internal-network reachability probe other modules branch on (`pr` clean-room network posture, later `proxy`/`k8s`/`mcp`). Quiet by default; structured log only at the probe and the decision boundary.

**Files.**

- `internal/net/net.go` — ops. `type Client struct { run exec.Runner; ... }`; `New(run exec.Runner, opts ...Option) *Client`. Core API:
  - `Reachable(ctx context.Context) (bool, error)` — cache-first: read the on-disk cache (Decision D2), return if fresh; else probe, write cache, return.
  - Probe mechanism: a `net.DialTimeout("tcp", host:port, timeout)` against a configurable internal endpoint (default from a new `[net]` config section: `probe_host`, `probe_port`, `ttl`, `timeout`). Do **not** shell out for the probe (no `Runner` call needed for a raw dial) — but keep the `Runner` field for consistency and any future CLI probe. Injectable `dialFunc` option for tests (mirror `tmux.WithInsideTmux`).
  - Cache: `internal/net/cache.go` — JSON `{reachable bool, checkedAt time.Time}` at `config.NetCachePath()` (add this to `internal/config/config.go` alongside `WorkflowsDir`). Short TTL (default 60s).
- `internal/cli/net.go` — `newNetCmd()` → `forgectl net` prints reachable/unreachable + age of the cached answer; `--refresh` forces a re-probe; `--json` for scripting.
- Register in `internal/cli/root.go` `newRoot()`.
- Config: add `Net NetConfig` to `config.Config` with `probe_host`/`probe_port`/`ttl_seconds`/`timeout_ms` + `IsZero()`.

**Size:** S (~200–300 LOC + tests). **Tests:** `internal/net/net_test.go` (cache hit/miss/expiry via injected clock + `dialFunc`; probe success/failure; corrupt cache file → treated as miss, not fatal), `internal/cli/net_test.go` (`--json` shape, `--refresh` forces probe). **Model:** Sonnet (bounded, spec'd).

#### 0B · `quarantine` (Refs cameronsjo/forgectl#20)

**Scope.** Move AI-instruction files (`CLAUDE.md`/`AGENTS.md`/`.claude`) out of the way via **two non-colliding, reversible schemes**: a `_` prefix (standalone use) and a `.quarantined` suffix (used inside review worktrees). `--dry-run`, `--targets`, reversible `restore`. This is **rename-based and reversible** — distinct from the workflow `strip` step's destructive `os.RemoveAll` (Decision D5).

**Files.**

- `internal/quarantine/quarantine.go` — ops. `New(run exec.Runner) *Client` (Runner kept for house consistency; renames use `os.Rename` directly).
  - `type Scheme int; const (PrefixUnderscore Scheme = iota; SuffixQuarantined)`.
  - `Hide(ctx, root string, scheme Scheme, targets []string, dryRun bool) ([]Move, error)` — for each matched target under `root`, compute the renamed path per scheme, guard with a `withinRoot` check (port `withinWorkspace` from `internal/workflow/exec.go` — this is where the promotion in D1 pays off; share the guard), `os.Rename` (skip on dry-run), return the reversible `[]Move{from,to}`.
  - `Restore(ctx, moves []Move) error` — reverse each rename; idempotent (missing source is a no-op).
  - **Canonical target list** — move the default instruction-file list here as `quarantine.DefaultTargets` (exported). Then update `internal/workflow/exec.go` `defaultStripGlobs` to reference `quarantine.DefaultTargets`, and change the `config.go` comment (Decision D3). This closes the `// #20 will source this from quarantine instead` TODO.
- `internal/cli/quarantine.go` — `newQuarantineCmd()` with subverbs `hide` (default), `restore`, `status`; flags `--dry-run`, `--targets` (repeatable), `--scheme prefix|suffix`, `--root` (default cwd).
- Register in `root.go`.
- **Security:** all rename targets validated `withinRoot` before any FS mutation; reject `..`/absolute in `--targets` (reuse `validateStripGlob`).

**Size:** M (~300–400 LOC + tests). **Tests:** `internal/quarantine/quarantine_test.go` (each scheme's rename mapping; dry-run makes zero FS changes; `Restore` round-trips; symlink/`..` escape rejected; missing target no-ops), `internal/cli/quarantine_test.go`. **Model:** Sonnet.

> **Phase 0 is two independent PRs** (`feat/net`, `feat/quarantine`) — dispatch in parallel; no shared files except the small `config.go` additions (net adds `[net]`, quarantine touches the strip-list comment — non-overlapping edits, but land net first to avoid a trivial config.go conflict, or have one PR own config.go additions per the parallel-worktree contract-freeze rule).

---

### Phase 1 — `pr <ref>` clean-room core (Refs cameronsjo/forgectl#29)

**Scope.** Spin up an isolated review session for a GitHub PR: temp git worktree + tmux window + selectable agent under deny-by-default allowlist + human approval gate before any review posts. Plus session management (`list`/`attach`/`open`) and teardown (`teardown`/`close`, `cleanup`, `keys`).

**Prerequisite refactor (Decision D1).** Create `internal/sandbox/sandbox.go` by promoting `newSandboxStep`'s worktree/clone logic + `teardownStep`/`cleanupSandbox` + `withinWorkspace`/`rejectOptionLike` out of `internal/workflow/exec.go` into reusable functions: `Sandbox(ctx, run, repo, ref string, alwaysClone bool) (workspace string, err error)`, `Teardown(ctx, run, workspace string) error`, `WithinWorkspace(...)`, `RejectOptionLike(...)`. Update `internal/workflow/exec.go` to call the promoted package (keep its `StepRunner` wrappers thin). This is a pure move + delegation — existing workflow tests must still pass unchanged.

**Files.**

- `internal/pr/ref.go` — PR-ref parsing. `ParseRef(s string) (Ref, error)` accepting `org/repo#N`, a full GitHub PR URL, and a bare `N` (resolved against the cwd repo's origin). **Fully-anchored regexes** (`^...$`) — the #29 security requirement. `type Ref struct { Owner, Repo string; Number int }`.
- `internal/pr/session.go` — the all-paths core (the function #32's funnel will also call directly):
  - `Prepare(ctx, ref Ref, opts PrepareOpts) (Session, error)` — resolve PR head via `gh pr view <n> --json headRefName,headRepositoryOwner,...` through `Runner`; `sandbox.Sandbox(...)` a worktree for the head ref; `quarantine.Hide(workspace, SuffixQuarantined, quarantine.DefaultTargets, false)` (the clean-room control — reversible suffix scheme); write a **breadcrumb** file recording `{workspace, ref, agent, createdAt}` under a per-session dir in `config` state.
  - **Breadcrumb validation** (`internal/pr/breadcrumb.go`): `LoadBreadcrumb(path)` validates **both location** (must be under the forgectl-owned session state dir — reject anything else) **and content** (schema + workspace path must be an existing forgectl temp dir) before *any* `git -C <workspace>` runs. The #29 hard requirement.
- `internal/pr/agent.go` — selectable agent + the **pure decision function**:
  - `type LaunchPath int; const (InlineSeeded LaunchPath = iota; BareTUIEscalation)`.
  - `func LaunchPathFor(agent string) LaunchPath` — pure, table-driven, unit-tested. Agent A (inline-seeded): build the review argv via `launch.BuilderArgs(profile, []string{"-p", reviewPrompt})` under a hardened allowlist `--settings` file (deny-by-default) + `launch.ClaudePath` for gateway auth. Agent B (bare-TUI): launch the agent bare into the tmux window, then type the command into its TUI; the interactive escalation to an all-tools-approved mode is surfaced with a **warning, never silent** (a `slog.Warn` + a visible stderr banner).
  - `internal/pr/allowlist.go` — writes the hardened deny-by-default settings file into the workspace for agent A.
- `internal/pr/launch.go` — dispatch the composed argv into a **tmux window** (`tmux new-window -t <session> -n pr-<n> -c <workspace> -- claude ...`) via `Runner` — **not** `launch.Exec` (which replaces the forgectl process). Human approval gate: before any `gh pr review`/comment post, present a `huh` confirm (model: `internal/launch/interview.go`) surfacing the drafted review; only on approval does the post fire.
- `internal/pr/manage.go` — `List`, `Attach(path)`, `Open(path)` over the session breadcrumbs + tmux windows.
- `internal/pr/teardown.go` — `Teardown(path)` (alias `close`): **exact-match** allow-entry (a set membership check against known session paths — *not* a glob), so code under review can't invoke arbitrary subcommands. `Cleanup(date)` — date-wide worktree discard. Both call `sandbox.Teardown` + `quarantine.Restore` (or just discard the throwaway worktree).
- `internal/cli/pr.go` — `newPrCmd()` parent (`pr`), bare `forgectl pr <ref>` runs the core; subverbs `list`, `attach`, `open`, `teardown`(alias `close`), `cleanup`, `keys` (a static tmux-review cheatsheet, model: `internal/cli/tmux_cheat.go` + `internal/tui/cheatsheet.go`). Add `--agent`/`FORGECTL_PR_AGENT` env, `--headless`, `--dry-run`.
- `internal/cli/pr_test.go` + per-ops tests.
- Register `newPrCmd(...)` in `root.go`.
- Consume `net.Reachable` to choose network posture (e.g. warn/adjust when off-network before a `gh` round-trip).

**Size:** L (~800–1000 LOC + tests) — the flagship. **Tests:** `ref_test.go` (anchored-regex accept/reject table incl. injection attempts), `agent_test.go` (`LaunchPathFor` truth table), `breadcrumb_test.go` (location + content rejection cases), `session_test.go` + `teardown_test.go` (assert `FakeRunner.Calls` build the right `git`/`gh`/`tmux` argv; exact-match teardown rejects a non-member path), `sandbox_test.go` (moved-in coverage from workflow). **Model:** Opus (novel design, security-sensitive, largest surface).

---

### Phase 2 — `pr local` (Refs cameronsjo/forgectl#30)

**Scope.** Clean-room review of LOCAL committed changes — **no GitHub round-trip, fully offline**. Isolation is *narrower* than the remote path: read-only git verbs only, **no network CLI**, plus a single write escape-hatch directory for the findings file.

**Files.**

- `internal/pr/local.go` — `PrepareLocal(ctx, path string, opts) (Session, error)`: reuse `sandbox.Sandbox` (worktree of a local ref) + `quarantine.Hide`, but pass an isolation profile that (a) permits only read-only git verbs, (b) allows **no** `gh`/network CLI in the agent allowlist, (c) grants one writable escape-hatch dir (`<workspace>/../findings/`) for the review output. The allowlist file (`internal/pr/allowlist.go`) gains a `localProfile()` variant.
- `internal/cli/pr_local.go` — `pr local [path]` subverb (default cwd); reuses the Phase-1 approval gate and tmux-window dispatch.

**Size:** S–M (~250 LOC + tests) — mostly a narrower allowlist over Phase-1 core. **Tests:** `local_test.go` (allowlist denies network CLI; escape-hatch dir is the only writable path; read-only git verbs). **Model:** Sonnet.

---

### Phase 3 — `pr prs`/`dash`/`pick` + reviewed-dimming (Refs cameronsjo/forgectl#31)

**Scope.** Discovery surfaces + reviewed-state dimming.

**Files.**

- `internal/pr/reviewed.go` — the `reviewedAt` store: a flat `map[prKey]time.Time` persisted as JSON at `config.PrReviewedPath()` (add to `internal/config`). Storing the **timestamp, not a boolean**, makes auto-un-dim fall out of a two-time comparison at render (reviewed-before-vs-latest-author-activity). `Mark`, `Unmark`, `Sync`, and `IsReviewed(pr, latestActivity) bool` (dimmed iff `reviewedAt >= latestActivity`).
- `internal/pr/approval.go` — a **single** `ApprovalState(pr)` function shared by *both* the launcher's skip logic (Phase 1) and the picker's dimming, so the picker can never disagree with the launcher about "done." (Refactor Phase 1 to call this if it inlined the check.)
- `internal/pr/discover.go` — `PRs(ctx)` (cross-repo open PRs: direct + assignee + team-review-requested, via `gh search`/`gh pr list --json`), `Dash(ctx)` (active reviews + awaiting-you + your open PRs). **Parallel prep, serialized per-repo:** fan out PR prep concurrently, but serialize PRs sharing a clone (guard concurrent `git fetch`/`worktree add` from colliding on `.git/index.lock`) — a per-repo mutex map; iterate results in **input order** for deterministic output (model: `projects.Inventory`'s concurrent-fetch + deterministic-sort pattern).
- `internal/cli/pr_prs.go`, `pr_dash.go`, `pr_pick.go` — `pr prs` (dashboard), `pr dash` (unified cockpit), `pr pick` (huh multiselect → parallel prep + **serial** session launch), `pr reviewed mark|unmark|sync`. Dimmed rows via `lipgloss` faint style; use `internal/tui` list components.

**Size:** L (~600–800 LOC + tests). **Tests:** `reviewed_test.go` (timestamp comparison auto-un-dims on fresh activity; mark/unmark/sync round-trip; corrupt store → empty, not fatal), `discover_test.go` (per-repo serialization prevents concurrent worktree-add on one clone; deterministic input-order output; degraded-host note like `projects.Inventory`), `approval_test.go` (launcher and picker agree). **Model:** Opus (concurrency correctness + the launcher/picker agreement invariant).

---

### Phase 4 — `pr poll` daemon + LaunchAgent + tray (Refs cameronsjo/forgectl#32)

**Scope.** Auto-review daemon that *stages* (never posts) a review session per newly-discovered awaiting-you PR; the human attaches and approves.

**Files.**

- `internal/pr/poll.go` — the daemon: on a fixed interval, `discover.Dash` → for each new PR, call the Phase-1 **core `Prepare` directly** (not through the CLI layer). **Two-layer launch funnel:** the all-paths core (`session.Prepare` — worktree prep, reviewed-marking, no-TTY routing) is reused verbatim; interactive-only UX (the post-launch navigation prompt) stays one layer up in the CLI command, because the daemon never goes through the CLI. Stateful dedup: the `reviewed.go` store doubles as in-flight memory. **No-TTY-safe:** when `!launch.IsInteractiveTTY()`, all narrative funnels to a log file (reuse `config.SetupLogger` with a poll-specific log path).
- `internal/pr/launchagent.go` — `Install()` writes a per-user macOS **LaunchAgent** plist to `~/Library/LaunchAgents/com.cameronsjo.forgectl.pr-poll.plist` and `launchctl bootstrap`s it; **idempotent** (re-install overwrites + reloads cleanly).
- `internal/pr/tray_darwin.go` (build tag `//go:build darwin && cgo`) + `internal/pr/tray_stub.go` (`//go:build !darwin || !cgo`) — optional menu-bar tray: status + Check-now / Pause / Attach / Quit. "Attach" spawns a terminal (the GUI process has no TTY). **Ships behind a build tag; the default release binary omits it** (Decision D6). Requires a tray dep (`fyne.io/systray` or similar) — gated by cgo.
- `internal/cli/pr_poll.go` — `pr poll [--tray]`, `pr poll install`.

**Size:** L (~500–700 LOC + tests, excluding tray). **Tests:** `poll_test.go` (dedup via reviewed store; no-TTY routes to log not stdout; the core-function funnel is exercised by both CLI and daemon paths), `launchagent_test.go` (plist content + idempotent re-install; guard with `runtime.GOOS == "darwin"` skips). Tray is manual-verified (cgo/GUI, not unit-testable). **Model:** Opus for the funnel/daemon correctness; tray is Sonnet mechanical.

---

## Sequencing & dependencies

| Phase | Gate (must be merged first) | Unblocks |
|---|---|---|
| **0A `net`** | — | Phase 1 (network posture), Phase 4 |
| **0B `quarantine`** | — (parallel to 0A) | Phase 1 (clean-room control) |
| **1 `pr <ref>`** | 0A + 0B + `internal/launch` (shipped) | Phases 2, 3 |
| **2 `pr local`** | Phase 1 | — |
| **3 discovery** | Phase 1 | Phase 4 |
| **4 `pr poll`** | Phase 1 + Phase 3 (dash cockpit) + 0A | — |

**Notes.** 0A and 0B are independent — land as two parallel PRs (freeze the tiny `config.go` additions to one owner to avoid a spurious conflict). Phase 1 carries the D1 sandbox-promotion refactor, so it must land before 2/3 touch `internal/sandbox`. Phases 2 and 3 are mutually independent (both need only Phase 1) and may proceed in parallel. Phase 4 needs the Phase-3 `dash` (its cockpit) and the reviewed store (its dedup memory).

---

## Verification

**There is no test CI workflow** (`release.yml` is tag-only). Every phase's PR is verified **locally**, and the PR body must paste the transcript. The universal gate for each phase:

```bash
go build ./...        # compiles
go vet ./...          # no vet findings
go test ./...         # all packages green (include the moved workflow tests in Phase 1)
gofmt -l .            # zero unformatted files
```

Per-phase real-invocation transcripts (build once with `go build -o /tmp/forgectl .`):

**Phase 0A — `net`:**

```
$ /tmp/forgectl net --json
{"reachable":true,"checkedAt":"2026-07-06T…","ageSeconds":0}
$ /tmp/forgectl net           # second call within TTL → served from cache, no re-probe
internal network: reachable (cached 3s ago)
```

Expected: first call probes + writes cache; second within TTL logs a cache-hit at `--log-level debug` and issues no dial.

**Phase 0B — `quarantine`:**

```
$ cd /tmp/demo-repo && ls
CLAUDE.md  AGENTS.md  README.md
$ /tmp/forgectl quarantine hide --scheme suffix --dry-run
would rename CLAUDE.md → CLAUDE.md.quarantined
would rename AGENTS.md → AGENTS.md.quarantined
$ /tmp/forgectl quarantine hide --scheme suffix && ls
AGENTS.md.quarantined  CLAUDE.md.quarantined  README.md
$ /tmp/forgectl quarantine restore && ls
AGENTS.md  CLAUDE.md  README.md
```

Expected: `--dry-run` mutates nothing; `hide` then `restore` round-trips exactly.

**Phase 1 — `pr <ref>` (dry-run first, then a live worktree):**

```
$ /tmp/forgectl pr cameronsjo/forgectl#42 --dry-run
plan: worktree cameronsjo/forgectl@<headRef> → strip instruction files → launch agent(claude, inline-seeded) → approval gate
$ /tmp/forgectl pr list
(no active review sessions)
```

Expected: ref parses; `--dry-run` prints the resolved plan and creates no worktree/tmux window; anchored-regex rejects a malformed ref (`/tmp/forgectl pr 'foo#bar'` → error). A live run (inside tmux) creates a `pr-42` window, quarantines instruction files in the worktree, and blocks on the approval gate before any `gh pr review`.

**Phase 2 — `pr local`:**

```
$ /tmp/forgectl pr local . --dry-run
plan: worktree <cwd>@HEAD → strip → launch agent(local profile: read-only git, no network CLI, findings/ writable) → approval gate
```

Expected: the allowlist in the dry-run plan shows network CLI denied and exactly one writable escape-hatch dir.

**Phase 3 — discovery:**

```
$ /tmp/forgectl pr prs
REPO                     #   TITLE                       STATE
cameronsjo/forgectl      42  feat: …                     open
cameronsjo/cadence       17  fix: …          (dimmed — reviewed 2h ago)
$ /tmp/forgectl pr reviewed mark cameronsjo/forgectl#42 && /tmp/forgectl pr prs
# row 42 now renders dimmed
```

Expected: a reviewed row is dimmed; simulating fresh author activity (a later PR `updatedAt`) auto-un-dims it. `pr pick` prep runs concurrently but serializes two PRs on the same clone.

**Phase 4 — `pr poll`:**

```
$ /tmp/forgectl pr poll --once --log-file /tmp/poll.log   # single tick, no-TTY safe
$ cat /tmp/poll.log
… "Successfully staged review session" pr=cameronsjo/forgectl#42 workspace=/tmp/forgectl-…
$ /tmp/forgectl pr poll install && launchctl list | grep forgectl
com.cameronsjo.forgectl.pr-poll
```

Expected: `--once` stages a session and writes narrative to the log (not stdout); re-running dedups an already-staged PR; `install` is idempotent.

---

## Decision points — Cameron calls with recommendations

**D1 — Promote the sandbox helpers to `internal/sandbox`? (Recommended: yes.)** ADR-0003 explicitly predicted `worktree`/`teardown` would be "reusable later by #4 `clean` and #29 `pr`." Promoting them out of `internal/workflow/exec.go` into `internal/sandbox` gives `pr` the primitive without duplication. Alternative: duplicate a small helper in `internal/pr` (faster, but drifts). *Recommend promote in Phase 1.*

**D2 — `net` cache: on-disk TTL file, or in-process only? (Recommended: on-disk.)** Each `forgectl` invocation is a fresh process, so an in-process cache never helps repeated CLI calls — the issue's "repeated calls are cheap" only holds with an on-disk short-TTL cache (`config.NetCachePath()`). *Recommend on-disk, 60s default TTL.*

**D3 — Make `quarantine` the canonical owner of the instruction-file target list? (Recommended: yes.)** `config.go` already carries the TODO `// #20 will source this from quarantine instead`. Export `quarantine.DefaultTargets` and have `internal/workflow`'s `defaultStripGlobs` reference it, retiring the duplicated list. *Recommend yes.*

**D4 — Concrete agents behind `--agent`. (Needs your call.)** #29 describes two non-identical agents: **A** = inline-seeded under a hardened allowlist (`claude -p` fits exactly — the shipped `launch.BuilderArgs` + `--settings`); **B** = launched bare with the command typed into its TUI + an interactive escalation-to-all-tools (surfaced with a warning). The plan is agent-agnostic (`LaunchPathFor(agent)` is a pure table). *Confirm agent B's identity* (e.g. Codex / Cursor-agent / Gemini CLI) so the implementer wires the concrete launch argv and TUI-typing sequence.

**D5 — `pr` clean-room control: reversible rename (quarantine) vs destructive strip? (Recommended: quarantine rename.)** The worktree is throwaway, so `strip`'s `os.RemoveAll` would also work — but #20's `.quarantined` suffix scheme is described as "used inside review worktrees," and rename+restore is auditable and reversible (you can inspect what was hidden). *Recommend `pr` uses `quarantine.Hide(SuffixQuarantined)`, reserving destructive `strip` for the workflow DSL path.*

**D6 — Scope of the Phase-4 tray + the original merge-loop facet. (Recommended: defer both.)** (a) The macOS menu-bar tray needs a cgo/GUI dep and can't be unit-tested — ship it behind a build tag, default binary omits it. (b) Epic #3's *original* merge-driving loop (multi-select open PRs → poll→triage→merge, a `review-loop` port) is a **separate facet** that #3 says "folds into #31's dashboard + merge path." *Recommend keeping the merge-loop out of this suite* (file/track it as a follow-on to Phase 3's `dash`) so the flagship ships the clean-room review facet first.

**D7 — Add a test CI workflow? (Recommended: yes, separately.)** The repo has only `release.yml`; nothing runs `go test` on PRs. A tiny `.github/workflows/ci.yml` (build+vet+test on push/PR, Go 1.25) would gate every phase automatically. *Recommend a standalone chore PR before or alongside Phase 0 — out of this suite's issue scope but high leverage given five stacked PRs.*

---

## Out of scope

- **The merge-driving loop** (epic #3's original scope: multi-select open PRs → drive to merge, the `review-loop`/`poll-prs.sh` port). Folds into a Phase-3 follow-on per Decision D6; not built here.
- **Workflow DSL signing (#10), the `board` (#12), `audit` (#14), the DSL registry (#17)** — separate roadmap items.
- **Wiring the DSL's stubbed `launch`/`collect` steps** as a *workflow-file* path. The `pr` command is the flagship surface; completing the `ErrNotYetWired` workflow steps is a separate #9-lineage task. (Phase 1 reuses the *sandbox* primitives, not the DSL's launch/collect step runners.)
- **Non-macOS tray / LaunchAgent equivalents** (systemd, etc.) — #32 is explicitly macOS.
- **`.mise.toml` Go-version drift fix** (1.24→1.25) — a one-line optional cleanup, not gating.
- **`forgectl clean` (#4), `branch` (#5), and other roadmap commands** — unrelated to the pr suite.
