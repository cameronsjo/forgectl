## Why

The estate moved from herdr 0.9 to herdr-core: `hc` runs persistent terminals and `herdr-app` draws the window. With the old herdr server gone, `forgectl surface launch --surface herdr --worktree <b>` creates the worktree and ledger row and then fails with `surface: launch failed in start (not-mutated): adapter did not create the surface`, because `herdr workspace list` answers `server_not_running`. Coordinators have been launching workers by hand with `git worktree add` and `hc run`, which skips the ledger, the worker posture floor, the environment allowlist, and the folder-trust preflight.

## What Changes

- New surface backend `hc`. `surface launch --surface hc` starts the trampoline with `hc run --bg --outlive-parent --no-notify --name <name> --cwd <dir> --label forgectl.surface=<ownership marker> -- <bootstrap>`. The harness invocation still reaches the terminal only through the trampoline socket, so hc never sees the harness path, args, env, or prompt.
- `--worktree` accepts `--surface hc` as well as `--surface herdr`, with the same ledger, `[launch.worker]` posture, environment allowlist, folder-trust preflight, and name rules.
- The worker verbs `list`, `read`, `ready`, `wait`, `brief`, and `close` take `--surface herdr|hc`, default `herdr`. Each backend keeps its own ledger per repo and session.
- Under hc, worker status comes from hc's OSC 7501 states (`idle`, `working`, `done`, `blocked`, `error`), never from screen text. `surface brief` sends with `hc send`, refuses when the status is anything but `idle` or `done`, and never passes `--force`. `surface close` runs `hc kill` then `hc rm`, only on a terminal whose label carries forgectl's ownership marker and whose creation time matches the ledger's reference.
- `surface tag`, `status`, `usage`, `merge`, and the drain stay herdr-only for now.
- The old herdr backend is unchanged.
- `--inherit-mode <mode>` on worker launches: a claude worker is never looser than the launching session's permission mode. The flag only tightens: auto workers still come from `[launch.worker] permission_mode = "auto"`. Claude Code exposes no live mode to child processes, so the launcher states it. Added at the operator's request on 2026-10-30.

Tracked as CAM-522 (Linear).

## Capabilities

### Modified Capabilities

- `surface`: a fourth backend, `hc`, and worker launches and worker verbs under it.

## Impact

- Flag values: `surface launch --surface hc`; `--surface herdr|hc` on `list`, `read`, `ready`, `wait`, `brief`, `close`, `tag` (which refuses hc).
- Reference wire: a `kind: "hc"` reference carries `terminal` and `created_ms`. Older binaries refuse it (unknown backend), as they should.
- Worker environment: an hc worker gets its own terminal's `HC_TERMINAL_ID` and `HC_SESSION`, read by the trampoline.
- Docs: `docs/herdr.md`, `README.md`, the shipped skill.
