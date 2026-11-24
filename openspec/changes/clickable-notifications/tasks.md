## 1. Prove the click (Step 0)

- [x] 1.1 Install terminal-notifier and click a probe that runs `herdr-app do tab.select` and activates herdr-app, there and back across spaces

## 2. Sender

- [x] 2.1 `internal/notify`: terminal-notifier when on PATH, osascript fallback, `Message{Title, Body, Group, Terminal}`, `Sent{Sender, Fallback}`, `Remove`, `GroupID`
- [x] 2.2 Build `-execute` from fixed parts only, each single-quoted; escape every free-text value for terminal-notifier's property-list parsing
- [x] 2.3 Tests: hostile id drops `-execute`; a path with a space and a quote survives `/bin/sh`; body never reaches `-execute`; fallback; both senders failing

## 3. Click

- [x] 3.1 `herdr.NewAppAt` and `AppClient.LocateTerminal` (ls only)
- [x] 3.2 Hidden `forgectl herdr _focus-terminal`: validate, locate, select space then tab

## 4. Callers

- [x] 4.1 `desk add`: group by queued hash, click terminal from `HC_TERMINAL_ID` in herdr-app, fallback warning; remove the banner when the item leaves pending
- [x] 4.2 Drain needs-you: group per worker, removed on Cleared; notify-test names the sender
- [x] 4.3 Update watcher and `pr drain` move to the new API

## 5. Doctor and release

- [x] 5.1 `doctor` `notifications` row
- [x] 5.2 Cask `depends_on formula: "terminal-notifier"` (rendered by a local snapshot)

## 6. Verify

- [x] 6.1 Staged break: splice the body into `-execute`; the injection test goes red
- [x] 6.2 Tests pass with terminal-notifier off PATH
- [x] 6.3 Live: `desk add` from a herdr-app terminal posts a grouped terminal-notifier banner, and skip removes it (2026-11-24)
- [ ] 6.4 Live: the banner click shows the queuing tab, and a repeat replaces the banner. Deferred to cameronsjo/forgectl#1502 (see design.md § Deviations)

## 7. Docs

- [x] 7.1 Update `docs/commands/desk.md`, `docs/commands/pr.md` and `docs/herdr.md` where they name osascript; README Usage needs no change (the verb is hidden)
