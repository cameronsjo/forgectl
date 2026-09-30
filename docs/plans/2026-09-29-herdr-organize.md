---
status: in-flight
next: "Merged as cameronsjo/forgectl#729. After a forgectl release ships the command, retire forgectl-herdr and its rules file in ~/.dotfiles"
branch: plan/herdr-roadmap
pr: cameronsjo/forgectl#721
updated: 2026-09-30
approved_session_id: a86d73e4-2c61-43bb-a377-9455a4c62f53
date: 2026-09-29
session_id: a86d73e4-2c61-43bb-a377-9455a4c62f53
model: claude-sonnet-5-5
harness: claude-code 2.1.285
machine: cf6e768835c7
---

# forgectl herdr organize (roadmap item 5)

Implementation ships on `feat/herdr-organize`, stacked on cameronsjo/forgectl#723 (the `internal/herdr` client) until that merges.

## Goal

Replace the `forgectl-herdr` script, a personal external command that answers `forgectl herdr organize`, with a native, tested command: group herdr tabs into workspaces by config rules, order tabs by wing/repo/cwd, and keep the caller's focus. The script's behavior is the spec; differences are listed under Intended changes. Findings from forgectl#720 and the client work are the constraints.

## Alternatives declined

- **Keep the script** — outside forgectl's tests, config, and docs, and cannot share the client's error model or fixtures.
- **`recipe organize` / `surface organize`** — `recipe` is for small built-ins and `surface` requires an explicit backend; decided at roadmap time. A `herdr` group also hosts `inbox` and `jump`.
- **A separate `rules.toml`** — a second config location outside `forgectl config` and `forgectl init`.
- **One large task** — the pure planner is where the bugs were, so it ships and is mutation-tested before any command exists.
- **Planning tab order from a post-move snapshot** — a dry run has none. The planner computes the target layout instead (below).

## Panel

Panel: 3 seats ran (2 plan-reviewer lenses, user-experience-reviewer) — ~57 findings, ~55 folded in, 2 declined (see Panel review — findings declined)

## Panel review — findings declined

- **Distinct exit codes for "herdr declined" versus "herdr failed"** — one code for both is fine for people; add distinct codes only if a script caller asks.
- **`forgectl doctor` flagging a leftover legacy rules file** — a good idea, but a separate change to `doctor`; recorded as a follow-up, not built here.

## Architecture

### Types (package `internal/herdr/organize`, pure, no I/O)

- `Snapshot{Workspaces []herdr.Workspace, Tabs map[workspaceID][]herdr.Tab, Panes []herdr.Pane}`: one read of the session, built by the command.
- `Config{Default string, WorkspaceOrder []string, Rules []Rule}`, `Rule{Glob, Workspace string}`.
- `Move{TerminalID, TabID, Title, CWD, From, To string, Blocked bool, BlockedReason string}`: `TerminalID` is the tab's identifying pane (below); `TabID` is valid only at plan time.
- `Layout{Workspaces []LayoutWorkspace}`, `LayoutWorkspace{Label string, Tabs []LayoutTab}`, `LayoutTab{TerminalID, Title string}`: the desired final arrangement, ids-free.
- `Plan{Moves []Move, Layout Layout, Unmatched []Unmatched, RuleHits []int, Warnings []string}`.

### Behavior

