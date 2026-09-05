---
body_sha256: "b35dd520f25448a80f92626a53eca59f62d12f019210085bfefeeefecb308793"
session: "brisk-garden"
session_id: "0e0e5f58-9ca8-4d51-bb3e-1d79a02636ae"
model: "claude-fable-5-1"
harness: "claude-code 2.1.261"
machine: "cf6e768835c7"
approved_in: "cedar-chisel"
approved_session_id: "4b1b4f96-d8eb-4faa-a96b-9bf46558cc02"
status: in-progress
next: "PR 1 (#454) is ready, all checks green, MERGEABLE/CLEAN — awaiting Cameron's merge. Then PR 2a theme core → PR 2b call sites → PR 3 hub. Re-read modules_test.go wantCount at PR 2a branch time: it was 28 when PR 1 branched and main has since gained the recipe module."
branch: chore/charm-v2
pr: https://github.com/cameronsjo/forgectl/pull/454
updated: 2026-09-05
date: 2026-09-05
---

# forgectl TUI — one lipgloss, an Artificer theme for the whole binary, and a hub menu

## Context

forgectl's bare-invoke TUI (`internal/tui`) exposes six tmux actions and nothing else, while the CLI has 28 command groups of which several prompt a human (`projects pick`, `pr pick`, `resume`, `launch`, `doctor`, `pr dash`, `bench status`, plus confirms). ADR-0005 ruled the menu "a tmux session jumper, not a command palette" and deferred a `Manifest.Menu` hook "until a second module actually wants one" — nine do now.

Color is three layers deep and none of them is the design system. The founding plan (`docs/plans/2026-05-28-forgectl.md:33`) says "Artificer design system palette" and then specifies lavender `#B0B9F9` plus the Tomorrow/gitmux set; `internal/tui/styles.go:15-34` implements those hexes faithfully. Artificer reaches the binary only as web assets for `docs serve`. Five `internal/cli` files bypass both with raw 256-color numbers (`launch_doctor.go:17`, `launch_which.go:17`, `pr_dash.go:18`, `pr_prs.go:23`, `doctor.go:19`), `internal/pr/launch.go:572` hardcodes `huh.ThemeCharm()`, every other huh picker runs the unstyled default, `internal/k8s/logs.go:292` writes raw ANSI, and fang's `--help`/errors/version are never themed.

Underneath: forgectl links **two lipgloss majors** — `github.com/charmbracelet/lipgloss v1.1.0` (TUI, huh, bubbles) and `charm.land/lipgloss/v2` (fang v1.0.0). Cameron ruled this the miss: collapse to one, don't bridge. The v2 line is GA (`lipgloss/v2 v2.0.6`, `bubbletea/v2 v2.0.9`, `bubbles/v2 v2.2.1`, `huh/v2 v2.0.3`; go directives ≤ 1.25.8, repo `go 1.26.0`; all confirmed on the proxy by the red-team seat).

**Palette source (panel finding, Cameron's call):** Artificer's *terminal* palette is `themes/_palette.json` in the artificer-design-system repo — the source of truth behind its ghostty, tmux, gitmux, cmux, herdr, gum, glamour, and lazygit targets — and it disagrees with the web `tokens.json` (`brandPurple` `#9070d0` vs `#5a3a9a`). forgectl generates from `_palette.json`, fetched at a pinned tag, so it shows the same colors as the tmux status bar it sits beside. The themes directory is not on the npm package, so the fetch is GitHub raw at the pinned ref, never the sibling checkout.

Cameron's rulings this session: Artificer by default **with user config overrides**; theme reaches the **whole binary**; the menu exposes a **curated set of human-interactive verbs**; hub top level stays the **flat seven**.

## Goal

forgectl links exactly one lipgloss (v2); every colored surface — TUI, huh pickers and confirms, doctor/bench marks, fang help/errors, k8s log severity — draws from one `internal/theme` generated from Artificer's terminal palette and overridable in `config.toml`; bare `forgectl` opens a seven-row hub where modules contribute their own human-interactive entries through a `Manifest.Menu` hook. Four PRs, in order.

## Alternatives declined

- **Bridge v1 and v2 lipgloss inside `internal/theme`** — Cameron: "that's a miss"; two majors of one library in one binary is the defect.
- **Generate from the web `tokens.json`** — it is the web palette; `_palette.json` is the terminal one and already drives the tmux/gitmux/ghostty colors forgectl sits beside.
- **Add forgectl as a `themes/build.mjs` target in artificer** — canonical, but cross-repo work plus a delivery channel (themes are not on npm); a pinned raw fetch gets the same bytes today.
- **Keep the lavender palette, only fix stray call sites** — smaller, still off-system; the founding plan's stated intent was Artificer.
- **Theme the TUI and forms only** — leaves `--help`, pickers, and doctor output in default charm colors.
- **Full command palette (all 28 groups)** — worst on a 40-column phone; most verbs need argv or are agent-shaped.
- **Keep the tmux jumper, retheme only** — nine modules are human-interactive and unreachable from thumb mode.
- **Fold Sessions + Windows into one group** — frees a top row at the cost of one tap on the thumb-mode primary; Cameron kept the flat seven.
- **Second `sharedSections` exception for `[theme]`** — `sharedSections` has one member by design; a module buys `theme preview`/`show`, the verification tool.
- **Hand-copied palette table** — drifts silently on every vendor bump.
- **Re-open the menu after a plain-output verb / capture output into the viewport** — alt-screen re-entry wipes the result; capture blocks on network (`pr dash` → `gh`). Thumb mode is "do the thing, show the result".
- **`ansi` 16-color preset** — no named consumer; lipgloss's writer downgrades to 16 colors on its own. A `legacy` preset (today's lavender set) ships instead as the migration escape.

## Panel

Panel: plan-reviewer ×2 + red-team + cameron-review + audience-pragmatist + audience-agent ran — 60 findings, 56 folded in, 4 declined (see Panel review — findings declined)

## Panel review — findings declined

- **[audience-agent] fang's own background probe runs on piped stdout** — refuted by the red team's probe: `fang@v1.0.0/theme.go:116` gates it on `term.IsTerminal(os.Stdout.Fd())`; a piped `--help` never queries. A test pinning that stays (Task 7).
- **[cameron-review] Cut per-role `[theme.colors]` overrides to a `debt:` marker** — Cameron ruled "default to Artificer tokens, but use user config too" at attune; the override map is that ruling.
- **[plan-reviewer] Rename the hub row `Sessions` (collides with the `sessions` module)** — the row is tmux vocabulary for thumb mode, where it has meant tmux sessions since 2026-05; the `sessions` module is agent-shaped and never in the menu. Noted in `docs/commands/hub.md`.
- **[cameron-review] forgectl as `themes/build.mjs` target 27** — Cameron picked the pinned fetch of `_palette.json`; the target variant needs an artificer-side channel first (recorded as a follow-up issue on artificer-design-system, Task 4).

## Architecture

