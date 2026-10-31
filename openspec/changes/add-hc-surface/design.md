## Context

herdr-core splits what herdr 0.9 did in one server. `hc` (0.1.15 measured) owns terminals: it starts programs, keeps them alive, reads their screens, types into them, and reports agent status. `herdr-app` (0.1.10 measured) only changes what the person sees: spaces, tabs, panes. Its manual says to use hc for "starting, reading, messaging, waiting on or cleaning up agents and programs". A worker launch needs only hc.

Measured against a scratch `HC_SESSION` with scratch XDG paths on 2026-10-30:

- `hc run --bg --json ... -- '<one string>'` runs the string under `sh -c` and returns `{"address","agent","cwd","name","state","turn"}` at once, with its pipes closed.
- `--name` must be 1-32 of `a-z 0-9 - _ .`, start with a letter or digit, and not look like an address (`t7`). A taken name fails with `{"error":{"kind":"name_taken"}}`, exit 1; a bad one with `invalid_params`, exit 6. forgectl's `fc-surface-<32 hex>` marker is 43 characters, so it cannot be the name.
- Labels take that marker as a value (`--label forgectl.surface=fc-surface-…`), and `hc get` returns them. Labels are a namespace shared with herdr-app (it writes `where` and `herdr.*`) and are not authenticated: any client can write one.
- `hc server status --json` reports `core.id`. It is stable across `hc server restart` (terminals and addresses survive a restart) and differs between sessions. It starts a core when none runs.
- `hc get` reports `created_ms`, `state` (`live`/`finished`), `labels`, and `status` as an object `{app, msg, state}`. `hc ls` reports `status` as a bare string. A missing terminal is `not_found`, exit 3.
- `hc kill` on a finished terminal exits 0. `hc rm` on a live one fails with `kind: "live"`. `hc send` to a finished terminal fails with kind `finished`; to a blocked one with kind `blocked` unless `--force`. Several kinds share exit code 1, so the adapter maps errors on `kind`, never on the exit code.
- `hc run` does not forward the caller's environment: a program gets the core's environment plus `HC_TERMINAL_ID`, `HC_SESSION`, `HC_TERMINAL_SOCKET`, and `HC_SCOPE`.
- A child dies with its parent terminal unless started with `--outlive-parent`, and its parent gets notes about it unless `--no-notify`.

## Decisions

### A new `hc` backend, not herdr detection

`--surface hc` is a new backend. `--surface herdr` keeps driving herdr 0.9 unchanged. The surface command already refuses to detect a backend ("no default and no detection"), and two binaries with different verbs behind one flag would make a stored reference ambiguous. A new Kind is also the more reversible choice: removing it later touches nothing herdr users rely on.

The worker verbs (`list`, `read`, `ready`, `wait`, `brief`, `close`) take `--surface herdr|hc` with default `herdr`, so today's commands behave exactly as before. Whether the default should move to `hc` is left to the operator.

### Verb mapping

| forgectl | hc |
|---|---|
| launch (Start) | `hc server status` (readiness, core id), then `hc run --bg --json --outlive-parent --no-notify --name <name> --cwd <dir> --label forgectl.surface=<marker> -- <bootstrap>` |
| reconcile after an ambiguous start | `hc ls --all --json --label forgectl.surface=<marker>` |
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
3. `hc get <address>` returns a terminal whose `forgectl.surface` label equals the reference's ownership marker and whose `created_ms` equals the reference's.

An address alone is not enough: hc can reuse one after `hc rm`. A `not_found` answer from a core whose id matches proves the terminal is gone.

### Status and screen, both

hc's status states are the first readiness signal for an hc worker, and hc's manual says not to guess status from screen text. But the status is the program's own report, and anything that can write to the terminal can print a forged `idle` over a permission dialog. So a ready verdict, and a brief, also need the screen to show the harness's input prompt with no dialog: the herdr predicates' prompt and dialog checks, without their herdr-agent clause. That catches an honest dialog. It does not catch a forger: the screen comes from the same terminal output, and the second security review showed three printed lines (a rule, a bare prompt, a rule) under a real permission dialog read as an empty prompt. Neither signal is proof that no dialog is open.

