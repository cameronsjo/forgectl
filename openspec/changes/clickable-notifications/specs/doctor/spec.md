## ADDED Requirements

### Requirement: Notification sender check
`doctor` SHALL report a `notifications` row naming the macOS notification sender: ok with the path when `terminal-notifier` is on PATH, warn when only osascript would post (a click opens Script Editor), with a hint to install `terminal-notifier`, and skip off macOS. The check MUST NOT post a notification.

#### Scenario: Only osascript
- **WHEN** `doctor` runs on macOS with no `terminal-notifier` on PATH
- **THEN** the `notifications` row is `warn` and its hint is `brew install terminal-notifier, then allow it in System Settings > Notifications`