Four PRs on four branches, strictly ordered. Each from a fresh `origin/main` worktree after the previous merges. Open PRs #427 (`feat/docs-tui-reader`: `go.mod`, `config.go`, `init_cmd.go`) and #439 (`feature/herdr-afk-recipe`: `modules_test.go` count 28 → 29) overlap this plan's files; every pin below is read at branch time, never copied from here.

**PR 1 — `chore/charm-v2` (behavior-neutral, proven not asserted).** Every charm v1 import (`internal/tui/{tui,items,styles}.go`, `internal/keymap/keymap.go`, `internal/cli/{confirm,doctor,launch_doctor,launch_which,pr_dash,pr_pick,pr_prs,projects_pick,resume}.go`, `internal/pr/launch.go`, three tests) moves to `charm.land/{lipgloss,bubbletea,bubbles,huh}/v2`. Colors stay byte-identical as v2 `lipgloss.Color(hex)`. Two regressions v2 would otherwise ship are closed **in this PR**: (a) v2 `Style.Render` never honors `NO_COLOR` or pipes (`lipgloss/v2 writer.go:14` moved the downgrade into the writer), so the five cli print sites write through a `colorprofile.NewWriter(cmd.OutOrStdout(), os.Environ())` shim in `internal/cli/colorout.go`; (b) huh v2's form dark flag defaults `false` and standalone `Form.Run()` never requests the background (`huh/v2 form.go:91,509-524,644`), so every `NewForm` pins `huh.ThemeFunc(func(bool) *huh.Styles { return huh.ThemeCharm(true) })`. First command in the worktree proves fang v1.0.0 compiles and renders against lipgloss/v2 v2.0.6 (fang pins a Nov-2025 beta; MVS lifts it; the compile is the unproven part). `go mod tidy` drops lipgloss v1, bubbletea v1, bubbles v1, huh v1, `muesli/termenv`. A root test shells `go list -m all` and pins one lipgloss.

**PR 2a — `feat/theme` (core, config, wiring, `theme` module; call sites untouched).** `internal/theme` (leaf: imports `config`, never `module`/`cli`/`tui`). `internal/theme/palettegen` (importable; `cmd/main.go` thin wrapper) renders `internal/theme/artificer_gen.go` from `internal/theme/artificer/_palette.json`, which `scripts/vendor-artificer.sh` fetches from `https://raw.githubusercontent.com/cameronsjo/artificer-design-system/<pinned-tag>/themes/_palette.json` alongside the npm vendor (same pin, one `--check`). `Theme` resolves dark/light at construction (`New(Options, isDark)`, `WithDark(bool) Theme`) and every accessor is argument-free: `Styles()`, `Marks()`, `Huh() huh.Theme`, `List() list.Styles`, `Fang() fang.ColorSchemeFunc`, `Writer(w io.Writer, env []string) io.Writer`. `Detect(mode Mode, env Env, probe func() bool) bool`: `dark|light` fixed; `auto` → dark for plain prints (no probe on the fast path — fang probes on its own for TTY help anyway), probe only where a Bubble Tea program or huh form is about to render and `env.StdinTTY && env.StdoutTTY && !env.NoColor && TERM ∉ {screen*, tmux*}`. The TUI issues `tea.RequestBackgroundColor` under the same predicate and rebuilds via `WithDark` on `BackgroundColorMsg`, so list, rows, and huh forms always agree. `config.ThemeConfig` (`[theme] preset = artificer|legacy`, `mode = auto|dark|light`, `[theme.colors] role = "#rrggbb" | {dark, light}`), validated in `ValidatePath`; `[theme.colors]` is exempt from `config`'s map redaction (hex is not a secret). `module.Deps.Theme` filled in `productionDeps`. New `theme` extension module: `theme preview` (swatch sheet, dark and light columns, huh confirm sample, fang snippet) and `theme show [--json]` (resolved hex per role, provenance preset/override/mode, AA contrast flag per role against `bg`, `warnings[]` for an ignored `[theme]`).

Role map (`_palette.json` key → role): `accent`→Accent (header, selected, section titles) · `success`→OK · `urgentText`→Danger (bare `urgent` fails AA as text) · `attention`→Warn, Active · `fgMuted`→Muted · `fgSecondary`→Meta · `fgDisabled`→Dim · `fg`→Fg · `steel`→Steel (session names, paths, flags) · `brandPurpleBright`→Brand (forge glyph only) · `bgRaised`→SurfaceRaised · `accentFill`→AccentFill · `onAccent`→OnAccent · `urgentFill`→UrgentFill · `onUrgent`→OnUrgent. `RoleNames()` order is this list. **Two-color rule** applies to chrome; status marks (`✓ ! ✗ -`) and log severity are exempt because glyph + text carry the state.

**PR 2b — `refactor/theme-callsites`.** TUI, cli, k8s call sites move onto `deps.Theme`; the PR 1 shims retire into `theme.Writer`; the root enforcement test lands in the same PR as the last literal it forbids.

**PR 3 — `feat/tui-hub`.** New leaf `internal/menu` (keymap precedent): `Entry{Label, Desc, Glyph, Section, Args, Screen}`; `Section` order is a fixed list in `internal/menu` (`""`, `Reviews`, `More`), never registry order. `module.Manifest.Menu func(Deps) []menu.Entry`; nil for scripting modules — the curation is structural. TUI hub renders section `""` rows then one row per section; `groupMode`; `Screen` entries drive the existing tmux screens; `Args` entries set `Action{Kind: ActionRunVerb, Args}`, quit, and `dispatchAction` runs `execCommand(ctx, root, args)` on the freed tty (a nil root returns an error, tested). `Args` rows carry an exit affordance (glyph `↗` / ASCII `>`) and the footer says `↗ runs after the menu closes`. `forgectl tmux` bare gets a tmux-only hub. Every level ≤ 7 rows, pinned. Top-level pin = count 7 + first row `Pick`; exact order is a golden that a deliberate edit may change. ADR-0009 supersedes the ADR-0005 alternative and names all three key moves: `4` Tree → `More › 3`, `5` Last → `More › 2`, `6` Cheatsheet → `More › 4`; the new `4`/`5` (Projects, Resume) open cancelable pickers, never a mutation.

```text
40 col                        80 col
 forgectl · menu               forgectl · menu
▌1  Pick                      ▌1  Pick        connect or smart-create (sesh)
 2  Sessions                   2  Sessions    attach · rename · kill
 3  Windows                    3  Windows     jump to any window, any session
 4  Projects ↗                 4  Projects ↗  open a project in tmux (clones if needed)
 5  Resume ↗                   5  Resume ↗    pick a recent Claude Code session
 6  Reviews ›                  6  Reviews ›   pr pick · dash · work inventory
 7  More ›                     7  More ›      launch · doctor · bench · tmux extras
 1-7 / enter · ↗ runs after the menu closes · q quit
```

