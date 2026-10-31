## Measurements

On a scratch hc core (its own `HC_SESSION` and XDG directories, hc 0.1.15):

- Every terminal carries `HC_SESSION`, `HC_TERMINAL_ID`, `HC_TERMINAL_SOCKET` and `HC_SCOPE`. `HC_SCOPE` differed between two terminals of one core; `HC_TERMINAL_SOCKET` is the core's socket path.
- A terminal started from inside terminal `t1` with that environment reported `parent: t1` and finished when `t1` was killed. Started with `HC_TERMINAL_ID`, `HC_TERMINAL_SOCKET` and `HC_SCOPE` unset, it reported `parent: null` and stayed live after `t1` was killed, with or without `--outlive-parent`. So the scrub on every hc call makes a worker no terminal's child; `hc run --outlive-parent` remains as a second guarantee.
- `HC_SCOPE` alone, without `HC_TERMINAL_ID`, changed neither what `hc ls` listed nor the parent of a terminal `hc run` started.

herdr's source sets `HERDR_ACTIVE_WORKSPACE_ID`, `HERDR_ACTIVE_TAB_ID`, `HERDR_ACTIVE_PANE_ID` and `HERDR_ACTIVE_PANE_CWD` only in the environment of a custom command (a keybinding's command). No pane's own environment carries them.

## Decisions

- **Two directions, one table.** The reverse check walks the hc scrub's own key and prefix list, so a key added to it alone fails until a launch strips it or `launchKeepsHcDrops` records why a launch keeps it. The kept set is Claude Code's own variables beyond the child marker and inbox (its configuration, which a harness needs), `HC_TERMINAL_SOCKET` (shared like `HERDR_SOCKET_PATH`), and a worker's identity, including `FORGECTL_DRAIN_WORKER`, whose containment must survive a launch.
- **`HC_SCOPE` is identity, not a server name.** It differs per terminal, so a surface launched from an hc terminal must not carry it. An hc worker gets its own terminal's value back from the trampoline, like `HC_TERMINAL_ID`.
- **The watch makes no ownership check.** It reads by name from a workers file or by the `cos-` prefix, has no ledger reference to compare, and never acts on a terminal. What it shares with the worker verbs is the process seam: the absolute hc path, the scrub, sealed argv, and output caps.
- **Events through a scrubbed stream.** The sensitive runner bounds each call, and `hc events` runs for as long as the view is open, so the subscription runs through `OSRunner.RunStreamingScrubbed`, which removes the same keys.
