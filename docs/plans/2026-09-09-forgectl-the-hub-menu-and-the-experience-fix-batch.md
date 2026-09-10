---
body_sha256: "d6bc8befecdaad68155d0c230be61365d7d3293eed96f56c68cf17fd7bfea864"
session_id: "38e660c6-ecd8-424d-9fbc-400ea43ce8e6"
model: "claude-fable-5-1"
harness: "claude-code 2.1.267"
machine: "cf6e768835c7"
approved_session_id: "bcffd4ae-076f-40f2-8c6d-b7c2835e4e52"
status: in-progress
next: "wave 0 dispatched (Tasks 3, 4, 5, 6); Task 1 after Task 3 merges; Task 7 own session after the forgectl PRs merge"
branch: plan/hub-menu
pr: —
updated: 2026-09-09
date: 2026-09-09
---

# forgectl: the hub menu and the experience-fix batch

## Context

Bare `forgectl` on a terminal opens a six-item tmux menu and nothing else. The binary registers 31 modules (`internal/cli/modules.go:14-48`), so 27 command groups are reachable only by typing them, and neither the menu nor `--help` mentions the other mode. Three reviewer seats (user, developer, agent experience) ran over 0.18.0 on 2026-09-09 and produced five tracker issues: the hub menu ([#479](https://github.com/cameronsjo/forgectl/issues/479)) and four independent defects ([#480](https://github.com/cameronsjo/forgectl/issues/480) test suite fails with worktrees present, [#481](https://github.com/cameronsjo/forgectl/issues/481) title-cased paths in errors, [#482](https://github.com/cameronsjo/forgectl/issues/482) `--json` missing on state verbs, [#483](https://github.com/cameronsjo/forgectl/issues/483) `docs list` hang).

ADR-0005 (`docs/adr/0005-module-architecture.md:124-126`) declined a *manifest-derived* menu: a `Menu` hook modules contribute entries to, "deferred until a second module actually wants one". The hub adds no manifest field and no hook. It derives its rows from the cobra tree plus `module.Manifest.Tier`, so it is a compliant addition, not a reversal. The ADR gets a dated addendum because the hub changes two things the ADR text describes: `shouldLaunchTUI` narrows to the bare arm (§Host dispatch rungs), and the tmux jumper becomes one row of a larger screen.

## Goal

Bare `forgectl` becomes a hub that reaches every registered module, with the tmux jumper preserved as one row, and the four verified defects are fixed. Five forgectl PRs, one per issue (#482 partial), plus one cadence-monorepo PR for the skill, run as a separate session.

## Alternatives declined

- **New `Manifest` field for TUI visibility or ordering** — a fourth thing to keep in sync beside the count pin, the name pin, and the README gate (`internal/cli/modules_test.go:128-152, 207`). `Tier` already ranks the core modules and cobra's `Short` already describes each.
- **A flat 31-row command palette** — ADR-0005's objection to a palette holds on contents. A router with core rows plus one filterable "all commands" row keeps the first screen at seven or eight rows.
- **Boa (a Bubble Tea command picker for cobra) or bubble-shell** — adopt-over-build was checked. Both add a runtime dependency, neither preserves the tmux jumper as a row, and neither draws from `internal/theme`, which `theme_literals_test.go` enforces.
- **One PR for all five issues** — the four defects are independent and cold-finishable; one PR would hold four merges hostage to one design review.
- **Fix every `--json` gap in #482 now** — 15 verbs plus `doctor --only` plus MCP `structuredContent` is its own project. This plan closes the six verbs the `using-forgectl` skill presents as a family and leaves #482 open.
- **Newline-delimited `docs list --json`** — a breaking change to a shipped shape, against ADR-0008 clause 2 ("JSON only grows"). `--limit` solves the "make `head` work" need without it.
- **A `doctor` first-run row** (#479 direction) — no "has doctor ever run" state exists; inventing one is more machinery than the row is worth. The `init` row covers the case that has a signal (`config.toml` absent).

## Panel

Panel: plan-reviewer ×2, red-team-reviewer, user-experience-reviewer, agent-experience-reviewer, cameron-review, security-reviewer (routed round, 2 lines ruled: 1 no change, 1 folded) ran — 88 findings, 80 folded in, 8 declined (see Panel review — findings declined)

## Panel review — findings declined

- **[Task 6] Drop `--timeout` unless the hang reproduces** (red-team) — the premise does not reproduce on this machine today (0.32s, no cloud-backed root), but the agent-experience seat observed the hang twice this morning. A deadline is cheap hardening; the timing verification step is dropped instead, since it cannot go red.
- **[Task 6] Default `--json` to `--limit 50`** (agent-experience) — a silent cap changes what an existing consumer receives; the skill tells agents to pass `--limit` instead.
- **[Task 3] Trailing `drift: N missing (exit 1)` line under the sections** (user-experience) — the `Long` documents the codes; a second status line on every run is noise for the common case.
- **[Task 1] `doctor` first-run row** (user-experience, restore-or-record) — recorded above under Alternatives declined.
- **[Task 4] Failure text names the directory the offending file sits under** (plan-reviewer, advisory) — out of #480's scope; the skip fixes the cause.
- **[Task 1] Delete `parentTakesArg` as dead code** (plan-reviewer, advisory) — retained: it is the predicate `HubLeaf.NeedsArgs` derives from.
- **[Task 5] Golden test per verb on the exact key set** (agent-experience) — folded as a key-set assertion, declined as a full golden file: golden output files for five verbs invite snapshot churn on every human-table change.
- **[Task 7] Reword "the local docs reader" trigger phrase** (agent-experience, advisory) — folded in spirit; the exact phrase is the Task 7 implementer's to tune against the description length gate.

## Architecture

`internal/tui` cannot import `internal/cli` (cli imports tui). Today `tui.Run(ctx, client, noIcons, th)` takes the tmux client and returns a deferred `Action` that `dispatchAction` (`internal/cli/execute.go:336`) executes once Bubble Tea has released the terminal. The hub keeps that shape:

- **Hub rows are built in `internal/cli`** (`internal/cli/hub.go`, `buildHub(root *cobra.Command, mods []module.Manifest, configPresent bool) []tui.HubEntry`): for each manifest in `allModules()`, the matching `root.Commands()` child gives `Name`, `Short`, and its `Commands()` give the leaves. `Core` comes from `Tier`. `NeedsArgs` on a leaf comes from the same predicate `parentTakesArg` (`execute.go:455`) applies to a parent's `Use` line. `configPresent` comes from `config.ConfigPath()` (`internal/config/config.go:1102`) plus `os.Stat`.
- **Row order:** when `config.toml` is absent, row 1 is `first run: set up forgectl — creates config.toml (init)`. Then `tmux — sessions, windows, tree` (opens today's `menuMode` screen unchanged in content), then the `TierCore` modules in registry order minus `tmux` (`projects`, `config`, `launch`, `workflow`, `pr`: five rows), then `all commands (N) — docs · env · sessions · docker · k8s · … — type to filter` where `N = len(allModules()) - core` and the sample is the first five extension names. Seven rows normally, eight on first run.
- **Navigation:** the hub is the quit level. `q`/`esc` in `hubMode` quits; in `menuMode` returns to the hub (today it quits, `tui.go:274-280`); every deeper mode returns to `menuMode` as today. `menuMode`'s footer changes `q/esc quit` to `q/esc back`. Hub footer, verbatim: `↑↓ move · 1-9 jump · enter select · / filter · q/esc quit · forgectl --help lists every command`.
- **Selecting a module row** opens its leaves as a filterable list (a module with no leaves, e.g. `doctor`, runs directly). A leaf with `NeedsArgs` shows its `Use` line as the description (`pr <ref> — needs a ref`); selecting it prints `$ forgectl pr <ref>` to stderr and returns to the shell without running, so the user has the exact line to complete. Any other leaf returns `Action{Kind: ActionRunVerb, Argv: []string{module, leaf}}`.
- **Running a selected verb re-enters the dispatch pipeline**, not `execCommand` directly: `Execute` handles `ActionRunVerb` by printing `$ forgectl <argv>` to stderr (prefix `$ ` always present; muted style when styled) and then calling the same argv path a typed command takes, so `launchIntercept` (`execute.go:146`) and the extension rungs (`execute.go:159`) apply to a hub-selected `launch` exactly as to a typed one. The `$ ` prefix is the signal under `NO_COLOR`; a comment at the join site notes `Argv` is registry-derived identifiers only, so it needs no shell quoting yet.
- **Routing:** `shouldLaunchTUI` (`execute.go:368-392`) keeps only the bare-invoke arm. Unknown top-level verbs and unknown subverbs fall to cobra's unknown-command error on TTY and headless alike. The `forgectl-<verb>` PATH rung runs before `decideRoute` today (`tryExtensionRungs`, `execute.go:159`) and is untouched; cobra's error fires only for verbs the rungs did not claim. The unknown-command tail becomes `Try --help for usage, or run forgectl with no arguments for the menu.`
- **Ordering inside the hub PR is load-bearing:** the unknown-subverb TUI arm is today the only thing between `forgectl quarantine restor` and `runQuarantineHide` (`quarantine.go:78-82`, a bare `quarantine` hides files). The `Args: cobra.NoArgs` commit lands **before** the routing-arm removal, in the same PR.
- **Root help:** `root.go` registers two cobra groups, `Everyday` and `More`, and the `allModules()` loop (`root.go:81-97`) sets `GroupID` from `Tier`. Root `Long`, verbatim: `Two ways in: type a command — forgectl tmux ls — or run forgectl with no arguments for a menu over every command group.`

## Tech Stack

- Go 1.26 (`go.mod`), cobra + fang, Bubble Tea v2 + bubbles list, lipgloss via `internal/theme`
- Tests: stdlib + testify, table-driven; `go vet ./...` separately from build; `golangci-lint run --new-from-rev=origin/main`
- Release (forgectl only): release-please generates `CHANGELOG.md` from conventional commit subjects. No hand-written changelog entry and no `[Unreleased]` section (`AGENTS.md:16`). A consumer-visible change that needs richer prose than the squash title carries one `BEGIN_COMMIT_OVERRIDE … END_COMMIT_OVERRIDE` block in the PR body (`AGENTS.md:19-30`). The cadence monorepo (Task 7) keeps its per-plugin `[Unreleased]` entries.

## Global Constraints

- No new runtime dependencies.
- Every colored surface draws from `internal/theme`; `theme_literals_test.go` and `deps_single_lipgloss_test.go` enforce it.
- Every `--json` emitter must satisfy `internal/cli/json_encoder_wiring_test.go` (raw `encoding/json` use outside the sanctioned seam fails the suite). Read that test before adding a flag.
- Anything derived from tmux output or config passes through `termsafe` before reaching the terminal.
- No error message leads with a raw path, identifier, or flag value: `env file %s not found`, never `%s not found`. Task 3 adds the guard; later tasks obey the convention.
- Branch-mode repo: all edits in worktrees under `.claude/worktrees/<slug>`, never the primary checkout. Read-only test runs from the primary checkout are fine. Commit with explicit paths, provenance trailers per git-workflow § Commit Provenance, `--no-follow-tags` on every push.
- Conventional commit subjects, scoped. One PR per issue, `Closes #N` in the body (Task 5 and Task 7 bodies say `Part of #482` with no closing verb).
- Probes that run the binary first build it: `go build -o forgectl .` in the worktree (`go build ./...` discards the executable; `/forgectl` is gitignored so a stale binary can sit there). Every probe runs under `perl -e 'alarm 20; exec @ARGV' -- ./forgectl … </dev/null`.
- Recorded probe output goes in the PR body under a `## Measured` heading, command and output together. A recipe without its output is a claim.
- `cadence-forge:polish` on every PR before ready; run the review arms against a diff built from the worktree.
- The registry pins (`modules_test.go:128` count, `:133-152` names) do not change.

## Orchestrator

**Driver:** fable — this session orchestrates; every task is a fresh Sonnet implementer from this plan file. No security trigger: no task touches auth, secrets, or a guard. The security seat's routed ruling is folded into Tasks 1 and 3.

---

## Tasks

> Reports dir: orchestrator `mktemp -d` outside every worktree at dispatch; each task's report path is `<reports-dir>/task-N.md`, pre-touched empty; the agent replies with only `wc -l` of that path.

### Task 1 — Hub menu, routing, root help (#479)

**Files:**
- Create: `internal/cli/hub.go` (`buildHub`), `internal/cli/hub_test.go`
- Modify: `internal/tui/tui.go` (`hubMode`, `HubEntry`, `HubLeaf`, `ActionRunVerb`, `Action.Argv`, esc semantics, footers), `internal/tui/tui_test.go`
- Modify: `internal/cli/execute.go` (`runAction`/`dispatchAction` signatures; `ActionRunVerb` re-enters the argv pipeline; `shouldLaunchTUI` bare arm only; unknown-command tail text), `internal/cli/execute_test.go`
- Modify: `internal/cli/tmux.go:46-56`, `internal/cli/projects.go:115`, `internal/cli/quarantine.go:82` (`Args: cobra.NoArgs`; `tmux`'s `RunE` passes the hub and starts in `menuMode`). `internal/cli/launch.go:87` is left alone: `launch [harness args…]` legitimately takes arbitrary args.
- Modify: `internal/cli/root.go` (groups from `Tier`; `Long` verbatim from Architecture)
- Modify: `docs/adr/0005-module-architecture.md` (addendum dated 2026-09-09: the hub derives from the cobra tree and `Tier`, adds no manifest field, the deferred `Menu` hook stays deferred; `shouldLaunchTUI` now has one arm; the tmux jumper is one row)
- Modify: `README.md:3` (module count 29 → 31) and `README.md:5-8` (two-mode paragraph names the hub)

**Interfaces:**
- Consumes: none
- Produces: `tui.HubEntry{Name, Short string; Core bool; Leaves []HubLeaf}`; `tui.HubLeaf{Name, Short, Use string; NeedsArgs bool}`; `tui.RunOptions{Hub []HubEntry; StartInTmux bool; NoIcons bool; Theme theme.Theme}`; `tui.Run(ctx, client, opts RunOptions) (Action, error)`; `tui.ActionRunVerb` with `Action.Argv []string`; `cli.buildHub(root *cobra.Command, mods []module.Manifest, configPresent bool) []tui.HubEntry`; `cli.runAction(ctx, deps, root, client, opts) error`

**Dispatch:** Wave 1, after Task 3 merges (both touch `execute.go`) · fresh Sonnet subagent, worktree `.claude/worktrees/hub-menu`, branch `feat/hub-menu`, branched from `origin/main` after the Task 3 merge

**Report:** `<reports-dir>/task-1.md`

**Steps, in this commit order:**
- [ ] Commit 1 `fix(cli): group parents refuse stray tokens` — `Args: cobra.NoArgs` on the three parents; table test in `execute_test.go` that walks `root.Commands()` and fails on any parent with `len(Commands())>0`, a `RunE`, no `Args`, and not in an explicit allowlist (`launch`); Go test that `tmux frobnicate` returns cobra's unknown-command error, never a Bubble Tea TTY error
- [ ] Commit 2 `feat(tui): bare forgectl opens a hub over every module; tmux jumper stays one row` — `buildHub` table test (fake root with both tiers, config present and absent: row order per Architecture, `N` in the all-commands label, `NeedsArgs` on `pr <ref>`); `shouldLaunchTUI` table test (`[]` true; `frobnicate`, `tmux frobnicate`, `lauch` false); esc test (`menuMode` + esc → `hubMode`, no `tea.Quit`; `hubMode` + esc → quit); `ActionRunVerb` re-enters the pipeline (test: a hub-selected `launch which` and a typed `launch which` produce the same argv path); `NeedsArgs` leaf prints `$ forgectl pr <ref>` and returns nil
- [ ] Commit 3 `feat(cli): group root help by tier; root Long names the menu` — groups; `Long`; unknown-command tail text; test asserts the tail string in `renderStructuredTerminalError`
- [ ] Commit 4 `docs(adr): ADR-0005 addendum — the hub menu; README module count`
- [ ] `go build ./... && go vet ./... && go test ./... && golangci-lint run --new-from-rev=origin/main` all exit 0
- [ ] `go build -o forgectl .`; probes recorded in the PR body: bare invoke headless exits 1 with usage on stderr and 0 bytes stdout; `frobnicate` exits 1 naming the unknown command with no suggestion block; `lauch` prints `Did you mean this?` with `launch`; `tmux frobnicate` prints cobra's error, no `Bubbletea`; `quarantine restor` prints cobra's error and touches no file
- [ ] Push; draft PR `Closes #479`, body carries `## Measured` and one `BEGIN_COMMIT_OVERRIDE` block describing the bare-invoke and unknown-verb changes
- [ ] run `cadence-forge:polish`; fold findings

---

### Task 3 — Errors keep their paths; `env check` documents itself (#481)

**Files:**
- Modify: `internal/cli/execute.go:290-297` (`termsafeErrorHandler`) and `:303-320` (`renderStructuredTerminalError`): when the message's first word contains `/` or starts with `.`, render through `styles.ErrorText.UnsetTransform()`; otherwise unchanged. Both handlers, one shared predicate `leadsWithPath(msg string) bool`
- Modify: `internal/cli/execute.go`: `silentCodedError{code int}` — carries an exit code and renders nothing; `termsafeErrorHandler` returns early on it
- Modify: `internal/cli/env.go:203, :366, :382, :478` (`env file %s not found`, `example file %s not found`; `%s`, never `%q`, so the path is byte-identical)
- Modify: `internal/cli/env.go` `check`: under `--json`, exit 2 writes exactly one object `{"error":"env file not found","code":"file_not_found","path":"<relative to the resolved repo root>"}` to stderr via `silentCodedError`, stdout stays empty. The path is **never** the resolved absolute path: `envpkg.Locate` → `ResolveTarget` (`internal/env/locate.go:65-116`) absolutizes and runs `EvalSymlinks`, so it can name a directory the caller never typed, and `--json` output lands in agent transcripts verbatim. The human `%s not found` text uses the same repo-relative form so the two surfaces cannot drift (security seat ruling). Exit 0 and exit 1 keep today's shape (verdict on stdout; stderr empty under `--json`). Only `:366`/`:382` are inside `newEnvCheckCmd`; `:203` and `:478` get the wording change only
- Modify: `internal/cli/env.go` `check` `Long`, appended verbatim: `Exit codes: 0 the file matches the example · 1 keys are missing or extra · 2 the file or the example was not found` (the wording `doctor` uses at `internal/cli/doctor.go:56`)
- Modify: `internal/cli/config_cmd.go:451`: `config file: %s (not found — using defaults; run forgectl init to create one)`
- Test: `internal/cli/execute_test.go` (a path-leading error renders its path byte-identical through both handlers; a prose error keeps its capital and period; `silentCodedError` renders nothing), `internal/cli/env_test.go` (missing file + `--json`: stderr parses as one JSON object with no ESC byte, stdout empty, exit 2; drift + `--json`: stdout verdict, stderr empty, exit 1)

**Interfaces:** none

**Dispatch:** Wave 0 · fresh Sonnet subagent, worktree `.claude/worktrees/error-paths`, branch `fix/error-paths`

**Report:** `<reports-dir>/task-3.md`

**Steps:**
- [ ] Tests above RED, then GREEN
- [ ] `go build ./... && go vet ./... && go test ./internal/cli/...`
- [ ] `go build -o forgectl .`; probe in a temp repo with no `.env`: `env keys` prints the path in its original case (recorded in PR body)
- [ ] Commit: `fix(cli): errors keep paths as written; env check documents exit codes; config names init` — push, draft PR `Closes #481`
- [ ] run `cadence-forge:polish`; fold findings

---

### Task 4 — Test suite ignores nested worktrees (#480)

**Files:**
- Modify: `.gitignore` (`.claude/worktrees/`)
- Modify: `theme_literals_test.go:97` and `deps_single_lipgloss_test.go:101-105`: skip any directory whose name starts with `.` (covers `.claude`, `.git`), plus `testdata`, `node_modules`, `dist` where already listed

**Interfaces:** none

**Dispatch:** Wave 0 · fresh Sonnet subagent, worktree `.claude/worktrees/test-worktrees`, branch `fix/test-skips-worktrees`

**Report:** `<reports-dir>/task-4.md`

**Steps:**
- [ ] Reproduce from the primary checkout (read-only): `go test . -run 'TestNoColorLiteralsOutsideTheme|TestSingleLipgloss'` FAILS with `.claude/worktrees/` lines in the output
- [ ] Fix in the worktree; postcondition from the primary checkout: the same command exits 0 and its output contains zero `.claude/` lines (assert the postcondition, not a before/after count: other sessions add and prune worktrees)
- [ ] `git check-ignore -v .claude/worktrees/x` names the tracked `.gitignore` line
- [ ] Commit: `fix(test): tree-walking tests skip dot-directories; ignore nested worktrees` — push, draft PR `Closes #480`
- [ ] run `cadence-forge:polish`

---

### Task 5 — `--json` on the six verbs the skill presents as a family (#482, partial)

**Files:**
- Modify: `internal/cli/launch_which.go:16` (wire struct `launchWhichJSON` with lowercase tags: `directory, config, matched, harness, model, effort, permission_mode, allow_danger, env_keys []string, add_dir`; **`Env` values are never emitted**, matching `printLaunchProfile`'s key-names-only rule at `:83-92`)
- Modify: `internal/cli/pr.go:232` (`pr list`; `pr_prs.go:63` already has `--json`, do not duplicate), `internal/cli/tmux_ls.go`, `internal/cli/workflow.go` (`list`, `status`, `verify`)
- Test: per verb, a table test asserting (a) the exact top-level key set, (b) rows match the human table's rows, (c) a failing run leaves stdout empty, (d) empty collections encode as `[]`, never `null`. `launch which --json` additionally asserts no value from `Profile.Env` appears in the output
- Flag help per verb states the shape the way `env.go:420` does (`emit {"missing":[...],"extra":[...]} to stdout`)

**Interfaces:** none

**Dispatch:** Wave 0 · fresh Sonnet subagent, worktree `.claude/worktrees/json-verbs`, branch `feat/json-state-verbs`

**Report:** `<reports-dir>/task-5.md`

**Steps:**
- [ ] Read `internal/cli/json_encoder_wiring_test.go` first; every emitter goes through the sanctioned seam
- [ ] Per verb: test RED, implement, GREEN. JSON to stdout only; under `--json` nothing on stderr on success
- [ ] `go build -o forgectl .`; probe: `./forgectl launch which --json` parses, `.model` is `"opus"` on this machine, and `grep -c ANTHROPIC` over the output is 0 (recorded in PR body)
- [ ] Commit: `feat(cli): --json on launch which, pr list, tmux ls, workflow list, status, and verify` — push, draft PR. Body: `Part of #482; the remaining verbs stay tracked there` (no closing verb)
- [ ] run `cadence-forge:polish`; fold findings

---

### Task 6 — `docs list` under a deadline, bounded output (#483)

Premise note: the hang was observed twice on 2026-09-09 by the agent-experience seat and did not reproduce for the red-team seat the same day (0.32s, 199 KB, no cloud-backed root configured). This task is hardening with a unit-test gate, not a reproduction-gated fix.

**Files:**
- Modify: `internal/docs/index.go`: new `NewIndexContext(ctx context.Context, paths []string, opts IndexOptions) (*Index, error)`; `NewIndex` and `NewIndexWithOptions` (`:207`, `:216`) delegate with `context.Background()` so the ~35 existing callers and `docs_serve.go:61` do not change. The walk (`index.go:418`) checks `ctx.Err()` on each directory entry and returns `fmt.Errorf("docs index: walk of %s stopped: %w", root, ctx.Err())`
- Modify: `internal/cli/docs_list.go:20-36`: `--timeout` (duration, default `15s`, help `walk deadline, e.g. 15s or 2m`); `context.WithTimeout` around `NewIndexContext`; on deadline exit 2, and under `--json` stdout stays empty with one `{"error":…,"code":2,"root":…}` object on stderr via Task 3's `silentCodedError` (this task waits for Task 3's merge only if it needs that type; otherwise it defines its own minimal equivalent and Task 1's polish reconciles)
- Modify: `internal/cli/docs_list.go`: after 2s with no output, one stderr line `indexing <root> …` naming the root being walked
- Modify: `internal/cli/docs_list.go`: `--limit` (int, default 0, help `stop after N entries (0 or unset: no limit)`); `--json` keeps its array shape

**Interfaces:** none

**Dispatch:** Wave 0 · fresh Sonnet subagent, worktree `.claude/worktrees/docs-deadline`, branch `feat/docs-list-deadline`

**Report:** `<reports-dir>/task-6.md`

**Steps:**
- [ ] Test: canceled context → error names the root and wraps `context.Canceled`; RED then GREEN
- [ ] Test: `--limit 3` prints three rows in both shapes; `--json` still parses with `jq -e 'type=="array"'`
- [ ] Test: deadline under `--json` → stdout empty, stderr one JSON object, exit 2
- [ ] `go build -o forgectl .`; probe: `./forgectl docs list --json --limit 3` parses and has 3 entries (recorded in PR body)
- [ ] Commit: `feat(docs): docs list walks under a deadline and can be bounded` — push, draft PR `Closes #483`, body notes the premise did not reproduce for one seat
- [ ] run `cadence-forge:polish`; fold findings

---

### Task 7 — `using-forgectl` skill catches up to 0.18.0 (#482 skill half) — separate session

**Repo:** `~/Projects/cadence-ecosystem/cadence` (cadence monorepo). Cross-repo work runs as its own session, never a second worker under this plan (a teammate's worktree registration pins the whole session's write sandbox, cadence-ecosystem#500).

**Files:**
- Modify: `plugins/cadence-forge/skills/using-forgectl/SKILL.md`:
  - `description:` gains the Vikunja task board and its MCP server, indexed local docs across configured roots (`forgectl docs`), and resuming a past session
  - table rows for `tasks` (note: pass `--limit` on `docs list`) and `docs`
  - line 34 reworded: three of the four contract clauses hold without checking; the `--json` clause must be checked per verb
  - line 37 replaced: `--json is not universal — most list and status verbs have it, some do not; confirm on the leaf (forgectl workflow status --help), never from forgectl version`
  - `pr` **keeps** its place in the deferred stanza at `:55`, with one added clause that `pr list --json` exists
- Regenerate: `python3 scripts/generate-skill-graph.py`, `python3 scripts/generate-llms-txt.py`
- Changelog: `plugins/cadence-forge/CHANGELOG.md` `[Unreleased]`, one line

**Dispatch:** Own session after the forgectl PRs merge, so the skill describes shipped behavior. Skill markdown is code: polish applies.

**Steps:**
- [ ] Edit; regen; `git diff --stat` shows only the skill, the two generated files, and the changelog
- [ ] Commit: `docs(cadence-forge): using-forgectl names tasks, docs, resume, and the --json gaps` — push, draft PR. Body: `Relates to cameronsjo/forgectl#482` (no closing verb)
- [ ] run `cadence-forge:polish`; fold findings

---

## Verification (after all forgectl PRs merge and a release cuts)

1. `brew upgrade forgectl`; `forgectl --version` shows the new tag.
2. In a real terminal: `forgectl` opens the hub; `tmux` row opens the six-item screen; `esc` there returns to the hub; selecting `launch → which` prints `$ forgectl launch which` and the profile table.
3. With `config.toml` moved aside, the hub's row 1 is the first-run row; restore the file.
4. Selecting `all commands` opens a filterable list; typing `/env` narrows it.
5. `forgectl lauch` in a terminal prints the unknown-command block with `launch` suggested and the menu pointer; no menu opens.
6. `forgectl env keys` in a repo without `.env` prints the path in its original case.
7. `go test ./...` from the primary checkout with worktrees present: exit 0.
8. `forgectl launch which --json | jq .model` → `"opus"`; the output contains no env values.
9. `forgectl docs list --json --limit 3 | jq length` → `3`.

## Deviations

*(empty at approval)*

## Learnings

*(empty at approval)*
