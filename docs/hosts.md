# Host support

Which forgectl features work on which terminal host. A cell holds a result only when it was run, with the version it was run on. A blank cell (`·`) is unverified, not unsupported. A cell that names a pull request describes work that is not on `main` yet. Read it as "built there" unless the cell also says "live run", which means a person ran it by hand against that host and the pull request carries the gated test or the notes.

Hosts: **tmux**, **cmux**, **herdr 0.9** (the `herdr` CLI, session server), **herdr-core** (`hc`, runs terminals), **herdr-app** (the window over herdr-core) and **Rex** (preview).

| Feature | tmux | cmux | herdr 0.9 | herdr-core (`hc`) | herdr-app | Rex |
| --- | --- | --- | --- | --- | --- | --- |
| Launch, probe and close a worker (`forgectl surface`) | · | · | · | live run on hc 0.1.21, by hand, two terminals (one finished before close, one still running): start, probe, close, second close all as documented; a finished terminal probes *gone* (gated test in #1428) | not a launch path; terminals run in herdr-core | live run on 0.1.0-preview.19, API revision 14, by hand: start, probe (running, then exit 7), close (gated test in #1425). Needs `rex` on `PATH` until the locator (#1418) merges |
| Desk "waiting" signal | · | · | notification and pane-blocked calls (existing; not re-measured) | OSC 7501 `blocked` built (#1417, stacked on #1402); not run on this host | OSC 7501 `blocked` built (#1417, stacked on #1402); not run on this host | OSC 7501 `blocked` built (#1417, stacked on #1402); not run on this host |
| `forgectl herdr organize` | · | · | existing path, unchanged (needs the fork's `tab move`) | n/a | through `herdr-app do tab.move` (#1419): reorders tabs within a space; a move between spaces is reported as unsupported; not run against a live window | n/a |
| Status of `workflow run` and `desk watch` (OSC 7501 `working`, `done`, `error`) | built (#1402): sent only when stderr is a terminal and `FORGECTL_WORKER` and `FORGECTL_DRAIN_WORKER` are unset; through tmux it needs `allow-passthrough on`; no host observed | built (#1402), same gates; no host observed | built (#1402), same gates; no host observed | built (#1402), same gates; no host observed | built (#1402), same gates; no host observed | built (#1402), same gates; no forgectl report observed. A hand-sent OSC 7501 record was read back with `block call program_status` (preview.19) |

## Versions this was measured on

- herdr-core `hc` 0.1.21 (the adapter's parsers were written against 0.1.15; the 0.1.21 comparison found additive drift only: cameronsjo/forgectl#1408).
- Rex 0.1.0-preview.19, API revision 14; the adapter refuses any other revision and any server shared with tailnet members.
- herdr-app 0.1.15: `tab.move` exists as a stock command; it has no notification command.

## What is still owed

- The three-host check of the OSC 7501 report (what each host does with `working`, `done` and `blocked`, including whether herdr clears `working` at process exit) needs a person at the keyboard: cameronsjo/forgectl#1404.
- `organize` against a live herdr-app space: cameronsjo/forgectl#1412.
- The Rex locator and the commands that act on a running Rex worker (list, close, brief, ready): cameronsjo/forgectl#1418, #1422, #1423 and #1424.
- tmux, cmux and herdr 0.9 cells are blank because this table was started from the herdr-core and Rex work; their behavior is documented on the command pages and was not re-measured here.

Update a cell only after running it, and change its version in the same edit.
