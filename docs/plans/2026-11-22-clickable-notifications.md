---
status: in-flight
next: "Step 0 blocked on Cameron: allow terminal-notifier notifications, then click the two probe banners."
approved_session_id: d97a8686-dc8c-47ce-af84-c537175b00b4
branch: feat/clickable-notifications
---

# Desk UX deep dive, clickable notifications, notch surface — routing and the notifier fix

## Context

Three asks from Cameron, 2026-11-22:

1. A **Fable** deep dive on the UI/UX of `forgectl desk`.
2. **Fix macOS notifications**: clicking one opens a file-open dialog.
3. A **notch / "Dynamic Island"-style surface** on the MacBook for forgectl notifications, because on a widescreen monitor top-right banners go unseen, especially ones that need action.

Root cause of #2, confirmed from code: `internal/notify/notify.go:27` posts through
`osascript -e 'display notification …'`. macOS attributes those banners to
**Script Editor**, so a click launches Script Editor, which shows its open-file
panel. No osascript form can change the click target; the sender has to be a
different app.

## Routing (today is an even date, so this session continues)

| Ask | Where | Model | Why |
|---|---|---|---|
| 1. Desk UX deep dive | **new herdr tab**, interactive session | Fable | He asked for Fable by name, and a deep dive is a conversation, not a report this Opus session should relay |
| 2. Notifier fix | **this session** | Opus 5.5 | Root cause is known; bounded code change in `internal/notify` + callers |
| 3. Notch surface | **new herdr tab**, research-first | Opus | Unknown territory (native macOS app, private notch APIs, existing projects to adopt) |

