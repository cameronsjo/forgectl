## Why

The hc worker surface (forgectl#1384) left five follow-ups in forgectl#1386 about a worker's life after launch:

- `surface brief` re-checks a worker's status and turn only through `hc send --if idle`, which covers a worker last seen `idle`. A worker last seen `done` is sent to with no re-check at all.
- A core whose state was wiped reports a new id, so `close` calls every row taken on the old core an identity mismatch and refuses it. Nothing removes those rows.
- The ledger row does not say which permission mode a worker got, or why (its config, `--inherit-mode`, or the built-in value).
- hc workers never show in the herdr-app window.
- A brief sent right after another sends its Escape inside the harness's double-press window, which opens a menu (rewind in Claude Code, a selector in pi) instead of closing a dialog.

## What Changes

- `surface brief --surface hc` hands the checked status and turn to the send. The send re-reads the worker and types nothing unless it is still at the checked turn and idle or done. `--if idle` now follows the re-read status.
- New `surface forget <name> --surface hc` drops a worker's ledger row, and nothing else, when hc proves the terminal gone: the core is not the one the worker started on, nothing is at the recorded address, or the address holds a terminal forgectl did not start. It reads hc and never kills, removes, or types anything.
- The worker ledger row records the resolved posture: each field, its value, and its source. `surface list --json` prints it as `posture`.
- `surface launch --worktree --surface hc --herdr-space <id>` labels the worker's terminal `herdr.space=<id>`, which herdr-app reads to show it in that space. Without the flag the worker stays out of the window, as herdr-app's guide asks of background workers.
- An hc brief waits until one second has passed since the worker's last typed brief before its checks and Escape. An Escape whose delivery is unknown holds that second before the refusal returns.

Part of forgectl#1386.

## Capabilities

### Modified Capabilities

- `surface`: hc briefs, worker ledger rows, and hc worker launches; a new `surface forget` verb.

## Impact

- New verb: `surface forget`. New flag: `surface launch --herdr-space`.
- Ledger: a new `posture` field on worker rows, without a version change. An older binary refuses a ledger holding it, as with earlier added fields; upgrade every forgectl on the machine together.
- `surface list --json`: a new `posture` key (additive).
- Docs: `docs/herdr.md`.
