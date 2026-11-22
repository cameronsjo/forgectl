---
status: planned
next: "T0 waits on cameronsjo/forgectl#1476 (clickable notifications, draft, build-test and macos-test red on 2026-11-22). After it merges: T0 banner trial → verdict. Build: T1 queue fields → T2 drain row → T3 jump → S1 security review → T4 NotchCore → T5 app → S2 security review → T6 cask/release → T7 docs → T8 decision issue. Not yet: write docs/research/2026-11-22-notch-surface.md and stop."
branch: plan/notch-surface
pr: "—"
updated: 2026-11-22
date: 2026-11-22
model: claude-opus-5-5
harness: claude-code
machine: sjomba
approved_session_id: 51770461-48e1-4a16-b1f3-36edd6bf865a
---

# Notch surface for the attention queue

## Context

On the widescreen external monitor, macOS banners appear in the far top-right, so the ones that need action go unseen. Cameron asked for a notch / Dynamic-Island-style surface. A parallel session (worktree `feat/clickable-notifications`, plan `docs/plans/2026-11-22-clickable-notifications.md` on that branch) is moving banners from osascript to terminal-notifier, with a click that runs `forgectl notify open --tab <id>` and a `-remove <group>` when the desk item or drain wait ends. This plan does not edit `internal/notify` or its callers.

**First question settled: can a notch surface be seen where Cameron looks?** The MacBook notch is on the built-in panel, not the external monitor. A built surface can draw there: DynamicNotchKit's `expand(on: screen)` draws a floating island on a screen with no notch. Every adoptable notch app either has no outside input or puts items on the built-in screen first. **The panel then raised a cheaper question this plan must answer first:** is the problem banner *placement* rather than the lack of a notch? `PingPlace` moves macOS banners to top-center, and the parallel session already makes them clickable and self-closing. T0 tests that combination before anything is built.

## Goal

1. **T0 (gate):** a short trial of top-center, persistent, clickable banners. If they get seen, the verdict is "not yet". A research doc records why, and nothing below T0 is built.
2. **Otherwise:** a separate signed and notarized agent app, `Forgectl Notch.app` (cask `forgectl-notch`), shows forgectl's attention queue at the top-center of every screen. The costliest waits (a blocked worker, a waiting desk item, a stuck drain) expand the island once. A click focuses the item's herdr tab. An item leaves the island when it leaves the queue. Banners stay as they are, and T8 decides whether the notch replaces them.

## Research summary (adopt vs build)

Sources: GitHub repos and Apple developer docs, read 2026-11-22. "None" means none documented in the README and `Info.plist`; no full source grep was run.

- **PingPlace** (GPL-3.0, 388 stars, v1.4.2 2026-07-15): moves system banners to one of 8 positions, Top Middle included. Needs Accessibility permission. Supports macOS 26.4.1+, which on paper covers this Mac (macOS 27.3). Untested on 27, and which display the banners use is not documented. Basis of T0.
- **Claude Island** (`farouqaldori/claude-island`, Apache-2.0, ~2.5k stars, a copy is at `~/Projects/claude-island`): a notch app for Claude Code sessions, with a screen picker and click-to-focus. Declined: it is fed by hook pushes, targets tmux/yabai rather than herdr, sends Mixpanel analytics, and defaults to the built-in screen. Its `ScreenSelector` and `NotchWindowController` are worth reading before T5.
- **boring.notch, Atoll, NotchDrop**: no outside input; they show their own widgets only.
- **MewNotch**: documents per-display and non-notch styles. It has a bash-script view but no push API and no click action.
- **notchbar**: `notchbar-cli` sends JSON over a socket anyone local can use. Text only, no click, notched screen first.
- **NotchIsland**: mirrors Notification Center by reading its database, which needs Full Disk Access.
- **DynamicNotchKit** (MIT, macOS 13+, 1.1.0, pushed 2026-11-21): `expand(on:)`, `compact(on:)`, `hide()`, `.auto` style. The build base.
- **One bundle can own banners later.** `UNUserNotificationCenter` posts under the bundle's identity and hands the click to the app. terminal-notifier 3.1.0 already works this way (peer plan, Deviations).

## Architecture

```text
 Claude sessions ─put─▶ spool ──┐
 desk add ───────────▶ desk/ ───┤
 forgectl sweep ─────▶ sweep.json┼─▶ forgectl attention list --json
 drain.json + drain.lock ───────┘   rows: key, tier, kind, pane…
  (T2: derived at read time)        sources: per-source state + ages
                                              │ FSEvents + 30 s poll,
                                              │ 10 s child timeout
                                              ▼
                                Forgectl Notch.app (LSUIElement)
                                ├ NotchCore: decode, diff, raise, stale
                                └ DynamicNotchKit island per screen
                                              │ click (row key only)
                                              ▼
                     forgectl attention jump --key K --json
                     re-lists → pane from herdr → shared focus-tab
                     function (the one behind `notify open --tab`)
```