Launch: `herdr-app do tab.new --name <slug> --folder <repo root> --argv '["zsh","-ic","claude-as alt -- \"$@\"","zsh"]' --task "<brief>"`
(this session's `CLAUDE_CONFIG_DIR` is `~/.claude-alt`; the Fable tab adds `--model fable`). Confirm each brief landed with `hc read <terminal>`.

- **Tab `desk-ux-fable` brief:** run `cadence:using-fable`, then deep-dive desk UI/UX with Cameron: render every desk view under tmux (memory: tmux, not pyte; fixture HOME; never press Enter), walk the artificer-design-system + building-cli-tools TUI checklist, produce a ranked findings doc in `docs/research/` and file the agreed items as `cameronsjo/forgectl` issues. Note the notifier fix is in flight in another session (don't touch `internal/notify`).
- **Tab `notch-surface` brief:** `cadence:attune` → research workflow. Survey adopt-vs-build (open-source notch apps with a plugin/extension or URL-scheme/IPC surface vs a small Swift app), how forgectl would feed it (the `internal/notify` sink interface this session adds), the "be mindful of notifications" policy (which signals earn the notch), and a plan with a `## Loop` for notch items. Decide whether it supersedes the banner path or sits beside it.

## Task 2 plan: clickable notifications

### Step 0 — prove the mechanism before any code

`brew install terminal-notifier`, then by hand: `terminal-notifier -title t -message m -group probe -execute 'herdr-app do tab.select …'` and `-activate <herdr-app bundle id>`. Click it on this Mac (Darwin 27). Then `terminal-notifier -remove probe`. **If the click does nothing, stop**: report back and fold #2 into #3's native app instead of building on a dead sender.

### Approach (recommended): `terminal-notifier` as the sender, osascript as fallback

1. **Sender chain in `internal/notify`.** Add a lookup seam (`WithLookPath`, default `exec.LookPath`) so tests never see the host's binary. When `terminal-notifier` resolves, send through it via `exec.Runner`; otherwise keep today's osascript path, unchanged. `Supported()` stays darwin-only. Error and status strings name the sender that ran (`notify.go` error wrap, `desk_signal_unix.go:166`, `surface_drain_notify.go:213-216,280`).
2. **API.** `Notify(ctx, Message)` with `Message{Title, Body, Group, Target}`; a thin `Notify(title, body)` wrapper is not kept — every caller moves. Seams that change and their rigs: `deskMacNotify` (`signalRig`), `sendMacSignal` (`signal.go`), `needsYouNotifier.Alert` (`notifyRig`), `resume_hooks.go` notifier. No sender interface for #3 yet (YAGNI until #3's verdict).
3. **Click target = a herdr tab.** herdr has no pane focus by id (`internal/herdr/mutate.go:167`: `FocusTab` is the finest grain). New hidden verb `forgectl notify open --tab <id>`: validates the id, calls `herdr.App.FocusTab` (`app.go:316`, `do tab.select`), then activates herdr-app. Callers resolve their pane to its tab when they post: desk items from the queuing pane, the drain's needs-you from the worker's ledger pane ref, resume hooks from their own pane. No resolvable tab → click only activates herdr-app.
4. **`-execute` is the one shell string.** Built only from: the absolute forgectl path from `os.Executable()` (resolved via `filepath.EvalSymlinks`), the fixed verb, and a tab id that passed `^[A-Za-z0-9_-]{1,64}$`; each part single-quoted by one helper in `internal/notify`. Title and body never enter it. Unit tests: hostile id rejected (falls back to activate-only), quoting of a path containing a space and a `'`.
5. **`-group`.** `forgectl-desk-<item id>` / `forgectl-drain-<worker ref>`, same charset check, capped. A repeat signal replaces the banner.
6. **Closer.** When a desk item runs or is skipped (where the pane state is cleared today, `desk_signal_unix.go`), and when the drain releases needs-you, run `terminal-notifier -remove <group>`. Best effort, same timeout.
7. **Doctor.** Report the active sender; with only osascript: "clicks open Script Editor; `brew install terminal-notifier`".
8. **Cask.** Add `depends_on formula: "terminal-notifier"` through the `homebrew_casks` entry (`.goreleaser.yaml:136`; goreleaser `dependencies` field, or the existing `custom_block`). Regenerate/check `testdata/release/forgectl-*.rb` if a test pins the cask shape. Every brew user gains this one dependency.

### Alternatives declined

- **Native Swift notifier** (UNUserNotificationCenter). forgectl already ships a Swift helper (`helper/forgectl-bless-helper`, signed in release), so the pipeline exists, but notifications need an **app bundle** with a bundle id, not a bare binary. Worth it only if #3 builds a native app anyway; #3 decides.
- **herdr `notification show` only** — no banner when herdr isn't frontmost.
- **`alerter`** — not in Homebrew core; same mechanism.
- **A per-send runtime warning when falling back** — noise on every signal; doctor carries it.

### Files

- `internal/notify/notify.go`, `notify_test.go`
- `internal/cli/signal.go`, `desk_signal_unix.go`, `surface_drain_notify.go`, `resume_hooks.go` + their tests
- new `internal/cli/notify_open.go` (+ test)
- doctor check; `.goreleaser.yaml`

## Verification

- `go test ./internal/notify/... ./internal/cli/...` and `golangci-lint run --max-issues-per-linter=0 --max-same-issues=0`.
- Staged break: make the sender splice the body into the argv string; the injection test must go red.
- Host-dependency check (memory): run the tests with `terminal-notifier` stripped from PATH; the osascript fallback test must still pass.
- Live: `forgectl desk add` a test item → banner appears with forgectl/terminal-notifier attribution → click focuses the desk pane, no Script Editor. Repeat add → banner replaced, not stacked. Cameron confirms by eye (macOS reports nothing).

## Loop

| Thing left behind | Opened by | Closed by | Who |
|---|---|---|---|
| Fable tab `desk-ux-fable` | this session's launch | its findings doc + filed issues, then its own outro | that session |
| Tab `notch-surface` | this session's launch | its plan PR (or research doc) | that session |
| Banners per desk item / drain worker | each signal | `terminal-notifier -remove <group>` on run/skip/release | forgectl |
| Test desk items from live verification | task 2 verification | `forgectl desk` skip/attach of each | this session |
| Notifier PR | task 2 | merge after review `crit=0 imp=0` + green CI | this session |

## Workarounds

| Limit | Real? | Mitigation | Owner fix? |
|---|---|---|---|
| osascript banners belong to Script Editor | yes (macOS attributes by sending app) | external sender | not ours; replaced, not patched |
| terminal-notifier uses deprecated NSUserNotification | unverified on Darwin 27 — Step 0 probes it | osascript fallback; #3 may supply a native sender | n/a |
| herdr has no focus-by-pane-id | yes (`mutate.go:167`) | focus the tab | herdr is Cameron's fork — a `pane.select` command is a possible upstream ask; filed as a follow-up, not built here |

## Orchestrator

**Driver:** opus

Panel: plan-reviewer ran — 10 findings, 9 folded in, 1 declined (see Panel review — findings declined)

## Panel review — findings declined

- #9 per-send fallback warning: declined; doctor reports the fallback, a warning on every signal is noise.

## Deviations

- 2026-11-22, Step 0: Homebrew's `terminal-notifier` is 3.1.0 (`fr.julienxx.oss.terminal-notifier`, ad-hoc signed, minimum macOS 27). It posts through UNUserNotificationCenter, not the deprecated NSUserNotification the Workarounds table assumed, so it needs the user's notification permission. Without it every send exits 3 with "Notifications are turned off for this application". Doctor has to report that state too, and a sender failure must fall back to osascript instead of losing the signal.
- Tabs `desk-ux-fable` (Fable, terminal `t147`) and `notch-surface` (Opus, terminal `t148`) launched; both briefs confirmed as first prompts with `hc read`.
