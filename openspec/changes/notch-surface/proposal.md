## Why

On a wide external monitor, macOS banners land in the far top-right, so the forgectl notifications that need action (a waiting desk item, a blocked worker, a stuck drain) go unseen. The operator asked for a notch-style surface at the top centre of the screen being looked at. A trial of moving banners to the centre was waived by the operator, so this builds the surface.

Tracking: cameronsjo/forgectl#1501.

## What Changes

- **Row key and tier.** Every row of `attention list --json` carries `key` (a stable 16-hex id for the wait) and `tier` (`raise`, `count` or `hidden`). Rows with the same key from one source collapse to one. The text view leads each row with a 6-character key.
- **Source detail.** `sources` gains `herdrDetail`, `sweepGeneratedAt`, `drain`, `core` and `coreDetail`.
- **Drain row.** A live drain whose claude launches a claude-slots check has held for 15 minutes shows one `drain` row, read from `drain.json` without writing or locking.
- **Desk terminal.** `desk add` records the herdr-app terminal that queued the item, and the hc core it belongs to, in the item meta (`terminal`, `terminal_core`). `attention list` gives a desk row that `terminal` only while `hc ls` lists it live on the same core.
- **Jump (planned).** `attention jump --key=K [--json]` focuses the row's herdr-app terminal. It waits on cameronsjo/forgectl#1476.
- **The app (planned).** `Forgectl Notch.app` (cask `forgectl-notch`), a signed agent app that reads `attention list --json`. Its decision logic is the `NotchCore` Swift package in `helper/forgectl-notch`.

## Capabilities

### Modified Capabilities

- `attention`: row key and tier, the drain row, desk terminals, new source fields, and (planned) `attention jump`.
- `desk`: the `terminal` and `terminal_core` meta fields.

## Impact

- `attention list --json`: new row fields `key`, `tier`, `terminal`; new row kind `drain`; new `sources` fields. All additive except that same-key duplicates from one source collapse.
- `attention list` text output: each line starts with a short key.
- Desk item meta: new optional `terminal` and `terminal_core`.
- `attention list` now runs `hc ls --json` (fixed argv, 3 s timeout) and reads `$XDG_STATE_HOME/forgectl/surface/drain.json`.
- New SwiftPM package `helper/forgectl-notch` and a `notch-core` CI job.
- Follow-up: cameronsjo/forgectl#1492 (`attention list --no-prune` for pollers).