`Reviews ›` (5): Pick PRs ↗ (`pr pick`) · Dashboard ↗ (`pr dash`) · Work inventory ↗ (`review`) · Active reviews ↗ (`pr list`) · Review keys ↗ (`pr keys`). `More ›` (7): Launch ↗ (`launch`, static desc "start the harness for the current directory") · Last · Tree · tmux cheatsheet · Doctor ↗ · Bench ↗ (`bench status`) · Update check ↗ (`update check`). Never in the menu, with reasons in `docs/commands/hub.md`: flags that skip a confirm (`--apply`/`--yes`/`--force`); browser-opening verbs (`bench open`, `docs open`, `docs serve` — the browser is on the server); verbs needing argv (`clean`, `k8s`, `pr <ref>`); agent/scripting modules (env, pip, proxy, sessions, surface, workflow, quarantine, y, net, config, init, preflight, upgrade, docker, version, ghostty, branch).

## Tech Stack

- Go 1.26.0 (module `github.com/cameronsjo/forgectl`); cobra; `github.com/charmbracelet/fang v1.0.0` (stays; newest release)
- Target: `charm.land/lipgloss/v2 v2.0.6`, `charm.land/bubbletea/v2 v2.0.9`, `charm.land/bubbles/v2 v2.2.1`, `charm.land/huh/v2 v2.0.3`; `github.com/charmbracelet/colorprofile` becomes direct; `github.com/charmbracelet/x/ansi` stays (`resume.go:14`); huh v2 brings indirect `x/exp/ordered`, `x/xpty`, `x/conpty`, `x/errors`, `creack/pty`, `catppuccin/go` (MIT)
- Palette: `themes/_palette.json` from `cameronsjo/artificer-design-system` at the tag `scripts/vendor-artificer.sh` pins; web assets stay on the npm vendor
- Tests: stdlib; registry pins in `internal/cli/modules_test.go`; root policy tests (`release_workflow_security_test.go` pattern)
- release-please owns `CHANGELOG.md`; release prose via `BEGIN_COMMIT_OVERRIDE` in each PR body (`AGENTS.md`)

## Global Constraints

- **One lipgloss.** After PR 1, `go list -m all` shows exactly one `charm.land/lipgloss/v2` and zero `github.com/charmbracelet/{lipgloss,bubbletea,bubbles,huh}` or `muesli/termenv`; no import of `charm.land/lipgloss/v2/compat`. Pinned by a root test that shells `go list -m all`.
- **Never `tea.KeyMsg` in a type switch** — it is an interface matching press and release; match `tea.KeyPressMsg` or keys double-fire on kitty-protocol terminals.
- **No raw color at call sites** (from PR 2b): outside `internal/theme`, no `lipgloss.Color(`, no `huh.Theme*`, no `\x1b[` string literal. Root test.
- **Every plain-command colored print goes through a colorprofile writer** (PR 1 shim, then `theme.Writer`). Bubble Tea and fang handle their own output. A table test runs every plain-output verb with stdout piped and asserts zero ESC bytes.
- **`mode = auto` never probes on the plain-print path**; probes only before a Bubble Tea program or huh form, and never under `TERM=screen*|tmux*`, `NO_COLOR`, or a non-TTY. Inside tmux everything renders dark; `docs/configuration.md` records that a light terminal inside tmux needs `mode = "light"`.
- **Thumb mode holds:** 40-column layout, number keys 1–9, `--no-icons` and `NO_COLOR` glyph fallback, every hub level ≤ 7 rows, fixed shape regardless of cwd (no `os.Getwd` at hub build).
- **Untrusted text boundary:** every row or status carrying text forgectl did not compose goes through `termsafe.SafeLine`; `TestScreensDrawNothingUnsafe` covers every new screen and row.
- **Artificer rules:** max two semantic colors on one screen of chrome (brand purple + gold count as one); status marks and log severity exempt (glyph + text); `theme preview` is a swatch sheet and says so.
- **Destructive verbs never reach the menu with a flag that skips their confirm.**
- **Module registry discipline (ADR-0005):** a new module's `modules_test.go` pins (`wantCount` = current + 1, `wantNames` + `"theme"`, read at branch time), `initSections`, and README `## Usage` fence line land in the same PR.
- **No new direct dependencies** beyond the four charm.land v2 modules and `colorprofile` becoming direct.
- **Worktree-first, four PRs**: fresh `origin/main` worktree each, `push -u` + draft PR at entry. Only the orchestrator commits, explicit pathspecs, producer-tuple trailers; tuple in the PR body (squash-merge). `cadence:redaction` over each PR body.
- **Never edit `CHANGELOG.md`**; `go build ./... && go vet ./... && go test ./...` green before every ready flip.

## Orchestrator

