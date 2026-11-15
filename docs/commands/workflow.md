# workflow

Runs a TOML step list: `forgectl workflow run <name> [--dry-run] [--resume] [--param k=v]`. A workflow outside the built-ins must be blessed first with `forgectl workflow bless <name>`. The design is [ADR-0001](../adr/0001-workflow-dsl-toml-step-list.md) (the step list), [ADR-0006](../adr/0006-workflow-blessing-user-presence-signing.md) (blessing).

## Program status (OSC 7501)

`desk` (the dashboard and `desk watch`), `workflow run`, and the `workflow bless` and `workflow trust` ceremonies report their state to the terminal over the Program Status Protocol, so cmux's sidebar and herdr's status line show them the way they show an agent. Reports go to stderr, so `--json` and piped stdout stay clean, and each carries `id=forgectl:app=forgectl`, so `clear` removes only forgectl's own row.

| Command | Reports |
|---|---|
| `workflow run` | `working` with `<name> <i>/<n> <verb>` and progress before each step (a resumed run shows `3/5`), `done` on success, `error` on any failure, or `error` reading `interrupted` on Ctrl-C or SIGTERM (forgectl still dies by the signal) |
| `desk` (the dashboard) | `blocked` with `kind=permission` and `<N> waiting` while any item waits for approval, cleared (`id=forgectl`) when none does and when the dashboard exits. Only outside a herdr session: inside one, the herdr notification and pane state described in [desk.md](desk.md) already signal it, and nothing is sent over OSC 7501. See the lifetime note below |
| `desk watch` | `blocked` with `kind=permission` while the item waits to start (pending, or claimed with no run yet), `working` with the step name once it runs, `done` on `RUN-END rc=0`, `error` on any other end, a lost or skipped item, or an interrupt. A `--deadline` expiry clears the row, because the run outlives the watch. `--responses` reports nothing |
| `workflow bless`, `trust init`, `trust rebuild` | `blocked` with `kind=auth` (a login or credential) and `Touch ID: <what>` while the presence prompt is open; `trust init` does the same for the `sudo` leg. The row is cleared when the call returns, on every path, and Ctrl-C or SIGTERM at the prompt leaves an `error` row reading `interrupted` |

- **Kinds:** a `blocked` report carries one of the protocol's three kinds. `permission` is an approval to do something (a waiting desk item), `question` is an answer the user must type, and `auth` is a login or credential. A host treats any other value as absent, so forgectl sends no `kind` for one.
- **Desk row lifetime:** the dashboard process holds its `blocked` row while it is open. `desk add` and the verbs that move an item out of pending run for a moment, and a host drops a `blocked` row when the reporting process exits, so a row set there would vanish at once. The dashboard is the one long-lived process that watches the queue: it sets the row when items wait, clears it when none do, and clears it on every return from the dashboard (quit, an error, a panic). Held and refused items are not counted.
- **Off switch:** set `FORGECTL_NO_PROGRAM_STATUS` to any non-empty value.
- **Off by itself** when stderr is not a terminal, `TERM=dumb`, or `FORGECTL_WORKER` or `FORGECTL_DRAIN_WORKER` is set. forgectl reads OSC 7501 from worker panes to decide readiness, so it never writes it inside one.
- **tmux:** reports are wrapped in tmux's passthrough, which needs `set -g allow-passthrough on`. Nested tmux is not handled.
- **A killed forgectl** (SIGKILL, a crash) leaves its last `working` row under tmux, since the terminal never sees the process exit. Clear it with `printf '\e]7501;state=clear:id=forgectl\e\\'`.