A typed brief has no read-back step under hc: `hc send` pastes and submits in one call, and hc cannot type without submitting (`hc keys` takes named keys only). The status alone is not a safe gate before it, because it is the program's own report and anything that can write to the terminal can forge it. So brief also reads the screen and requires the harness's input prompt, empty, with no dialog (`ready.Table.EvaluateText`). Since both can be forged, the guard that holds against a forger is a key, not a check: brief sends one Escape (`hc keys <address> esc`, measured on hc 0.1.15 to deliver a single ESC byte) right before `hc send`, so the first key any open dialog sees is a refusal. Per harness: Claude Code's docs say Esc closes a dialog and declines a permission prompt, "the same as No", and at an idle prompt it interrupts nothing; Codex's approval overlay maps Esc to cancel, a hard contract for MCP elicitation; pi binds escape to select-cancel and interrupt. Only a second Escape within the harness's double-press window opens a menu (rewind in Claude Code, the tree selector in pi), and brief sends one; a retry sent within that window can open one. These are default bindings: an operator who rebinds Escape (Claude Code `keybindings.json`, a Codex approval keymap) voids the guarantee. Claude's vim editor mode would turn that Escape into NORMAL mode, so every claude worker's `--settings` pins `editorMode` to `normal`, and brief refuses a screen that shows a vim mode line as a backstop. That the inline `--settings` value outranks a user-level `editorMode: vim` comes from Claude Code's settings precedence docs (command-line settings sit above user and project settings) and its settings schema (`editorMode` takes `normal` or `vim`), not from a live run; an operator check with a scratch config dir holding `editorMode: vim` would confirm it. Escape also interrupts a running turn, and `hc keys` has no `--if idle`, so `PressEscape` takes the status and turn brief checked, re-reads the terminal, and refuses without a key when either moved. These come from documentation and source, not a live run of each harness (live runs of the real binaries were out of bounds). The remaining limits: a dialog drawn after the Escape and before hc pastes is not covered, nor a turn that starts in the one hc round trip between the Escape's re-read and the key. hc also refuses a blocked terminal itself. The marker is recorded after the Escape and before the send, so a refused Escape leaves no marker and a sent brief always has one. A send that hc refuses (blocked, finished, status mismatch) reports `refused`. Any other send failure reports `unconfirmed`, because the text may have arrived.

### Ledger

The ledger is keyed by repo and session. An hc worker's session key is `hc:<session>` (`hc:default` without `HC_SESSION`), so herdr and hc ledgers never share rows.

### Environment

hc does not forward the launcher's environment, and the worker allowlist is unchanged. The harness still gets its built environment over the trampoline socket. The trampoline adds the worker terminal's own `HC_TERMINAL_ID` and `HC_SESSION`, which hc set in that terminal, through the existing pane-identity path (`FORGECTL_SURFACE_PANE=hc`), never the launcher's. With both, a worker's own `hc send` reaches its core.

When the launcher runs inside hc, the worker terminal's parent is the launcher's terminal. `--outlive-parent` keeps the worker running when that coordinator ends, and `--no-notify` keeps hc from typing notes about the worker into the coordinator's terminal: the coordinator follows its workers through forgectl.

### herdr-app

Not needed for this change. A worker started by hc stays hidden from the window unless it carries `herdr.space=<id>`. Showing workers in the person's window is a follow-up: label the terminal with a space id from `herdr-app ls`, or call `herdr-app do terminal.keep`.

### Following the launcher's permission mode

A coordinator in auto should start auto workers, and a stricter coordinator should not start looser ones. forgectl needs the launcher's mode to do that, and Claude Code exposes no documented signal to a child process: no environment variable carries it (checked on 2.1.314), the session's argv (`CLAUDE_PID`) shows only the starting mode and misses a Shift+Tab change, and the transcript's per-entry `permissionMode` is an undocumented format. So the launcher states it with `--inherit-mode <mode>`.

The rule, for claude workers only:

1. The launcher's mode is a ceiling: whatever the worker resolves to (`[launch.worker]`, the built-in `acceptEdits`, or a stricter project block) is capped at it.
2. With `[launch.worker] inherit_launcher_mode = true` (and no `permission_mode`; both together refuse the launch), the worker takes the launcher's mode, capped at `auto`, in place of the built-in value. Without the opt-in the flag never raises a worker.

Why the raise needs the opt-in: the flag is the caller's unverifiable claim. A first draft let any worker with no `[launch.worker]` value take it, and the security review flagged that any session allowed to run `surface launch` could then self-grant `auto` workers. The operator asked for auto coordinators to start auto workers, so the raise returns behind config, which a session cannot grant itself. With the opt-in on, any caller passing `--inherit-mode auto` does get `auto` workers, a worker's own sub-workers included; that is no looser than `permission_mode = "auto"`, and the worker floor still applies. The opt-in also does nothing without the flag, so a launch under the opt-in that omits `--inherit-mode` prints a note, and the shipped skill tells a coordinator to pass it.

A later change could read the transcript's latest `permissionMode` when the flag is absent, if Claude Code documents that field or a hook writes the mode somewhere stable.

## Out of scope

- `surface tag`: it writes herdr sidebar metadata, which has no hc equivalent yet.
- `surface status`, `usage`, and `merge`: they read the ledger through seams that do not take a surface yet, so they reach herdr workers only.
- The drain (`surface drain`): it launches herdr workers and posts herdr notifications.
- A `forgectl.worker=<name>` label: the adapter cannot read the sealed display name to write it. The terminal's hc name is the worker name.
- `surface launch --surface hc` without `--worktree` works, but its display name must fit hc's name rules; the launch refuses otherwise.

## Risks

- A core whose state was wiped reports a new `core.id`. Its old workers' rows then list as `unreadable` and `close` refuses them as an identity mismatch, by design: forgectl cannot prove the terminal it would act on is its own. herdr's `ForSessionLedger` recovery has no hc equivalent yet; such a row needs a follow-up (a ledger-only forget) rather than a looser check.

- hc is 0.1.x. The JSON shapes above were measured on 0.1.15; the adapter reads them strictly and fails closed (unreadable, never gone) on anything else.
- `hc server status` starts a core when none runs, so `surface list --surface hc` can start one. It never stops or restarts one.