**The feed is queue state, not pushed events.** The app runs `attention list --json` and renders it. No new IPC channel, no sink behind `internal/notify`. All validation stays in Go, and items close when their source does. The one signal missing from the queue, the drain's stuck hold, becomes a row derived from `drain.json` (T2).

## Notification policy (what earns the notch)

**The policy lives in Go.** `attention list` emits a `tier` per row, computed beside the cost constants in `internal/attention/queue.go`. v1 has no `[notch]` config.

| Tier | Rows | Island |
|---|---|---|
| `raise` | desk waiting (cost 2), blocked worker (cost 1), drain hold (cost 1) | expand once, then stay in the badge |
| `count` | marker-clean PR (3), `Action (Cameron)` (4), and any `q`/`a` row | badge count only |
| `hidden` | questions (5), proposals (6) | not shown; still in `attention list` |

- **Threat statement.** Any same-user process, a prompt-injected session included, can add a desk item, write a spool record, or write `sweep.json`. So a raise is a request for attention, never authority. The worst a forged row can do is expand the island once. A click passes only the row key; `jump` re-resolves the pane from herdr and runs nothing from row text. Record-derived rows (`q`/`a`) never raise, whatever their origin. `origin` is displayed as a word on every row and gates nothing.
- **Raise rules.** A key raises at most once per app run. On the first good read after launch, only rows whose `at` is after launch raise, so a restart does not replay every wait. All new raise rows in one read share one expand, with a count and one accessibility announcement. Only a good read changes the set of keys already raised; a failed read changes nothing.
- **Badge.** It shows the worst tier as a glyph plus a word, the count, and the age of the oldest raise row once past 15 minutes (`1 blocked · 3 · 25m`). It never relies on colour alone.
- **Collapse and control.** The island collapses after 8 s without hover, on Esc, or on a click outside it. A right-click opens a menu: `Snooze all 30m` (expiry re-raises once), `Open at login`, the resolved forgectl path and version, and `Quit`. There is no per-row snooze in v1.
- **No sound.** Under Reduce Motion the island crossfades instead of animating. Row text is rendered with `Text(verbatim:)`, never as markdown and with no link detection.

## Supersede or sit beside: beside, with a dated decision

Banners keep Notification Center history and reach Cameron under Focus or away from the desk. The notch takes "look here now". T8 files a decision issue 14 days after ship with three options:

- (a) keep both;
- (b) forgectl skips the raise-tier banner while the notch's heartbeat is fresh. This must fail open: a missing or stale heartbeat posts the banner.
- (c) the notch bundle takes over banners through `UNUserNotificationCenter`, and terminal-notifier is retired.

(b) and (c) touch `internal/notify` and wait until the parallel session's PR merges.

## Loop

| Thing left behind | Opened by | Closed by | Who closes |
|---|---|---|---|
| T0 trial setup (PingPlace install, Accessibility grant, banner style set to Alerts) | T0 | verdict "build": kept or removed at Cameron's choice. Verdict "not yet": kept as the fix, and the research doc records the setup | Cameron |
| A raised or counted island item | a row with tier `raise` or `count` | the row leaving `attention list` (desk item run or skipped, worker unblocked at the next sweep, drain hold cleared or drain stale, record pruned) | the source; the app drops the item on the next good read |
| An item Cameron acted on but whose source has not refreshed (worker rows clear only at the next sweep) | a successful jump | the next sweep; until then the row is dimmed and labelled "acted on · waits for next sweep" | the app |
| A stale or degraded source | the last good read older than 90 s, a 10 s child timeout, `sources.X` not `ok`, `sweepAge` over 2 h, or `herdr` unavailable (clicks disabled) | the next good read for that source. Only that source's rows grey out, and a one-line reason is shown | the app |
| The snooze-all | the menu | its 30-minute expiry, or Quit | the app (in memory) |
| The login item | the `Open at login` toggle (`SMAppService.mainApp`) | the toggle, or the bundle's removal by `brew uninstall --cask forgectl-notch`. T6 checks by hand once that macOS drops the entry, and `docs/notch.md` records the result and any manual step | Cameron / macOS |
| `events.jsonl` (T8 evidence) | the app's first raise | size rotation at 1 MB, keeping one old file. `docs/notch.md` names it for removal at uninstall | the app |
| The herdr `pane focus <id>` issue (filed in T3) | T3 | herdr shipping it, after which `jump` moves to pane grain (`debt:` marker in the code) | Cameron (herdr fork) |
| The supersede decision | T8's issue, filed at ship | Cameron's ruling at ship + 14 days | Cameron |