1. **The tab's identity and keys.** A tab is identified by the `terminal_id` of its FIRST pane in `pane list` order; a tab in `tab list` with no panes is skipped with a warning. Match key: `"<Pane.CWD> :: <Pane.TerminalTitleStripped>"`, with `""` for a null. Classification walks the tab's panes in order and the first pane matching any rule decides; no match sends the tab to `Default`, and the sort pane is the matching pane, else the first. Match uses the RAW cwd. The sort key strips any `/.claude/worktrees/…` suffix.
2. **Glob.** `Match(glob, s)` has Python `fnmatchcase` semantics: `*` matches `/`, `?`, `[a-z]`, `[!x]`, an unclosed `[` is a literal, regexp metacharacters are escaped, case-sensitive.
3. **Sort.** (wing, repo, cwd, tab id compared as a STRING, as the script does, so `t10` sorts before `t9`). Wing and repo are the first two path parts under the projects root, checked with `filepath.Rel` and a `..` test (so `~/Projects2/x` is outside); a tab outside the root sorts under `~`. For host-tree repos filed as `<root>/<host>/<owner>/<name>`, wing and repo are host and owner; that is intended. The projects root comes from the existing resolver (`$PROJECTS_DIR`, else `~/Projects`) in `internal/projects`; Task 2 reuses it, and if it cannot be called without a git-backed client, a preparatory commit extracts a small exported root function with its own tests.
4. **Workspaces.** A rule's `Workspace` label maps to the EXISTING workspace with that label; with duplicates, the lowest `Number` wins and a warning names the others. A label with no workspace is created. `WorkspaceOrder` labels absent from the session are skipped; existing workspaces not listed keep their relative order after the listed ones; a label repeated in `WorkspaceOrder` is a validation error. `MoveWorkspace` takes a 0-based index equal to the label's position among the listed labels that are present.
5. **The planner computes the TARGET LAYOUT, not id-keyed index moves.** `Plan` assigns every tab to its workspace, then sorts each workspace's tabs; that ordered list of `terminal_id`s is `Layout`. Nothing in `Plan` depends on ids that a move will renumber.
6. **A workspace's last tab cannot leave it** (herdr answers `last_tab_in_workspace`, measured). The planner marks such a move `Blocked`, with the reason in the user's words, and never attempts it. A tab that would be the only one left is therefore a known limit, not a failure.

### Dry run (default)

Prints the plan and stops. Moves are exact. The reorder count is exact only when no moves are pending; with moves pending it says `tab order will be rechecked after the moves`. It needs a herdr session but NOT the fork: only list calls.

### Apply

1. Gate: session check, then the fork check (`tab move`), both before any change.
2. Lock: `config.WithFileLock` on a DEDICATED path, `<config dir>/herdr-organize`, so it does not contend with config writers; a non-blocking attempt runs first and prints `waiting for another forgectl herdr organize (Ctrl-C to cancel)` to stderr if it must wait. (A small `LOCK_NB` addition to `internal/config/lock_unix.go`; `lock_other.go` stays a no-op.)
3. Snapshot focus: the focused pane's `terminal_id` and, per workspace, its active tab's first-pane `terminal_id`.
4. Moves: for each non-blocked `Move`, re-list panes and resolve the tab by `terminal_id` immediately before moving (its id may have changed). Create-versus-join belongs to this loop, not the planner: a missing label uses `ToNewWorkspace`, later tabs for it use `ToWorkspace` with the id `MoveResult` returned; if the create is declined or fails, the next tab for that label becomes the creator, and the failure is reported.
5. Workspace order: `MoveWorkspace` per the rules above.
6. Tab order: for each workspace, re-list, then for each position compare the current order to `Layout`; issue an index move by `terminal_id` (resolved to the current tab id) only where it differs, using the tab list `MoveResult` returns to know the new order, and re-listing when the result is not trusted (unmatched length).
7. Focus restore, in a `defer` so the error path restores too: each workspace's active tab first (resolving each `terminal_id` through a fresh `pane list`; skip one that is gone), the caller's focused terminal LAST. Tab grain only: there is no `FocusPane`, so a focused pane inside a split tab is not restored to pane grain. A failed restore is reported as `could not restore focus: <err>` and joined with, never masking, the run's own error.

### Errors and exit codes

- 0: done, or a dry run, including predicted-blocked moves (reported, not counted).
- 1: an unexpected `*herdr.Declined`, or any herdr error mid-apply.
- 2: not in a herdr pane, no rules configured, an invalid config, the probe cannot run herdr, or a usage error; set with the existing `WithExitCode(err, 2)` helper in `internal/cli`.
- The config check runs BEFORE any herdr call, so a first-time user outside herdr with no config sees both problems in one run.
- A run that fails mid-apply prints four lines: which command failed and why; `applied: N of M moves; not run: …`; `focus restored to "<title>" [<id>]`; `the plan is recomputed on every run; re-run … --apply to finish`.

## Tech Stack

