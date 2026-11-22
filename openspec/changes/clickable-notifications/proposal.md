## Why

Clicking a forgectl macOS notification opens Script Editor's open-file panel. forgectl posts through `osascript -e 'display notification …'`, macOS credits those banners to Script Editor, and a click launches the app that posted. No osascript form changes that, so another app has to post (cameronsjo/forgectl#1491).

## What Changes

- macOS notifications post through `terminal-notifier` when it is on PATH. When it is missing or fails (its notification permission is off: exit 3), osascript posts the same title and body, so the operator is still told.
- A click on a `desk add` banner from a herdr-app terminal shows that terminal's space and tab, and brings herdr-app forward. Other banners (the drain's needs-you, the update watcher's, `pr drain`'s) bring herdr-app forward only.
- New hidden verb `forgectl herdr _focus-terminal --terminal <id> --herdr-app <path>`: what a click runs. It is not for typing.
- Each desk item and each drain worker posts under its own banner group. A repeat replaces the banner, and the banner is removed when the item runs or is skipped, or when the worker leaves needs-you.
- `desk add` warns when osascript posted in terminal-notifier's place, naming why and that a click opens Script Editor.
- `surface drain notify-test` names the sender that posted (`sender` in `--json`, additive) and notes a fallback.
- `forgectl doctor` gains a `notifications` row: ok with terminal-notifier, warn with osascript only, skip off macOS.
- The Homebrew cask depends on the `terminal-notifier` formula.

## Impact

- Surfaces: `desk add` (warning text), `surface drain notify-test` (`sender` field), `doctor` (`notifications` row), hidden `herdr _focus-terminal`, the cask.
- Code: `internal/notify`, `internal/herdr/app.go`, `internal/cli/{signal.go,desk_signal_unix.go,surface_drain_notify.go,resume_hooks.go,pr.go,herdr.go,herdr_focus_terminal.go}`, `internal/doctor`, `.goreleaser.yaml`.
- Issue: cameronsjo/forgectl#1491.
