## MODIFIED Requirements

### Requirement: Operator signal on add
When an item is queued, `add` SHALL notify the operator inside a herdr pane (a notification plus the pane's blocked state) and on macOS (a desktop notification), each switchable off under `[desk]` `notify_herdr` and `notify_macos`. The macOS notification SHALL post through `terminal-notifier` when it is on PATH, and through osascript when it is not or when `terminal-notifier` fails. A signal that fails MUST be reported as a `warning: operator signal failed:` line and MUST NOT fail the add. A notification osascript posted in `terminal-notifier`'s place MUST be reported the same way, naming the cause and that a click opens Script Editor.

#### Scenario: herdr notification fails
- **WHEN** `desk add` queues an item and the herdr notification call fails
- **THEN** the item is still queued and the exit code is 0
- **AND** stderr carries a `warning: operator signal failed:` line naming the setting that turns it off

#### Scenario: terminal-notifier is not allowed to notify
- **WHEN** `desk add` queues an item and `terminal-notifier` exits non-zero
- **THEN** osascript posts the notification, the exit code is 0, and stderr warns that a click opens Script Editor

## ADDED Requirements

### Requirement: Clickable desk banner
A desk banner posted through `terminal-notifier` SHALL, when `desk add` runs in a herdr-app terminal, show that terminal's herdr-app space and tab on click and bring herdr-app forward. Elsewhere a click SHALL bring herdr-app forward only, when it is installed. The command a click runs MUST NOT contain the notification's title or body. Each item's banner SHALL replace an earlier one for the same item and MUST be removed when the item runs or is skipped, whatever `notify_macos` now says.

#### Scenario: Click from a herdr-app terminal
- **WHEN** `desk add` runs in herdr-app terminal `t146` and the operator clicks the banner
- **THEN** herdr-app comes forward showing the space and tab that hold `t146`

#### Scenario: Hostile item text
- **WHEN** an item's WHAT line is `'; touch /tmp/pwned; '`
- **THEN** the banner's body shows that text and the click command does not contain it

#### Scenario: Skip clears the banner
- **WHEN** a queued item is skipped
- **THEN** its banner is removed from Notification Center
