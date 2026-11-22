## ADDED Requirements

### Requirement: Needs-you banner lifecycle
The drain's needs-you macOS banner SHALL post through `terminal-notifier` when it is on PATH, under a banner group per worker, so a repeat replaces it, and it MUST be removed when the worker leaves needs-you. `surface drain notify-test` SHALL name the sender that posted (`sender` in `--json`) and note an osascript fallback.

#### Scenario: Worker leaves needs-you
- **WHEN** a worker in needs-you moves to another state
- **THEN** its needs-you banner is removed from Notification Center

#### Scenario: notify-test after a fallback
- **WHEN** `surface drain notify-test --json` runs and `terminal-notifier` exits non-zero
- **THEN** the macOS attempt is ok with `sender` `osascript` and a note naming the fallback
