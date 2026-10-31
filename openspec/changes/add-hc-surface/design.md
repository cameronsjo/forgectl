## Context

herdr-core splits what herdr 0.9 did in one server. `hc` (0.1.15 measured) owns terminals: it starts programs, keeps them alive, reads their screens, types into them, and reports agent status. `herdr-app` (0.1.10 measured) only changes what the person sees: spaces, tabs, panes. Its manual says to use hc for "starting, reading, messaging, waiting on or cleaning up agents and programs". A worker launch needs only hc.

Measured against a scratch `HC_SESSION` with scratch XDG paths on 2026-10-30:

- `hc run --bg --json ... -- '<one string>'` runs the string under `sh -c` and returns `{"address","agent","cwd","name","state","turn"}` at once, with its pipes closed.
- `--name` must be 1-32 of `a-z 0-9 - _ .`, start with a letter or digit, and not look like an address (`t7`). A taken name fails with `{"error":{"kind":"name_taken"}}`, exit 1; a bad one with `invalid_params`, exit 6. forgectl's `fc-surface-<32 hex>` marker is 43 characters, so it cannot be the name.
- Labels take that marker as a value (`--label forgectl_surface=fc-surface-…`), and `hc get` returns them.
- `hc server status --json` reports `core.id`. It is stable across `hc server restart` (terminals and addresses survive a restart) and differs between sessions. It starts a core when none runs.
- `hc get` reports `created_ms`, `state` (`live`/`finished`), `labels`, and `status` as an object `{app, msg, state}`. `hc ls` reports `status` as a bare string. A missing terminal is `not_found`, exit 3.
- `hc kill` on a finished terminal exits 0. `hc rm` on a live one fails with `kind: "live"`. `hc send` to a finished terminal fails with exit 11; to a blocked one with exit 9 unless `--force`.

## Decisions

### A new `hc` backend, not herdr detection

`--surface hc` is a new backend. `--surface herdr` keeps driving herdr 0.9 unchanged. The surface command already refuses to detect a backend ("no default and no detection"), and two binaries with different verbs behind one flag would make a stored reference ambiguous. A new Kind is also the more reversible choice: removing it later touches nothing herdr users rely on.

The worker verbs (`list`, `read`, `ready`, `wait`, `brief`, `close`, `status`, `usage`) take `--surface herdr|hc` with default `herdr`, so today's commands behave exactly as before. Whether the default should move to `hc` is left to the operator.

### Verb mapping

| forgectl | hc |
|---|---|
| launch (Start) | `hc server status` (readiness, core id), then `hc run --bg --json --outlive-parent --name <name> --cwd <dir> --label forgectl_surface=<marker> -- <bootstrap>` |
| reconcile after an ambiguous start | `hc ls --json --label forgectl_surface=<marker>` |
| probe / list | `hc server status`, `hc get <address>` |
| read | `hc get` for status, then `hc read --json --lines N <address>` |
| ready / wait | the OSC 7501 status from `hc get`: `idle` or `done` is ready, `blocked` is blocked, anything else is not ready |
| brief | `hc get` (must be `idle` or `done`), record the marker, `hc send --json <address> <text>`; never `--force` |
| close | `hc kill <address>` (when live), then `hc rm <address>` |
| terminal id in `list --json` | the hc address (`t7`) |

Every call goes through the sensitive runner, as the other adapters' calls do: the cwd, name, and bootstrap stay opaque, and only the kind is logged.

### Ownership and identity

A reference names the terminal by address and `created_ms`, and the server by a fingerprint of the session name and `core.id`. Close, probe, read, and brief act only when all of these hold:

1. the reference's source matches the session this process selects (`HC_SESSION` set or not);
2. the fingerprint of `core.id` matches, so a reference from another session or a wiped core state is refused;
3. `hc get <address>` returns a terminal whose `forgectl_surface` label equals the reference's ownership marker and whose `created_ms` equals the reference's.

An address alone is not enough: hc can reuse one after `hc rm`. A `not_found` answer from a core whose id matches proves the terminal is gone.

### Status, never screen text

hc's status states are the readiness signal for an hc worker. The herdr readiness table matches prompt patterns on screen text because herdr 0.9's agent status was not trusted on its own; hc's manual says not to guess status from screen text. Screen text is still read, but only for `surface read` and for finding a `REPORT <marker>:` line.

A typed brief therefore has no read-back step under hc: `hc send` pastes and submits in one call, and hc itself refuses a blocked terminal. The marker is recorded before the send, as it is under herdr. A send that hc refuses (blocked, finished, status mismatch) reports `refused`. Any other send failure reports `unconfirmed`, because the text may have arrived.

### Ledger

The ledger is keyed by repo and session. An hc worker's session key is `hc:<session>` (`hc:default` without `HC_SESSION`), so herdr and hc ledgers never share rows.

### Environment

`HC_SESSION` joins the worker allowlist so a worker's own `hc send` reaches the same core. The trampoline passes the worker's own `HC_TERMINAL_ID` through the existing pane-identity path (`FORGECTL_SURFACE_PANE=hc`), never the launcher's.

The worker terminal's parent is the launcher's terminal when the launcher runs inside hc, so hc's own notes about a blocked or exited child reach the coordinator. `--outlive-parent` keeps the worker running when that coordinator ends.

### herdr-app

Not needed for this change. A worker started by hc stays hidden from the window unless it carries `herdr.space=<id>`. Showing workers in the person's window is a follow-up: a `--show` option that labels the terminal with a space id from `herdr-app ls`.

## Out of scope

- `surface tag`: it writes herdr sidebar metadata, which has no hc equivalent yet.
- The drain (`surface drain`): it launches herdr workers and posts herdr notifications.
- `surface launch --surface hc` without `--worktree` works, but its display name must fit hc's name rules; the launch refuses otherwise.

## Risks

- hc is 0.1.x. The JSON shapes above were measured on 0.1.15; the adapter reads them strictly and fails closed (unreadable, never gone) on anything else.
- `hc server status` starts a core when none runs, so `surface list --surface hc` can start one. It never stops or restarts one.