**Driver:** opus — trigger: a cross-cutting dependency-line migration with API-break judgment across 17 files (key-event double-fire, huh's light default, `Render` no longer downgrading), plus a design-system mapping where a wrong call ships to every screen. Tasks are Sonnet-dispatched where spec'd.

---

## Tasks

> Reports dir: orchestrator `mktemp -d` outside the worktree at dispatch time; each dispatched task writes `<reports-dir>/task-N.md` and replies with `wc -l` of it.

### PR 1 — `chore/charm-v2`

### Task 0 — Prove fang compiles against lipgloss/v2 GA

**Files:** none in the repo (scratch module under `mktemp -d`)

**Interfaces:** Consumes *(none)* · Produces a go/no-go for PR 1

**Dispatch:** In-context · **Report:** —

**Steps:**
- [x] Scratch module: `require github.com/charmbracelet/fang v1.0.0` + `charm.land/lipgloss/v2 v2.0.6`; a cobra root through `fang.Execute`; `go build` and run `--help` on a TTY and piped. Record output inline here at execution.
- [x] If fang fails to compile or render: **stop and report** — the chain blocks on an upstream fang release; do not start Task 1.

**Result (2026-09-05): GO.** Scratch module `/tmp/fangprobe.nuzOhZ`, `go mod tidy` clean, `go list -m` confirms `charm.land/lipgloss/v2 v2.0.6` + `github.com/charmbracelet/fang v1.0.0` in the build list. `go build` succeeded; fang's `--help` and error frames rendered correctly; a `lipgloss.Color("#d4a24c")` render emitted a truecolor SGR sequence. Piped `--help` emitted **zero** ESC bytes, corroborating the red team's reading of `fang@v1.0.0/theme.go:116` — fang's background probe is gated on `term.IsTerminal(os.Stdout.Fd())`. MVS lifts fang's own beta lipgloss/v2 pin to GA with no API break.

### Task 1 — Migrate the charm stack to v2, behavior-neutral

**Files:**
- Modify: `go.mod`, `go.sum`; `internal/tui/{tui,items,styles}.go`; `internal/keymap/keymap.go`; `internal/cli/{confirm,doctor,launch_doctor,launch_which,pr_dash,pr_pick,pr_prs,projects_pick,resume}.go`; `internal/pr/launch.go`
- Create: `internal/cli/colorout.go` (`colorOut(cmd) io.Writer` = `colorprofile.NewWriter(cmd.OutOrStdout(), os.Environ())`; retires in PR 2b)
- Test: `internal/tui/{tui,termsafe}_test.go`, `internal/cli/pr_prs_test.go`, `internal/tui/golden_test.go` (new: menu/sessions/windows render at 80 cols with a fixed fake client, golden captured **before** the swap on main and asserted after)

**Interfaces:** Consumes Task 0 go · Produces the same public behavior on `charm.land/*/v2`

**Dispatch:** In-context (Opus driver) — API-break judgment; a Sonnet pass may do the mechanical import swap first · **Report:** —

**Steps:**
- [x] Worktree `.claude/worktrees/charm-v2`, branch `chore/charm-v2` from fresh `origin/main`; `push -u`; draft PR; persist this plan to `docs/plans/2026-09-05-tui-theme-and-hub.md` and commit it first
- [x] Capture goldens on the pre-swap tree (`internal/tui/golden_test.go` with `-update`), commit them
- [x] `go get charm.land/lipgloss/v2@v2.0.6 charm.land/bubbletea/v2@v2.0.9 charm.land/bubbles/v2@v2.2.1 charm.land/huh/v2@v2.0.3`; `go build ./...` — fang must compile here (Task 0 said so)
- [x] `internal/tui/tui.go`: drop `tea.WithAltScreen()` (absent in v2); `View()` returns `tea.View` with `AltScreen = true`; `case tea.KeyMsg` → `case tea.KeyPressMsg` at `:164,196,289`; `viewport.New(0,0)` → `viewport.New()`, `Width/Height` → `SetWidth/SetHeight`; `l.Styles = list.DefaultStyles(true)` explicitly; huh forms at `:407,421` add `.WithTheme(darkCharm)` where `darkCharm = huh.ThemeFunc(func(bool) *huh.Styles { return huh.ThemeCharm(true) })` (huh v2 defaults light; `huh.ThemeCharm` is a `func(bool) *Styles`, so a bare identifier does not satisfy `huh.Theme`). Note: `Form.Update` returns an internal `compat.Model`; keep the existing `fm.(*huh.Form)` pattern at `:302-303` and never name that type
- [x] `internal/tui/styles.go:15-34`: same hexes as v2 `lipgloss.Color(...)`; no palette change
- [x] `internal/keymap/keymap.go`: import swap; re-verify the esc-during-filter side effect against huh v2 `keymap.go`
- [x] cli pickers/confirms (`confirm.go:10`, `pr_pick.go:110`, `projects_pick.go:204`, `resume.go:254`, `pr/launch.go:572`): import swap + `.WithTheme(darkCharm)` (shared var in `colorout.go`); `pr/launch.go:572` `huh.ThemeCharm()` → `darkCharm`
- [x] cli marks/dim (`launch_doctor.go:17`, `launch_which.go:17`, `pr_dash.go:18`, `pr_prs.go:23`, `doctor.go:19`): keep the styles, write through `colorOut(cmd)`; `pr_prs_test.go` `forceColor` (`lipgloss.SetColorProfile`) has no v2 equivalent — render through `colorprofile.NewWriter(buf, []string{"CLICOLOR_FORCE=1"})`
- [x] Tests: `tea.KeyMsg{Type: tea.KeyRunes, Runes: …}` → `tea.KeyPressMsg{Code: '2', Text: "2"}` / `{Code: tea.KeyEscape}`; `View()` assertions read `.Content`; goldens green
- [x] `go mod tidy`; verify `go list -m all | grep -cE 'charmbracelet/(lipgloss|bubbletea|bubbles|huh) |muesli/termenv'` = `0` (prints `5` on main today), `grep -c '^charm.land/lipgloss/v2 '` = `1`
- [x] Manual: `go run .` menu identical to the golden; `k`/`r` forms render dark; `go run . --help` renders; `NO_COLOR=1 go run . doctor` and `go run . doctor | cat` emit no escapes (now true because of `colorOut`)
- [x] `go build ./... && go vet ./... && go test ./...`; commit `chore(deps): migrate the charm stack to charm.land v2 — one lipgloss`

### Task 2 — Pin one lipgloss

**Files:** Create `deps_single_lipgloss_test.go` (repo root)

**Interfaces:** Consumes Task 1's tidied `go.mod` (precondition: zero charm v1 modules) · Produces a red test on any v1 charm module or `muesli/termenv` in the **build list** (`go list -m all`), or a `charm.land/lipgloss/v2/compat` import under `internal/**`

**Dispatch:** Serial (after Task 1's tidy) · fresh Sonnet subagent · **Report:** `<reports-dir>/task-2.md`

**Steps:**
- [x] Shell `go list -m all` from the test (skip with `t.Skip` when `go` is absent, as `release_workflow_security_test.go` does for its tools); assert exactly one `charm.land/lipgloss/v2`, zero of the five v1 modules; walk `internal/**/*.go` for the `compat` import
- [x] Run — expect GREEN; the RED demonstration is a one-time manual run against `origin/main` recorded in the PR body, not a test case
- [x] Commit `test(deps): pin a single lipgloss major`; PR body: summary, `BEGIN_COMMIT_OVERRIDE` `chore(deps): migrate to charm.land v2 (bubbletea, bubbles, huh, lipgloss); one lipgloss in the binary`, tuple; `cadence:redaction`; `cadence-forge:polish` (diff-based reviewers against the worktree diff); ready flip

### PR 2a — `feat/theme`

### Task 3 — `[theme]` config section (first: it consumes nothing)

**Files:**
- Modify: `internal/config/config.go` (`ThemeConfig`, `ColorOverride` + `UnmarshalTOML`, `Validate`, `IsZero`, `ValidatePath` wiring at `:762`), `internal/config/*_test.go`; `internal/cli/init_cmd.go` (`initSections` entry; template recommends `mode = "dark"` unconditionally and notes `mode = "light"` for a light terminal inside tmux); `internal/cli/config_cmd.go` (`[theme.colors]` exempt from map redaction at `:202`)

**Interfaces:** Consumes *(none)* · Produces `config.ThemeConfig{Preset, Mode string; Colors map[string]ColorOverride}`, `ColorOverride{Dark, Light string}`, `(ThemeConfig).Validate() error`, `IsZero() bool`, `config.ThemeRoleNames []string` (pinned equal to `theme.RoleNames()` by a test in Task 4)

**Dispatch:** Serial (wave 1 of PR 2a) · fresh Sonnet subagent · **Report:** `<reports-dir>/task-3.md`

**Steps:**
- [ ] Worktree `.claude/worktrees/theme`, branch `feat/theme` from fresh `origin/main` (after PR 1 merges); `push -u`; draft PR
- [ ] TOML: `preset = "artificer"|"legacy"`, `mode = "auto"|"dark"|"light"`, `[theme.colors] <role> = "#rrggbb" | { dark = "#..", light = "#.." }`; `^#[0-9a-fA-F]{6}$`; unknown role error lists `ThemeRoleNames`: `[theme].colors["acent"]: unknown role; roles are accent, ok, danger, …`
- [ ] `Load` stays tolerant; `Execute` warns once via `termsafe.SafeLine` and uses `Default()`; `ValidatePath` returns the error so `doctor` and `launch doctor` report a bad `[theme]` non-zero
- [ ] `TestInitSections_CoversEveryStructSection` green; `config` prints `[theme]` with real hex values
- [ ] Commit `feat(config): [theme] section — preset, mode, per-role color overrides`

### Task 4 — `internal/theme` core and the generated Artificer palette

**Files:**
- Create: `internal/theme/{theme,roles,styles,huh,list,fang,presets}.go`; `internal/theme/palettegen/{render.go,render_test.go}` (importable) + `internal/theme/palettegen/cmd/main.go`; `internal/theme/artificer/_palette.json` (fetched) ; `internal/theme/artificer_gen.go` (generated); `internal/theme/*_test.go`
- Modify: `scripts/vendor-artificer.sh` (fetch `themes/_palette.json` at the pinned tag by GitHub raw into `internal/theme/artificer/`; `--check` covers both artifacts)

**Interfaces:** Consumes Task 3 · Produces:

```go
type Role int; func RoleNames() []string // order = Architecture role map
type Pair struct{ Dark, Light string }
type Palette struct{ Name string; Roles [numRoles]Pair }
type Mode int; const ( ModeAuto Mode = iota; ModeDark; ModeLight )
type Options struct{ Preset string; Mode Mode; Overrides map[Role]Pair }
type Env struct{ StdinTTY, StdoutTTY bool; Term string; NoColor bool }
type Styles struct{ Header, Accent, Selected, OK, Warn, Danger, Active, Muted, Meta, Dim, Steel, Fg, Brand lipgloss.Style }
type Marks struct{ OK, Warn, Fail, Skip string } // pre-rendered "✓" "!" "✗" "-"
func Artificer() Palette; func Legacy() Palette
func FromConfig(c config.ThemeConfig) (Options, error)
func Detect(mode Mode, env Env, probe func() bool) bool
func ShouldProbe(mode Mode, env Env) bool // the one predicate Execute, huh sites, and the TUI share
func New(o Options, isDark bool) Theme; func Default() Theme // zero Theme == Default()
func (t Theme) WithDark(isDark bool) Theme
func (t Theme) Mode() Mode; func (t Theme) IsDark() bool
func (t Theme) Hex(r Role) string; func (t Theme) Color(r Role) color.Color; func (t Theme) Style(r Role) lipgloss.Style
func (t Theme) Styles() Styles; func (t Theme) Marks() Marks
func (t Theme) Huh() huh.Theme; func (t Theme) List() list.Styles; func (t Theme) Fang() fang.ColorSchemeFunc
func (t Theme) Writer(w io.Writer, env []string) io.Writer
func (t Theme) Contrast(r Role) float64 // vs bg, for theme show's AA flag
```

**Dispatch:** Serial (wave 2) · fresh Sonnet subagent · **Report:** `<reports-dir>/task-4.md`

**Steps:**
- [ ] `palettegen.Render(palette []byte) ([]byte, error)`: reads `_palette.json` dark/light role blocks, emits `func Artificer() Palette` with each key's comment as the role's doc comment; `go/format`; token path constant `theme.PalettePath = "artificer/_palette.json"` shared by the generator and `TestArtificerGen_IsCurrent`
- [ ] `Legacy()`: today's `styles.go` hexes (lavender set) as the migration escape
- [ ] `Huh()`: `huh.ThemeFunc` closure that ignores its `isDark` argument and uses `t.IsDark()` (huh v2 defaults light; standalone forms never request the background); from `huh.ThemeBase(t.IsDark())`: Title/selectors/prefix/prompt/cursor→Accent, Description→Meta, Unselected→Muted, Error→Danger, FocusedButton→OnAccent on AccentFill, BlurredButton→Muted on SurfaceRaised, Placeholder→Dim
- [ ] `Fang()`: ignores the passed `LightDarkFunc`, uses `t.IsDark()`: Base→Fg, Title/Program/Command→Accent, Description/FlagDefault/Help→Meta, Codeblock→SurfaceRaised, DimmedArgument/Comment/Dash→Muted, Flag/QuotedString→Steel, Argument/ErrorDetails→Fg, ErrorHeader→{OnUrgent, UrgentFill}
- [ ] `Detect`: `dark|light` fixed; `auto` → `probe()` only when `ShouldProbe`, else dark. `ShouldProbe` = `StdinTTY && StdoutTTY && !NoColor && !HasPrefix(Term,"screen") && !HasPrefix(Term,"tmux")`
- [ ] Tests: `FromConfig` table; `Detect` truth table with a counting probe (tmux → 0 calls; NO_COLOR → 0; light/dark → 0; auto TTY → 1); zero `Theme{}` equals `Default()` per method; `TestArtificerGen_IsCurrent` (re-render, byte-compare, message names `go generate ./internal/theme`); `TestRoleNames_MatchConfig`; `Fang()` fills every `ColorScheme` field (reflect); `Huh()` returns dark styles for both `Theme(true)` and `Theme(false)` when resolved dark; `Writer` with `CLICOLOR_FORCE=1` emits ESC and with `NO_COLOR=1` emits none
- [ ] File follow-up issue on `cameronsjo/artificer-design-system`: "a forgectl (Go) target in `themes/build.mjs`, or ship `_palette.json` on npm" — the pinned raw fetch is the interim
- [ ] Commit `feat(theme): Artificer terminal palette generated from _palette.json; v2 styles, huh, list, fang adapters`

### Task 5 — Wire the theme: `Deps`, `Execute`, fang, and the `theme` module

**Files:**
- Modify: `internal/module/module.go` (`Deps.Theme theme.Theme`), `internal/cli/execute.go` (`productionDeps`; resolve after `setupLogger`: `opts, err := theme.FromConfig(cfg.Theme)` → warn-and-default; `isDark := theme.Detect(opts.Mode, env, nil)` with **no probe** on this path; `fangOptions(version, commit, th)` adds `fang.WithColorSchemeFunc(th.Fang())`), `internal/cli/modules.go`, `internal/cli/modules_test.go` (`wantCount` = current + 1, `wantNames` + `"theme"` — **read at branch time**; #439 may have moved it to 29), `README.md` (`## Usage` fence line + roster row), `docs/README.md`, `fang_error_termsafe_test.go`, `version_test.go`
- Create: `internal/cli/theme_cmd.go` (`theme preview`, `theme show [--json]`), `internal/cli/theme_cmd_test.go`, `docs/commands/theme.md` (role table pinned equal to `theme.RoleNames()` by a test)

**Interfaces:** Consumes Tasks 3, 4 · Produces `deps.Theme` populated; `forgectl theme preview|show`

**Dispatch:** Serial (wave 3) · In-context · **Report:** —

**Steps:**
- [ ] `theme preview`: every role as swatch + glyph + sample row in dark and light columns, a huh confirm sample, a fang-styled snippet; Long says it breaks the two-color rule on purpose. `theme show`: hex per role, provenance (preset / override / mode / probed?), AA flag (`Contrast < 4.5` → `below AA`), `warnings[]`; `--json` emits the same as an object (pattern: `config --json`)
- [ ] Test: `theme show --json` parses and lists every `RoleNames()`; `--help | cat` writes zero `\x1b]11` bytes (pins fang's TTY gate)
- [ ] Manual: `go run . --help` in gold/steel; `go run . theme preview` under Ghostty and inside tmux; `mode = "light"` in a scratch config flips it
- [ ] Commit `feat(theme): thread the theme through Deps, fang, and a theme module (preview, show)`; PR body with `BEGIN_COMMIT_OVERRIDE` `feat(theme): Artificer palette core, [theme] config, theme preview/show`, tuple; `cadence:redaction`; `cadence-forge:polish`; ready flip

### PR 2b — `refactor/theme-callsites`

### Task 6 — Move every call site onto the theme

**Files (three disjoint slices):**
- 6a TUI: `internal/tui/{tui,items,styles,cheatsheet}.go` (delete color/style vars; `tui.Run(ctx, client, noIcons, th)`; `Init` → `tea.RequestBackgroundColor` iff `theme.ShouldProbe(th.Mode(), env)`; `case tea.BackgroundColorMsg` → `m.theme = m.theme.WithDark(msg.IsDark())`, rebuild `m.styles`, `m.l.Styles = m.theme.List()`, delegate; huh forms `.WithTheme(m.theme.Huh())`; session name→Steel, active→Active, forge glyph→Brand), `internal/cli/tmux_cheat.go`, TUI tests (goldens re-captured deliberately — this PR changes the look)
- 6b cli: `confirm(th theme.Theme, prompt string)` and `var confirmFn = confirm` retyped (`confirm.go:10,31`; fakes at `clean_test.go:325`, `update_test.go:81`); client-only constructors gain `th`: `newTmuxKillCmd(client, th)`, `newPrFindingsCleanupCmd(client, th)`, the `…ForClient` constructors in `branch.go`, `clean.go`, `update.go`; `pickPRs(th, …)`, `pickRepo(th, …)`, `pickSession(th, …)`, `confirmReview(th, …)` (`pr/launch.go:572`); marks: `newLaunchDoctorCmd(boundary, cfg, th)`, `doctorMark(m theme.Marks, s)`, `benchGlyph(m, s)`, `printLaunchProfile(w, th, …)`, `prDimStyle` → `th.Styles().Muted` at `review_list.go:146`, `pr_pick.go:142`, `resume.go:248`; prints through `th.Writer(cmd.OutOrStdout(), os.Environ())`; delete `internal/cli/colorout.go`; tests
- 6c k8s: `internal/k8s/logs.go:253,292` (`severityANSI` → `LogsOptions.Styles theme.Styles` **and** `LogsOptions.Out` wrapped by `th.Writer`; Trace→Dim, Debug→Steel, Info→OK, Warn→Warn, Error/Fatal→Danger), `internal/cli/k8s.go:310-318`, `logs_test.go`

**Interfaces:** Consumes PR 2a · Produces zero color literals outside `internal/theme`

**Dispatch:** Parallel (wave 1 of PR 2b) · three fresh Sonnet subagents, one per slice · **Report:** `<reports-dir>/task-6a.md`, `task-6b.md`, `task-6c.md`

**Steps:**
- [ ] Worktree `.claude/worktrees/theme-callsites`, branch `refactor/theme-callsites` from fresh `origin/main` (after PR 2a merges); `push -u`; draft PR
- [ ] Each slice keeps its `\x1b[`-present/absent tests meaningful by rendering through `th.Writer(buf, []string{"CLICOLOR_FORCE=1"})` / `{"NO_COLOR=1"}`
- [ ] `TestScreensDrawNothingUnsafe` and the huh-form unsafe tests green with `theme.Default()`
- [ ] Orchestrator commits one per slice: `refactor(tui|cli|k8s): draw from internal/theme`

### Task 7 — Enforcement, piped-output test, docs, ship PR 2b

**Files:**
- Create: `theme_literals_test.go` (root), `internal/cli/piped_output_test.go`
- Modify: `docs/configuration.md` (`[theme]`; the tmux-renders-dark trade-off), `docs/artificer-adaptations.md` (A5: terminal role map, `urgentText` over `urgent`, `_palette.json` over `tokens.json`)

**Interfaces:** Consumes Task 6 · **Dispatch:** Serial (wave 2) · In-context · **Report:** —

**Steps:**
- [ ] Enforcement test: `go/parser` over `main.go` + `internal/**/*.go`, skip `_test.go`, `internal/theme/`, `internal/termsafe/termsafetest/`, `.claude/`; flag `ast.BasicLit` STRING containing `\x1b[`/`\033[`, selectors `lipgloss.Color`, `huh.Theme*`, imports of `charm.land/lipgloss/v2/compat` or any charm v1 path; message names file:line and "use internal/theme". RED demonstration is a one-time manual run against `origin/main`, recorded in the PR body
- [ ] `piped_output_test.go`: table over every plain-output verb reachable with a `FakeRunner` (`doctor`, `launch which`, `launch doctor`, `pr dash`, `pr prs`, `review`, `bench status`, `tmux ls`, `theme show`); stdout = buffer with env `NO_COLOR=1`; assert zero `\x1b` bytes
- [ ] Manual acceptance (dogfood): the TUI in a tmux pane under Ghostty `artificer-dark`, beside the Artificer tmux status bar and gitmux — accent, steel, and danger read as the same colors. Record the verdict in the PR body
- [ ] Invoke `artificer-design-system:artificer-feedback` (background)
- [ ] `go build ./... && go vet ./... && go test ./...`; PR body with `BEGIN_COMMIT_OVERRIDE` `feat(theme): every colored surface draws from the Artificer palette; NO_COLOR and pipes stay plain`, tuple; `cadence:redaction`; `cadence-forge:polish`; ready flip

### PR 3 — `feat/tui-hub`

### Task 8 — `internal/menu` leaf, `Manifest.Menu`, glyph map

**Files:**
- Create: `internal/menu/menu.go` (`Entry`, `Glyph` vocabulary, `Screen` constants, `Sections = []string{"", "Reviews", "More"}`, `MaxRows = 7`), `internal/menu/menu_test.go`
- Modify: `internal/module/module.go` (`Menu func(Deps) []menu.Entry` after `Steps`), `internal/tui/styles.go` (glyph struct → `map[menu.Glyph]string` with `For(k)` falling back to generic `` / `•` / ASCII `-`; keep Forge/Attached/Detached/Pane/Kill/Rename keys; add `Exit` = `↗` / `>`), `internal/tui/{cheatsheet,keybinds}.go`, tests

**Interfaces:** Consumes *(none)* · Produces `menu.Entry{Label, Desc, Glyph, Section, Args []string, Screen}` (Label ≤ 12 chars, trusted literal; Desc may be untrusted, `SafeLine`d by the TUI; Args and Screen mutually exclusive); `Manifest.Menu`

**Dispatch:** Serial (wave 1 of PR 3) · fresh Sonnet subagent · **Report:** `<reports-dir>/task-8.md`

**Steps:**
- [ ] Worktree `.claude/worktrees/tui-hub`, branch `feat/tui-hub` from fresh `origin/main` (after PR 2b merges); `push -u`; draft PR
- [ ] New glyphs: project (folder), resume (history), review (pull-request), launch (rocket), health (heartbeat), more (ellipsis), exit (`↗`); `TestGlyphFallbackForUnknownKey`; `TestKeybindSheet_NoIcons_UsesASCIIGlyph` green
- [ ] Commit `feat(menu): Manifest.Menu hook and the menu entry contract`

### Task 9 — Entry-driven hub in the TUI

**Files:** Modify `internal/tui/tui.go` (`Run(ctx, client, hub []menu.Entry, noIcons, th)`; hub rows from section `""` then one row per `menu.Sections` entry present; `groupMode`; `activate` switches on the selected entry; `ActionRunVerb` + `Action.Args`; `Args` rows render `Label + " " + glyph.Exit`; footer adds `↗ runs after the menu closes` when any Args row is visible), `internal/tui/items.go` (`menuItem` carries `menu.Entry`; `Desc` through `SafeLine`; drop the "menuItem is exempt" comment), tests

**Interfaces:** Consumes Task 8 · Produces `tui.ActionRunVerb`, `Action.Args`

**Dispatch:** Serial (wave 2) · fresh Sonnet subagent · **Report:** `<reports-dir>/task-9.md`

**Steps:**
- [ ] `fixtureHub()` in tests: tmux screens + one `Args` entry + one `Reviews` group; update `TestMenuViewRenders`, `TestNumberKeyNavigatesAndAttaches`, `TestCheatFromMenu` (`7` then `4`), `TestLastFromMenu` (`7` then `2`)
- [ ] Add `TestHubRenders40And80Cols` (narrow drops Desc; labels fit; exit glyph and footer line present), `TestNumberKeyRunsDeferredVerb`, `TestGroupRowDrillsAndEscReturns`, `TestEveryLevelAtMostSevenRows`; `TestScreensDrawNothingUnsafe` gains `{"hub",""}` and `{"more","7"}` with a hostile `Desc` fixture
- [ ] Commit `feat(tui): entry-driven hub with section drill-down and deferred verbs`

### Task 10 — Build the hub from the registry; dispatch deferred verbs

**Files:**
- Create: `internal/cli/menu.go` (`buildHub(deps) []menu.Entry` over `allModules()`, sorted by `menu.Sections` then contribution order; `tmuxOnlyHub()`), `internal/cli/menu_test.go` (fixture-hub assertions only in this task)
- Modify: `internal/cli/execute.go` (`runAction(ctx, client, root, hub, noIcons, th)`; `dispatchAction` `case tui.ActionRunVerb`: nil root → `errors.New("menu verb needs the command tree")`; else `logDispatch` + `execCommand(ctx, root, args)` appending `--no-icons` when `cfg.NoIcons || hasNoIcons(args)`; `var openHub = runAction` seam beside `buildRoot`), `internal/cli/tmux.go:50-53` (tmux-only hub, nil root), `execute_test.go`, `dispatch_test.go`

**Interfaces:** Consumes Task 9 · Produces bare `forgectl` → hub; `forgectl tmux` → tmux-only menu; `routeHeadlessMenu` unchanged

**Dispatch:** Serial (wave 3) · In-context · **Report:** —

**Steps:**
- [ ] `TestDispatchAction` gains `ActionRunVerb` against a recording leaf **and** the nil-root error path; `TestExecute_HeadlessNeverOpensHub` (bare args, `isInteractiveTTY=false`, `openHub` fail sentinel → `errHeadlessMenuRoute`); `TestDecideRoute` byte-identical
- [ ] Commit `feat(cli): registry-built hub; deferred verbs run on the freed tty`

### Task 11 — Module entries and the hub pins

**Files:** Modify `internal/cli/{tmux,projects,resume,pr,review,launch,doctor,bench,update}.go` (each manifest's `Menu`; `launch` desc is the static string — no `os.Getwd`); `internal/cli/menu_test.go` (the real-hub pins)

**Interfaces:** Consumes Task 10 · Produces the hub content in § Architecture

**Dispatch:** Serial (wave 4) · fresh Sonnet subagent · **Report:** `<reports-dir>/task-11.md`

**Steps:**
- [ ] Entries per Architecture (`More ›` = Launch, Last, Tree, tmux cheatsheet, Doctor, Bench, Update check — exactly 7)
- [ ] Pins: `TestHub_EntriesResolveToRegisteredCommands` (walk `findChild` token by token skipping flags; require `Runnable()`; reject root and the `tmux` parent), `TestHub_ScreensOnlyFromTmux`, `TestHub_TopLevel` (count 7, first row `Pick`) plus a golden of the exact order (a deliberate edit re-captures it), `TestHub_EveryLevelAtMostSeven`, `TestHub_NoDestructiveFlags` (`--apply|--yes|--force`), `TestHub_GlyphKeysExist`, `TestHub_NoCwdDependence` (build twice from two cwds, equal)
- [ ] Commit `feat(menu): tmux, projects, resume, pr, review, launch, doctor, bench, update contribute hub entries`

### Task 12 — ADR-0009, docs, ship PR 3

**Files:**
- Create: `docs/adr/0009-hub-menu-manifest-entries-deferred-verbs.md`, `docs/commands/hub.md`
- Modify: `docs/adr/0005-module-architecture.md:124-126` (+ "Superseded by ADR-0009") and `:142-143` (+ "Implemented — see ADR-0009"), `docs/adr/README.md` (row 0009, 4-column table), `README.md:8,63,311` (hub wording; 10-line "Hub" subsection under Usage; `--help` root Long gains one line: an unknown verb on a TTY opens the hub, whose rows run real verbs), `docs/README.md`

**Interfaces:** Consumes Task 11 · **Dispatch:** Parallel (wave 5) · fresh Haiku subagent for docs; ADR drafted in-context · **Report:** `<reports-dir>/task-12.md`

**Steps:**
- [ ] ADR-0009: Context (six tmux rows; nine modules human-interactive; ADR-0008 already TTY-gates them) · Decision (`Manifest.Menu`, `internal/menu` leaf, deferred `ActionRunVerb` through `execCommand`, tmux keeps in-TUI screens, fixed shape, ≤ 7 rows per level, scripting modules return nil, exit affordance) · Alternatives (viewport capture — declined on network blocking; re-open loop; hide-by-cwd; hard-coded list) · Consequences (pins; `tmux` bare gets a tmux-only menu; **key moves**: `4` Tree → `More › 3`, `5` Last → `More › 2`, `6` Cheatsheet → `More › 4`; new `4`/`5` open cancelable pickers)
- [ ] `docs/commands/hub.md`: rows, groups, the never-list with reasons, the `Sessions` row means tmux sessions, the three key moves
- [ ] `go build ./... && go vet ./... && go test ./...`; PR body with `BEGIN_COMMIT_OVERRIDE` `feat(tui): hub menu — modules contribute human-interactive entries; Tree/Last/Cheatsheet move under More` and the key-move note, tuple; `cadence:redaction`; run `cadence-forge:polish`; fold findings; ready flip

---

## Verification (end to end)

- PR 1: `go list -m all | grep -cE 'charmbracelet/(lipgloss|bubbletea|bubbles|huh) |muesli/termenv'` = `0` (main prints `5` today); golden renders unchanged; `NO_COLOR=1 go run . doctor | grep -c $'\x1b'` = `0`; pickers render dark
- PR 2a: `go run . theme preview` shows gold/steel/brick/apothecary in dark, ink-on-ivory in light; `go run . --help` themed; `go run . theme show --json | jq '.roles | length'` = `15`; `go run . --help | cat | grep -c $'\x1b]11'` = `0`
- PR 2b: enforcement test green; piped-output table green; TUI opens inside tmux with no pause; side-by-side with the tmux status bar reads as one palette
- PR 3: `go run .` shows seven rows at 40 and 80 cols with `↗` on Projects/Resume; `4` opens `projects pick` on the freed tty; `7` then `1` execs `launch`; `go run . tmux` shows the tmux-only menu; `go run . < /dev/null; echo $?` = `1` (fang prints usage; the `errHeadlessMenuRoute` text is swallowed by design — check the exit code, never the message)

## Deviations

- **[Task 1] `darkCharm` lives in `internal/keymap`, not duplicated into `internal/tui` and `internal/cli`.** The plan placed the huh dark-theme pin as a shared var in `internal/cli/colorout.go`, which leaves `internal/tui` needing its own copy (it cannot import `internal/cli`). `internal/keymap` is already the leaf both import for exactly this reason, and its own doc comment argues against hand-maintained copies of one huh literal. Shipped as `keymap.DarkCharm()`; `internal/pr` uses it too, replacing the `huh.ThemeCharm()` call at `launch.go:572`. Retires into `theme.Huh()` in PR 2b as planned.
- **[Task 1] Goldens were re-captured after the swap, with the neutrality proven separately.** The plan had them captured before and asserted unchanged after. lipgloss v2 emits `ESC[m` where v1 emitted the equivalent `ESC[0m`, so a byte-identical assertion was never going to hold. A throwaway script normalised only that spelling and required an exact match on all three goldens — verified able to go red by perturbing one palette hex first — then the goldens were re-captured under v2. The script was deleted; its verdict is in the PR body.
- **[Task 1] `internal/cli/review_list.go` also needed `colorOut`.** The plan listed five plain-print sites; `review_list.go:146` uses `prDimStyle` through its own `renderReviewTable` and is a sixth.
- **[Task 1, unplanned] `termsafetest.AssertInert` had to be taught the difference between forgectl's styling and an injected sequence.** The shared inertness contract rejected every ESC, which was equivalent to "reject every ESC a value contributed" only because lipgloss v1 rendered plain inside a test binary. Under v2 it fired on forgectl's own colours across seven tests. Rather than blanket-stripping SGR — which would have silently retired the colour-injection arm — it now allows appearance-only SGR (colour, bold, italic, underline, reverse video for huh's cursor) and still rejects cursor moves, erases, OSC, C1, bidi, blink, conceal, and malformed sequences. A single table asserts both arms so the exemption cannot widen unnoticed. Offsets and hex in the failure message still index the original bytes.
- **[Task 1, unplanned] Two hand-rolled ESC assertions were replaced by the shared contract.** `keybinds_test.go` scanned for five specific bytes (narrower than the shared contract, and blind to C1 and bidi); `doctor_test.go` asserted no ESC anywhere. Both were proxies that held only because of v1's test-binary rendering. The keybind sheet now asserts with icons off, matching every other inertness test in that package — Nerd Font glyphs are private-use runes and so are not graphic.
- **[Task 7 → PR 1] The piped-output enforcement moved forward from PR 2b.** The plan scheduled `piped_output_test.go` for Task 7. Polish found the gap was not hypothetical: the migration missed **three** styled commands — `ghostty cheat` (117 escapes piped and under `NO_COLOR`), `tmux cheat` (23), and `bench status` (2, reached through a helper). PR 1 therefore ships `internal/cli/colorout_test.go`, an AST guard asserting that a raw `cmd.OutOrStdout()` never receives styled text — directly, through a local variable, or via a package function that styles (resolved transitively, including package-level pre-rendered vars like `launchOKMark`). PR 2b still owns the behavioural table over plain-output verbs; this is the structural half, and it is what caught `bench status`.
- **[Task 1, unplanned] `AssertInert` now rejects invalid UTF-8 before the rune scan.** A raw `0x9B` — the 8-bit CSI introducer, which a Latin-1 terminal acts on exactly like `ESC[` — decodes to `U+FFFD` under `range`, and `unicode.IsGraphic(U+FFFD)` is true, so it was accepted. Pre-existing, surfaced by the security arm.
- **[PR 1] `main` was merged into the branch mid-flight, and had to be.** Release-please cut `0.17.0` (#452) after this branch was created. CI's `check-changelog-owner` compares the *merge* commit against the PR's original base SHA, so main's own `CHANGELOG.md` edit was attributed to this PR and `lint` went red on a branch that never touched the file. Merging `main` up cleared it; the underlying check defect is filed as forgectl#458. The merge also brought in a new `internal/cli/recipe.go`, which the new AST guard now covers.
- **[Task 2] The RED demonstration ran against `origin/main`'s extracted `go.mod`/`go.sum` rather than a checkout of main.** The enforce-worktree guard refuses `git worktree add` targeting the shared checkout from an isolated session. `git show origin/main:go.mod` into a scratch module reproduces the same build list, and the test named all five retired modules plus the 2-lipgloss count.

## Learnings

- **A test that "asserts no escape sequences" is measuring the colour profile, not the code.** Seven tests in this repo passed for years on a proxy: lipgloss v1 resolved its profile from a global that a test binary left unset, so styled output arrived plain and "no ESC" happened to mean "no ESC from an untrusted value". Moving the downgrade to the writer separated the two and the proxy fired on forgectl's own colours. The general shape: when a check's subject can be rendered inert by the environment it runs in, the check may be asserting the environment.
- **The fix for a control that fires on legitimate output is an allowlist of what the code emits, not a strip of the class it belongs to.** Blanket-stripping SGR would have made every one of those tests pass while retiring the colour-injection arm — a green that could no longer go red, produced by a change described as a test fix. Asserting the accepted and rejected cases in one table is what keeps the exemption honest.
- **`go.mod`'s direct block cannot answer "how many majors of X do we link".** Both lipglosses were present for months; only the build list showed it, because the second arrived indirectly through fang.
- **A per-call-site convention is not an invariant, and the PR that introduces one is where it first breaks.** `colorOut` was applied to six sites and missed three, in the same change that added it — because nothing at an `internal/cli` call site looks styled when the styling lives inside `internal/tui` or behind a pre-rendered package var. The lesson is not "be more careful": it is that a rule which cannot be checked mechanically will be violated by the commit that writes it down. ADR-0008 had stated the rule since before this branch and had no mechanism.
- **A guard written to commemorate a bug should be tested against the bug's actual shape.** The first version of the AST guard matched only a bare `cmd.OutOrStdout()` as the first argument of `fmt.Fprint*` — which caught two of the three sites and would have missed the pre-diff shape of `pr_dash.go` entirely. The security reviewer caught the overclaim in its doc comment. The rewritten version resolves local writer variables and follows the call graph, and it found `bench status` on its own.
- **huh v2 asks for the window size but never the background colour.** Any standalone `Form.Run()` therefore resolves to light styles regardless of the terminal. Nothing fails, nothing warns, and the only symptom is that the prompt looks wrong.