Go per `go.mod`, cobra, `internal/herdr` (forgectl#723), `internal/config`, `internal/exec`, `internal/projects`. No new dependencies.

## Global Constraints

- **Config:** `[herdr.organize]` in forgectl's `config.toml`: `default` (label), `workspace_order` (labels), `[[herdr.organize.rule]]` with `glob` and `workspace`. Rules present with an empty `default` is a validation error naming the key. Errors name the rule by 1-based position and glob: `[[herdr.organize.rule]] #3 (glob "…"): workspace is empty`. The command calls the section's `Validate()` itself (`config.Load` is tolerant and `ValidatePath` is only doctor's). The `config.Config` value converts to `organize.Config` in `internal/cli`; `internal/config` does not import the organize package.
- **No rules configured:** exit 2 with a message that says what was found. Variants (each tested): no legacy file: `forgectl init adds a commented [herdr.organize] section; edit it (forgectl config shows the path)`; legacy file at `~/.config/herdr-organize/rules.toml` found: `forgectl no longer reads that file`, plus the mapping `default`, `workspace_order`, and `[[rule]]` becoming `[[herdr.organize.rule]]` under `[herdr.organize]`; `HERDR_ORGANIZE_RULES` set: say it is ignored. Once `[herdr.organize]` exists from `forgectl init`, the message points at editing it and never tells the user to run `init` again (`init` reports it already present).
- **Module wiring in one commit:** the config section, the module (`herdrModule`, Tier Extension, ConfigKey `herdr`), `internal/cli/modules_test.go` pins (count 32, `wantNames`), the `initSections` entry with an ACTIVE `[herdr.organize]` header and commented keys, `docs/configuration.md`, `docs/commands/herdr.md`, a line-leading `forgectl herdr` usage line in `README.md` (`TestModules_DocumentedInREADME` rejects a passing mention), and `config_cmd_test.go` in the verify list (`TestConfig_PrintsEverySection` needs a `[herdr]` header).
- **Help:** a `Long` in `clean.go`'s style: what it does in plain words, the wing/repo/cwd order and where the projects root comes from, that nothing is closed or renamed, the rule syntax, the fork and pane requirements, and that `--apply` restores focus and asks no confirmation because moves are reversible. `forgectl herdr` with no subcommand lists `organize`.
- **Output** (human text, stdout; under `--json`, human text goes to stderr and stdout carries ONE object): each tab as `"<title, ≤30 cols>" [<id>]  <cwd with ~>`. States: `organized: N tabs in M workspaces; nothing to do`; `move …`; `blocked  "<title>" [id]  a -> b: it is the only tab in a, and herdr will not empty a workspace` with `fix: open another tab in a, or move it by hand`; `order <workspace>: "<title>" [id] -> position N`; an always-on `unmatched` line (`N tabs matched no rule and go to "<default>": …  (--explain shows why)`); a closing line that matches what is pending (`re-run with --apply to move them`, or `… to reorder them`); never `organized` while anything is pending. `--explain` prints every tab with its rule, current and target workspace, and the match key for unmatched ones, then a rule summary (`rule 3 "<glob>" -> forge: 0 tabs`) and a warning for a rule workspace missing from `workspace_order`.
- **`--json`** (ADR-0008): `{"plan":{"moves":[…],"layout":[…],"unmatched":[…],"warnings":[…]},"result":{"applied":[…],"blocked":[…],"not_run":[…],"error":""}}`, including on exit 1; a golden test pins the field names.
- **Changelog:** do not edit `CHANGELOG.md` (release-please-owned, CI-guarded); the `feat(herdr):` subject is the record.
- **Fork dependency** on `tab move` is inherited from the client; the refusal message says `herdr has no "tab move", which organize --apply needs (it is in the cameronsjo/herdr fork, not upstream herdr); see docs/commands/herdr.md#requirements`.
- Never rename a tab. Conventional commits; merge, not rebase; a polish pass before the PR.

### Intended changes from the script

The dry run works on stock herdr; blocked moves are predicted instead of attempted; `unmatched` always prints and `--explain` adds a rule summary; ids in messages come with titles; failures print the four-line summary; the lock is dedicated and announces a wait; the config lives in forgectl's `config.toml`, so the old rules file and `HERDR_ORGANIZE_RULES` are no longer read. Once a release ships, the native `herdr` group shadows `forgectl-herdr` for every verb; the script has only `organize`, so nothing else is lost.

## Orchestrator

**Driver:** Sonnet — spec'd; the planner is pure and table-testable; no security-posture trigger.

---

## Tasks

### Task 1 — pure planner

**Files:**
- Create: `internal/herdr/organize/glob.go`, `classify.go`, `plan.go`, `sort.go`, `types.go`
- Test: the matching `_test.go` files

**Interfaces:**
- Consumes: `herdr.Pane`, `herdr.Tab`, `herdr.Workspace`
- Produces: the types above; `Match`, `Classify`, `BuildPlan(cfg, snapshot, projectsRoot) Plan`

**Dispatch:** In-context · Sonnet — defines the interfaces

**Report:** —

**Steps:**
- [x] Failing tests: glob table (every case in Behavior 2); classify (first matching pane wins in pane order, default, null cwd matches on `" :: title"`, the raw worktree cwd is what a glob sees); sort (wing, repo, cwd, string tie-break where `t10` precedes `t9`, `~Projects2` is outside the root, host-tree repos, a tab with no cwd sorts under `~`); BuildPlan (already organized is empty; two tabs into one missing workspace; duplicate labels pick the lowest `Number` with a warning; a `workspace_order` label absent from the session is skipped; a sole-tab move is `Blocked`; a tab with no panes is skipped with a warning; `Layout` is independent of ids, and building it from a snapshot after renumbering gives the same order)
- [x] Run `go test ./internal/herdr/organize/` — expect RED; implement; run — expect GREEN
- [x] Mutation sweep: force `Match` true, drop the worktree strip in the sort key, compare tab ids numerically, drop the `Blocked` prediction, ignore `Number` for duplicates; each turns a named test red
- [x] Commit: `feat(herdr): organize planner`

### Task 2 — config, module, dry run

**Files:**
- Create: `internal/cli/herdr.go`, `docs/commands/herdr.md`
- Modify: `internal/config/config.go`, `internal/cli/modules.go`, `internal/cli/modules_test.go`, `internal/cli/init_cmd.go`, `internal/herdr/probe.go` (split out `CheckSession` from `Probe`), `README.md`, `docs/configuration.md`
- Test: `internal/config/herdr_test.go`, `internal/cli/herdr_test.go`, `internal/cli/config_cmd_test.go`

**Interfaces:**
- Consumes: Task 1, `herdr.Client`, `herdr.CheckSession`
- Produces: `forgectl herdr organize` (dry run, `--explain`, `--json`); package-level injectable env lookup, stat, and runner seams in `herdr.go` (as `recipe.go` does with `lookupRecipeEnv`), so CLI tests need no real socket

**Dispatch:** Serial (after Task 1) · Sonnet — one command

**Report:** —

**Steps:**
- [x] Read how `[theme]` and `[[launch.project]]` are declared, validated, scaffolded, and shown; list every touch point in the commit message
- [x] Failing tests: decode a rules block; each validation error message (position, glob); rules with no default; the three no-rules message variants; a `[herdr.organize]` from `init` with no rules points at editing, never at `init`; not in a herdr pane → exit 2; a dry run makes ZERO mutating herdr calls and NO `tab move --help` call; `--explain` output including the rule summary and the missing-`workspace_order` warning; each output state; `--json` matches a golden file (a `{}` would fail); the config check comes before any herdr call
- [x] Run — expect RED; implement; register the module with its pins; run `go test ./...` and `golangci-lint run ./...` — expect GREEN
- [x] Commit: `feat(herdr): forgectl herdr organize (dry run)`

### Task 3 — apply

**Files:**
- Modify: `internal/cli/herdr.go`, `internal/config/lock_unix.go` (a non-blocking attempt and a wait notice)
- Test: `internal/cli/herdr_apply_test.go`, `internal/config/lock_test.go`

**Interfaces:**
- Consumes: Task 2, the client's mutations, `config.WithFileLock`
- Produces: `--apply`

**Dispatch:** Serial (after Task 2) · Sonnet — same file

**Report:** —

**Steps:**
- [x] Failing tests using a `FakeRunner` whose `RunFunc` closes over a small state machine (a call counter and a mutable layout, since `RunFunc` is stateless by design) that returns POST-move ids on later lists: call order is gate → lock → focus snapshot → moves → workspace order → tab order → focus restore; a target is re-resolved by `terminal_id` right before its move; an unexpected `*Declined` exits 1 and still restores focus; a herdr error on the second of three moves stops, restores focus, and prints the four-line summary; a declined create makes the next tab the creator; focus restore order is background workspaces first and the caller's terminal last; a restore failure is reported without masking the run's error; the lock: a goroutine holds it and a second run prints the wait notice and blocks until a deadline (unix-only)
- [x] Run — expect RED; implement; run — expect GREEN
- [x] Mutation sweep: delete the focus restore, skip the re-resolve, ignore `*Declined`, restore the caller's terminal before the workspaces; each turns a named test red
- [x] Commit: `feat(herdr): organize --apply`

### Task 4 — parity, live check, retirement

**Files:** none

**Interfaces:**
- Consumes: Tasks 1-3
- Produces: parity evidence; the dotfiles change

**Dispatch:** In-context · Sonnet — needs the live session

**Report:** —

**Steps:**
- [x] Parity, read-only: run `forgectl-herdr organize --explain` DIRECTLY (a registered native command wins over the external one, so `forgectl herdr organize` would reach the new code) with the operator's real `HOME` and rules, then the dev build with `HOME` pointed at a temp dir that holds a forgectl `config.toml` built from the same rules and `PROJECTS_DIR` pinned to the real projects root. Compare, per workspace, the ordered list of `terminal_id`s. Record the measured result in `## Learnings`
- [x] Live `--apply`: first make the plan provably small. Create a throwaway workspace with two tabs, misplace one tab by giving it a cwd a rule sends elsewhere, run the dry run, and confirm the plan lists ONLY that tab's move (the rest of the session is already organized by the script); then `--apply`, verify layout and focus, and close only what was created
- [ ] After a forgectl release ships the command: a separate `~/.dotfiles` change removes `forgectl-herdr` and its rules file. Not part of this PR
- [x] run a pre-PR polish pass over the diff; fold findings

---

## Deviations

- **Prep commit for the projects root.** `internal/projects` gained an exported `ResolveRoot` (its own commit, with a test), as the plan allowed. `New` now calls it; behavior is unchanged.
- **The planner grew an order half.** `OrderSteps`, `Reorders`, `WorkspaceOrderChange`, `ArrangeTarget`, and `CheckGlob` live in `internal/herdr/organize`, because the dry run must count tab and workspace reorders exactly and apply must plan them the same way. `Move` carries `FromWorkspaceID` and `ToWorkspaceID`, and `Plan` carries `Assignments` for `--explain`.
- **Apply re-reads before every step** rather than trusting the tab list `MoveResult` returns: each cross-workspace move, each workspace move, and each tab-order step is preceded by a fresh list. Slower on a large session (tracked in cameronsjo/forgectl#732), but no step acts on a stale id.
- **Any declined move continues the run.** The plan sent an unexpected `*Declined` to exit 1. It still exits 1, but the run now finishes the moves and stages that do not depend on the refused one, because a decline after the snapshot means the session changed, not that the rest is unsafe.
- **Config checks beyond the plan.** An uncompilable glob, and a workspace label that starts with `-` or holds a control character, are config errors (exit 2), because otherwise the dry run promises a move that apply then fails halfway through.
- **Live check needed pinned rules.** The plan assumed the session was already organized. It was not (four tabs out of place), so the throwaway check used a config that pins every real tab to its current workspace, plus rules for the throwaway workspaces. The plan then still held three tab reorders of real tabs, which Cameron approved.
- **Review triage.** The eight-angle review's candidates were triaged by reading the code rather than by the per-candidate verifier stage, because many were duplicates across angles. Each fix has a test that fails without it; the rest went to cameronsjo/forgectl#732.

## Learnings

- **Parity (read-only).** `forgectl-herdr organize --explain` and the dev build's `--json` agree, per target workspace, on the ordered terminal ids (cadence 5 tabs, homelab 4, misc 2, tooling 2), and both plan the same four moves.
- **Live apply.** In two throwaway workspaces, `--apply` made exactly one cross-workspace move (creating the second workspace), applied four reorders (three real tabs, one throwaway), left the focused terminal, every real tab's workspace, the workspace order, and every workspace's active tab unchanged, and a second dry run then planned nothing. Both throwaway workspaces were closed afterward.
- **`--no-focus` works.** `herdr workspace create` and `tab create` with `--no-focus` did not move the caller's focus.
- **A fake that renumbers is worth its size.** The stateful fake herdr in `internal/cli/herdr_world_test.go` (renumbering ids, `renumberAll`, an interceptor for declines and errors) is what let the stale-id, decline, and focus tests fail for the right reason.
- **A workspace label reached the terminal unescaped through planner-built text.** The security pass found the label in `BlockedReason`; the fix escapes at the sink. Text the planner builds from herdr values is untrusted at the renderer.
