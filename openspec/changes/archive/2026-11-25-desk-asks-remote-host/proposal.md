## Why

The desk directory is local to each machine, so an ask recorded on a laptop sits on that laptop, and the laptop sleeps. The operator wants one always-on machine to hold the asks log for every machine. Sessions elsewhere must post and read asks there. Staged scripts stay local: they run where they are staged.

## What Changes

- New variable `FORGECTL_DESK_ASKS_HOST` (an ssh host alias or hostname). When it is set and no `--dir`, `DESK_DIR` or `CLAUDE_DESK_DIR` names a local desk, `desk ask`, `desk asks [ASK-ID]`, `desk respond` and `desk watch --responses` run on that host over ssh.
- `FORGECTL_DESK_ASKS_REMOTE_BIN` names the binary there (default `forgectl` on the remote PATH).
- Transport is `ssh -o BatchMode=yes -T -- HOST` run directly, with no local shell. Every remote word is single-quoted for the remote POSIX login shell, and the remote command clears `FORGECTL_DESK_ASKS_HOST` so the host uses its own desk.
- The host and the binary are validated before use; a value of the wrong shape is a usage error.
- Steps are read locally and sent on ssh's stdin. `--from` is resolved locally. `--notify` is refused, because the hc terminal is local.
- The remote output streams unchanged and its exit code passes through. An ssh failure (255) names the host and says nothing was recorded.

## Capabilities

### Modified Capabilities

- `desk`: asks can be held on another host.

## Impact

- `internal/cli/desk_asks_remote.go`, `internal/cli/desk_asks_remote_unix.go` (new), `internal/cli/desk_ask_unix.go`, `internal/cli/desk.go`, `docs/commands/desk.md`, `README.md`, the bundled skill's desk reference.
- New outbound call: `ssh` to the configured host. No new listener.
