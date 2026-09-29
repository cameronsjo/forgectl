# herdr

`internal/herdr` is forgectl's client for the herdr session the process runs in. It shells to the `herdr` CLI through `exec.Runner`. It is plumbing for the herdr commands tracked in cameronsjo/forgectl#721; no command uses it yet. Starting a harness on a pinned server stays in `internal/surface/herdradapter`.

## Requirements

- **A herdr pane.** `Probe` requires `HERDR_ENV=1` and a `HERDR_SOCKET_PATH` that names an existing socket. herdr exports both into every pane it hosts.
- **The `cameronsjo/herdr` fork for `tab move`.** Upstream herdr has no `tab move`, and the fork hides it from `herdr tab --help`. `Probe` runs `herdr tab move --help` and accepts only a real usage line (`usage: herdr tab move ...` at the start of a line). Anything else returns `ErrForkRequired`.
- **`Probe` checks the CLI, not the server.** A fork CLI talking to a server still running an older binary passes the probe and fails at `MoveTab` with a typed `*herdr.Error`. Restart the herdr server after upgrading the binary. The client does not translate that server error into `ErrForkRequired`: herdr's code for a missing verb could not be measured without a stock binary.
- **`Probe` reads the environment you give it.** Pass a lookup that matches the environment of the `Runner` you will use. If the `Runner` pins a different `HERDR_SOCKET_PATH`, the lookup must reflect that pin, or the gate checks the wrong socket.
- **Call `Probe` once.** It spawns `herdr` each time, and the answer does not change within a process.

## Failure shapes

| What happens | What the client returns |
|---|---|
| herdr fails (exit 1, JSON on stderr) | `*herdr.Error{Code, Message}`; match on `Code` (`workspace_not_found`, `pane_not_found`, `server_not_running`, ...). It unwraps to the `*exec.CommandError` |
| stderr is truncated, has log lines before the JSON, or is not JSON | the wrapped `*exec.CommandError` (never an `*Error` with an empty code) |
| the child was killed or timed out (exit -1), even with an error object on stderr | the wrapped `*exec.CommandError`, so `errors.Is(err, context.DeadlineExceeded)` still works |
| `tab move` exits 0 with `move_result.changed=false` | `*herdr.Declined{Reason}`, for example `last_tab_in_workspace` |
| a move reply that lacks `move_result` (except an index move), lacks `changed`, or names no tab or workspace | an error; `MoveTab` fails closed |
| the response has no `result`, or a list reply lacks its list (`panes`, `tabs`, ...) | an error. A renamed key must not read as an empty session; an empty `[]` is fine |

`pane read` is the one call that prints raw text on success, so `ReadPane` returns stdout without decoding it. `exec.Runner` trims trailing newlines, so trailing blank terminal rows are not preserved. The text is whatever another pane displays: it can hold secrets typed or printed there, and terminal control sequences, so do not log it or render it to a terminal unfiltered.

`Error.Error()` and `Declined.Error()` drop control characters from herdr's text, because herdr can echo pane-controlled values (labels, titles) in a message. The `Code` and `Message` fields stay as herdr sent them.

`MoveWorkspace`, `FocusWorkspace`, and `FocusTab` do not decode herdr's reply. If herdr declines one quietly the way `tab move` does, the client cannot report it.

## Ids move

Moving a tab between workspaces renumbers the tab and its panes. `MoveResult.TabID` is the id after the move; the id you passed in is stale. It is not known whether herdr reuses a freed id, so re-list after every mutation and resolve a target by `terminal_id` immediately before acting on it. `terminal_id` is stable across moves. Only `Panes()` and `Agents()` carry it (with the pane's `tab_id`); `Tabs()` and `TabGet()` do not, so join through `Panes()`.

There is no `FocusPane`. herdr's `pane focus` is directional only, so `FocusTab` is the finest focus grain available by id.

## Logging

`exec.OSRunner` logs any non-zero exit at `ERROR`, including the probe's expected exit 2. A caller that runs `Probe` will see one `ERROR` line per call even on success.

## Testing

Unit tests replay fixtures captured from a live session and sanitized with an allowlist (`internal/herdr/testdata`). One test checks that every fixture joins on its ids and carries no live values. To check the client against the real session, from inside a herdr pane:

```bash
HERDR_LIVE=1 go test ./internal/herdr/ -run TestLiveSession -v
```

That test is read-only.