## Workarounds

| Limit worked around | Platform | Real? | Disposition |
|---|---|---|---|
| The external monitor has no notch | hardware | yes | floating island via DynamicNotchKit; no fix possible |
| herdr focuses by tab, not pane (`internal/herdr/mutate.go:167`) | herdr (Cameron's fork) | yes on origin/main | ship at tab grain and show the pane in the expanded row. T3 files the pane-focus issue (Loop row) |
| No change event from herdr or sweep | herdr / forgectl | yes | a 30 s poll, debounced at 1 s against FSEvents bursts (pruning a spool file fires FSEvents) |
| `attention list` costs 0.2-0.8 s per call (3 runs, sjomba, herdr unavailable in the sandbox) | forgectl | yes | at most one read in flight, a 10 s timeout, and FSEvents-triggered reads |
| A login-item app may not reach herdr: launchd gives no `HERDR_SOCKET_PATH` and `PATH` is minimal | herdr / macOS | **unverified**, and it decides whether clicks work | T1 measures it first (Live-state claim 4); `fix first` if it fails |
| The Developer ID job would compile third-party Swift | release pipeline | yes | build unsigned in a job with no `signing` environment, then sign the hash-checked artifact in the signing job (T6) |

## Alternatives declined

- **Build without trying placement first**: a one-afternoon trial could make the whole app unnecessary.
- **Adopt Claude Island, notchbar, MewNotch or boring.notch**: reasons above. None has both external-screen-first placement and a click that reaches herdr.
- **A push sink behind `internal/notify`**: it duplicates the queue's validation, every pushed item needs its own closer, and it edits files the parallel session owns.
- **Ship the app inside the `forgectl` cask**: forgectl is public, and every CLI upgrade would quit the app. The separate `forgectl-notch` cask `depends_on cask: "forgectl"`.
- **A `[notch]` config section in v1**: thresholds are fixed in Go, with tests; screens are all.
- **Render inside Herdr.app**: ties the surface to one terminal host.

## Related

- cameronsjo/forgectl#1284 (attention queue), cameronsjo/forgectl#1349, cameronsjo/forgectl#1177 / #1266 / #1189 (drain needs-you delivery), cameronsjo/forgectl#1417 / #1404 (desk OSC 7501 signal), cameronsjo/forgectl#32 (pr poll tray).
- The parallel plan: `docs/plans/2026-11-22-clickable-notifications.md` on `feat/clickable-notifications` (cameronsjo/forgectl#1476).

## Panel

Panel: plan-reviewer (conflicts and loops), plan-reviewer (underspecified tasks), cameron-review, security-posture-reviewer, operability-reviewer, user-experience-reviewer ran — 72 findings, 69 folded in, 3 declined (see Panel review — findings declined)

## Panel review — findings declined

- **[Policy] count questions (cost 5) in the badge** (UX advisory): v1 keeps them hidden to protect the badge's signal. The T8 data shows whether that hid real waits.
- **[T4] persist the raised-key set across restarts** (ops nit): the first-read-after-launch rule already stops the restart replay without a state file.
- **[T1] `attention list --no-prune` for UI pollers** (ops advisory): this pruning behaviour predates the plan and happens on any `list`. T1 files it as a follow-up issue rather than widening this plan.

## Tech Stack

- Swift 5.9+/SwiftPM, macOS 14+ target.
- DynamicNotchKit pinned by **revision**, `Package.resolved` committed, built with `--force-resolved-versions`, vetted with `cadence-forge:vet-dependencies`.
- Universal binary (arm64 + x86_64, checked with `lipo`).
- Developer ID signing plus notarization, with the notarytool step lifted from `cameronsjo/minute`'s release workflow.
- Go side: `internal/attention`, `internal/cli`, `internal/surface/drain` (read only).

## Global Constraints

- Do not edit `internal/notify` or its callers. The shared focus-tab function lives outside `internal/notify`, and its name is agreed with the parallel session before either PR merges.
- The app never executes, opens, or interprets row text. A click passes only a key matching `^[0-9a-f]{16}$`, as `--key=K`.
- The app resolves forgectl once at launch: `/opt/homebrew/bin/forgectl`, else `/usr/local/bin/forgectl`, with symlinks resolved. It accepts it only if `codesign --verify -R` matches Team `5H883FMW6H`, and logs the path and version. Otherwise it shows a stale reason and runs nothing. No shell, no `$PATH` lookup.
- The app parses `attention list` output as untrusted: it ignores unknown fields, caps lengths again, and treats a decode failure as stale.
- `attention list --json` changes are additive only, except that identical-key duplicates from one source collapse to one row (T1, documented).
- Logs never carry row text: `os_log` subsystem `com.cameronsjo.forgectl.notch`; `events.jsonl` holds key, kind, tier, action, outcome and time.

## Live-state claims

| # | Claim | Probe | Acting step |
|-:|---|---|---|
| 1 | The parallel session ships `forgectl notify open --tab` and a focus-tab function T3 can share | `git show origin/main:internal/cli` after its PR merges, plus a message to that session | T3 |
| 2 | PingPlace runs on macOS 27.3 and puts terminal-notifier banners at top-middle of the screen Cameron is using | install it and post a probe banner | T0 |
| 3 | `drain.json` carries `slots_hold`, `drain.Effective` reports a dead drain as stale, and `drain.lock` is flocked while it runs (`internal/surface/drain/status.go`, `internal/surface/worker/drainfiles.go`) | `git show origin/main:internal/surface/drain/status.go` | T2 |
| 4 | `attention list` and herdr tab focus work from a process with launchd's environment | `open -a` a probe app, or `launchctl asuser` a one-shot, that runs `forgectl attention list --json` with a fixture spool and prints `sources.herdr` | T1, before any Swift |
| 5 | `forgectl config get` does not exist (only `config [--json]`); v1 needs no config read | `forgectl config --help` | T4 |

## Orchestrator

**Driver:** opus — the click path runs a command from an app fed by same-user-writable data, and the release adds a signed, notarized artifact. T5's SwiftUI work can go to a Sonnet implementer working from T4's tested core.

## Tasks

- [ ] **T0 Banner trial (gate).** Runs after the clickable-notifications PR merges.
  - Install PingPlace and set it to Top Middle. Set terminal-notifier's banner style to Alerts so banners persist.
  - Run 3 working days on the external monitor.
  - Each time a desk item or needs-you appears, Cameron notes one of three outcomes: seen within a minute / seen late / missed.
  - **Not yet** (no missed, at most one late): write `docs/research/2026-11-22-notch-surface.md` from this plan's research summary plus the trial result, on a `research/` branch with a PR, and stop.
  - **Build**: tick T0 with the counts and go on.
  - If PingPlace does not run on 27.3, record that and go to build.
- [ ] **T1 Queue fields.**
  - **`key`**: 16 hex characters of sha256 over a length-prefixed tuple of the row's source plus fields fixed at creation. Per kind:
    - desk: item name (`Tag`);
    - item (`q`/`a`): session, turn, N;
    - worker: worker name and block;
    - PR: owner/repo#n;
    - drain: the constant `slots-hold`.

    Session and worker merges never enter a desk key.
  - **`tier`**: per the policy table.
  - **`sources`**: gains `sweepGeneratedAt` and `herdr` detail.
  - **`attention list` text output**: shows a short key.
  - **Tests**: the key is stable across a herdr flap, a sweep merge and a new turn; two desk items with the same content get distinct keys; duplicate sweep entries collapse. Stage the break: make two rows share a key and confirm red.
  - **Live-state claim 4** is measured here first.
  - **File** the `--no-prune` follow-up issue.
- [ ] **T2 Drain row (own PR).** Derive a `drain` row at read time. It exists only when all four hold: `drain.json` reads `running`, `slots_hold` is set, `drain.SlotsStuck` is true, and a `LOCK_SH|LOCK_NB` probe on `drain.lock` fails (the drain is alive). There is no new writer and no TTL. Text is fixed, with no free text from the drain. Tests: a crashed drain with a hold shows no row, a stale tick shows no row, and a live stuck drain shows one. Update `docs/commands/attention.md` (cost table, `drain` source).
- [ ] **T3 Jump.** `forgectl attention jump --key=K [--json]`:
  - It validates K, re-lists, and requires exactly one matching row.
  - It takes the pane from herdr only, resolves its tab, and calls the shared focus-tab function, which focuses the workspace too where `FocusNeedsSpace()`, then brings herdr-app forward.
  - `--json` returns `{outcome: ok|gone|ambiguous|no-pane|herdr-unavailable, reason}`. Usage errors exit 2.
  - Each call appends a `source=notch` line to the app's `events.jsonl` through a flag the app passes.
  - It files the herdr `pane focus <id>` issue.
- [ ] **S1 Security review (before T4).** Run `cadence-forge:security-reviewer` at Opus over `internal/attention/*`, `internal/cli/attention_unix.go`, and the T1-T3 diffs. It rules on the provenance basis for raising and the key/jump resolution.
- [ ] **T4 NotchCore** (`helper/forgectl-notch/Sources/NotchCore`). Pure logic, unit-tested like `BlessHelperCore`:
  - decode;
  - the raise diff (first-read rule, grouping, good-read-only updates);
  - the badge string;
  - per-source staleness with its reason lines;
  - jump outcome → row message;
  - the "acted on" dimming;
  - the forgectl path and Team-ID acceptance decision.

  Stage the break: let a `q` row raise, and let a failed read clear the raised set. Each must go red.
- [ ] **T5 App shell.**
  - An LSUIElement app with one DynamicNotchKit island per screen. Read the Claude Island screen and window code first.
  - FSEvents on the spool and desk directories, using the paths T1 confirms from `attention list`; a 30 s poll; a 10 s child timeout.
  - Collapse and menu as in the policy. Pane-less rows show no jump affordance and give the reason. Jump failures show inline for 5 s.
  - `os_log` plus `events.jsonl` at `$XDG_STATE_HOME/forgectl/notch/`, mode 0700/0600.
  - Hand-check checklist in `docs/notch.md`: MacBook alone, external alone, both, over a full-screen app, Reduce Motion, VoiceOver. Always launch with `open -a` or the login item, never from a pane shell.
- [ ] **S2 Security review (before the first tagged release).** Review `release.yml`, `.goreleaser.yaml`, `Package.swift`/`Package.resolved`, the bundle script, and the new cask's stanzas.
- [ ] **T6 Bundle, sign, notarize, cask.**
  - `scripts/build-notch-app.sh <identity> <keychain>` is one idempotent script that local ad-hoc and CI signing share. It assembles the bundle (Info.plist, LSUIElement, bundle id `com.cameronsjo.forgectl.notch`, SwiftPM resource bundles copied into `Contents/Resources`) and signs inner code first, then the bundle, with the hardened runtime, no `get-task-allow`, and no `--deep`.
  - CI: build and test NotchCore. Release: build unsigned in a job without the `signing` environment, hand the artifact over by sha256, then sign, notarize and staple in the signing job. Add `-x` to its `security import`.
  - Fail-closed check: `codesign --verify --strict -R '=anchor apple generic and certificate leaf[subject.OU] = "5H883FMW6H"'` plus a runtime-flag assertion.
  - New `forgectl-notch` cask in the tap: `app`, `depends_on cask: "forgectl"`, `uninstall quit:`, `zap` for the state directory. No quarantine strip, because the bundle is notarized. The tap job's cask check becomes an allowlist of exact stanza lines for both casks.
  - Check the login-item cleanup by hand once (Loop row).
  - A `doctor` row reports whether the notch is running and registered.
- [ ] **T7 Docs.** `docs/notch.md`: what shows, the tiers, the threat statement, logs and `log show --predicate 'subsystem == "com.cameronsjo.forgectl.notch"' --last 1h`, the hand-check list, and uninstall. Also a README roster line, and `docs/commands/attention.md` for `jump`, `key` and `tier`.
- [ ] **T8 Supersede decision.** At ship, file a cameronsjo/forgectl issue with options (a)/(b)/(c), the fail-open rule for (b), a due date of ship + 14 days, and the `jq` query over `events.jsonl` that counts raises, notch jumps, and raises that left with no click. Ask the parallel session to tag its `notify open` clicks the same way so the comparison is notch vs banner. Cameron rules.

## Verification

- `go test ./internal/attention/... ./internal/cli/...` and `golangci-lint run --max-issues-per-linter=0 --max-same-issues=0`.
- `swift test --package-path helper/forgectl-notch`.
- End to end on sjomba, with the app started by `open -a`:
  1. `forgectl desk add` a dummy item. The island expands on the external screen within 2 s.
  2. Click it. Focus lands on the queuing herdr tab.
  3. `forgectl desk skip <name> --reason test`. The item leaves on the next read.
  4. Hand-write a spool record with `cwd: cos-x`. It is counted, not raised.
  5. Kill herdr. Clicks disable and the reason line shows.
- `bash scripts/build-notch-app.sh - ""` (ad hoc), then the `codesign --verify --strict` requirement check. It must fail on the ad-hoc build and pass on the CI artifact. That is the staged break for the Team-ID gate.

## Deviations

## Learnings
