# herdr

`internal/herdr` is forgectl's client for the herdr session the process runs in. It shells to the `herdr` CLI through `exec.Runner`. It is plumbing for the herdr commands on the roadmap (`docs/plans/2026-09-29-herdr-commands-roadmap.md`); no command uses it yet. Starting a harness on a pinned server stays in `internal/surface/herdradapter`.

## Requirements

- **A herdr pane.** `Probe` requires `HERDR_ENV=1` and a `HERDR_SOCKET_PATH` that names an existing socket. herdr exports both into every pane it hosts.
- **The `cameronsjo/herdr` fork for `tab move`.** Upstream herdr has no `tab move`, and the fork hides it from `herdr tab --help`. `Probe` runs `herdr tab move --help` and accepts only a real usage line (`usage: herdr tab move ...` at the start of a line). Anything else returns `ErrForkRequired`.
- **`Probe` checks the CLI, not the server.** A fork CLI talking to a server still running an older binary passes the probe and fails at `MoveTab` with a typed `*herdr.Error`. Restart the herdr server after upgrading the binary.

## Failure shapes

| What happens | What the client returns |
|---|---|
| herdr fails (exit 1, JSON on stderr) | `*herdr.Error{Code, Message}`; match on `Code` (`workspace_not_found`, `pane_not_found`, `server_not_running`, ...) |
| stderr is truncated, has log lines before the JSON, or is not JSON | the wrapped `*exec.CommandError` (never an `*Error` with an empty code) |
| `tab move` exits 0 with `move_result.changed=false` | `*herdr.Declined{Reason}`, for example `last_tab_in_workspace` |
| the response has no `result` | an error; the client fails closed |

`pane read` is the one call that prints raw text on success, so `ReadPane` returns stdout as-is.

## Ids move

Moving a tab between workspaces renumbers the tab and its panes. `MoveResult.TabID` is the id after the move; the id you passed in is stale. It is not known whether herdr reuses a freed id, so re-list after every mutation and resolve a target by `terminal_id` immediately before acting on it. `terminal_id` is stable across moves.

There is no `FocusPane`. herdr's `pane focus` is directional only, so `FocusTab` is the finest focus grain available by id.

## Logging

`exec.OSRunner` logs any non-zero exit at `ERROR`, including the probe's expected exit 2. A caller that runs `Probe` will see one `ERROR` line per call even on success.

## Testing

Unit tests replay fixtures captured from a live session and sanitized with an allowlist (`internal/herdr/testdata`). One test checks that every fixture joins on its ids and carries no live values. To check the client against the real session, from inside a herdr pane:

```bash
HERDR_LIVE=1 go test ./internal/herdr/ -run TestLiveSession -v
```

That test is read-only.
