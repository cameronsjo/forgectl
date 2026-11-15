# Host support

Which forgectl features work on which terminal host. A cell holds a result only when it was run, with the version it was run on. A blank cell (`·`) is unverified, not unsupported. A cell that names a pull request describes work that is not on `main` yet. Read it as "built there" unless the cell also says "live run", which means a person ran it by hand against that host and the pull request carries the gated test or the notes.

Hosts: **tmux**, **cmux**, **herdr 0.9** (the `herdr` CLI, session server), **herdr-core** (`hc`, runs terminals), **herdr-app** (the window over herdr-core) and **Rex** (preview).

| Feature | tmux | cmux | herdr 0.9 | herdr-core (`hc`) | herdr-app | Rex |
| --- | --- | --- | --- | --- | --- | --- |
| Launch, probe and close a worker (`forgectl surface`) | · | · | · | live run on hc 0.1.21, by hand, two terminals (one finished before close, one still running): start, probe, close, second close all as documented; a finished terminal probes *gone* (gated test in #1428) | not a launch path; terminals run in herdr-core | live run on 0.1.0-preview.19, API revision 14, by hand: start, probe (running, then exit 7), close (gated test in #1425). Needs `rex` on `PATH` until the locator (#1418) merges |
| Desk "waiting" signal | · | · | notification and pane-blocked calls (existing; not re-measured) | reporter probe on hc 0.1.21: `blocked`, and `blocked` with `kind=auth`, are recorded under the terminal's `records`; `desk watch` itself not run (#1417, stacked on #1402) | OSC 7501 `blocked` built (#1417, stacked on #1402); sent only while `desk watch` or the dashboard runs, under the status row's gates; not run on this host | reporter probe on 0.1.0-preview.19: `blocked`, and `blocked` with `kind=auth`, are read back with `block call program_status`; `desk watch` itself not run (#1417, stacked on #1402) |
| `forgectl herdr organize` | · | · | existing path, unchanged (needs the fork's `tab move`) | n/a | through `herdr-app do tab.move` (#1419): reorders tabs within a space; a move between spaces is reported as unsupported. The report (no `--apply`) ran against the live window on herdr-app 0.1.15 and hc 0.1.21 and changed nothing; `--apply` not run | n/a |
| Status of `workflow run` and `desk watch` (OSC 7501 `working`, `done`, `error`) | built (#1402): sent only when stderr is a terminal, `TERM` is not `dumb`, and `FORGECTL_NO_PROGRAM_STATUS`, `FORGECTL_WORKER` and `FORGECTL_DRAIN_WORKER` are unset; through tmux (also when another host runs inside tmux) it needs `allow-passthrough on`; no host observed | built (#1402), same gates; no host observed | built (#1402), same gates; no host observed | built (#1402), same gates. Reporter probe on hc 0.1.21: `working` (with progress and message), `blocked`, `done`, `error` and `clear` are each recorded under the terminal's `records` and shown by `hc get`; after the program exits, every record is gone, `done` and `error` included | built (#1402), same gates; no host observed | built (#1402), same gates. Reporter probe on 0.1.0-preview.19: the same states are read back with `block call program_status`; after exit `working` is dropped and `done` is kept |

## Versions this was measured on

- herdr-core `hc` 0.1.21 (the adapter's parsers were written against 0.1.15; the 0.1.21 comparison found additive drift only: cameronsjo/forgectl#1408).
- Rex 0.1.0-preview.19, API revision 14; the adapter refuses any other revision and any server shared with tailnet members.
- herdr-app 0.1.15: `tab.move` exists as a stock command and `herdr-app commands` lists no notification command (read on this machine).

## What is still owed

- The OSC 7501 report was probed on hc and Rex with a small program that uses forgectl's reporter (not a full `workflow run` or `desk watch`). What herdr-app and herdr 0.9 do with it, and what any host's window shows, needs a person looking at the screen: cameronsjo/forgectl#1404.
- `organize --apply` against a live herdr-app space (it reorders the open window): cameronsjo/forgectl#1412.
- The Rex locator and the commands that act on a running Rex worker (list, close, brief, ready): cameronsjo/forgectl#1418, #1422, #1423 and #1424.
- tmux, cmux and herdr 0.9 cells are blank because this table was started from the herdr-core and Rex work; their behavior is documented on the command pages and was not re-measured here.

Update a cell only after running it, and change its version in the same edit.
