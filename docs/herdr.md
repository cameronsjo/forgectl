# herdr

`internal/herdr` is forgectl's client for the herdr session the process runs in. It shells to the `herdr` CLI through `exec.Runner`. It is plumbing for the herdr commands tracked in cameronsjo/forgectl#721; the first command to use it is [`forgectl herdr organize`](commands/herdr.md). Starting a harness on a pinned server stays in `internal/surface/herdradapter`.

## Requirements

- **A herdr pane.** `CheckSession` requires `HERDR_ENV=1` and a `HERDR_SOCKET_PATH` that names an existing socket. herdr exports both into every pane it hosts. It runs no herdr command, so a read-only caller needs nothing else.
- **The `cameronsjo/herdr` fork for `tab move`.** Upstream herdr has no `tab move`, and the fork hides it from `herdr tab --help`. `CheckFork` runs `herdr tab move --help` and accepts only a usage line at the start of a line (`usage: herdr tab move ...`, matched case-insensitively so a clap-style `Usage:` also passes). Anything else returns `ErrForkRequired`. Run it after `CheckSession`, before a mutation.
- **`CheckFork` checks the CLI, not the server.** A fork CLI talking to a server still running an older binary passes the check and fails at `MoveTab` with a typed `*herdr.Error`. Restart the herdr server after upgrading the binary. The client does not translate that server error into `ErrForkRequired`: herdr's code for a missing verb could not be measured without a stock binary.
- **`CheckSession` reads the environment you give it.** Pass a lookup that matches the environment of the `Runner` you will use. If the `Runner` pins a different `HERDR_SOCKET_PATH`, the lookup must reflect that pin, or the gate checks the wrong socket.
- **Call `CheckFork` once.** It spawns `herdr` each time, and the answer does not change within a process.

## Failure shapes

| What happens | What the client returns |
|---|---|
| herdr fails (exit 1, JSON on stderr) | `*herdr.Error{Code, Message}`; match on `Code` (`workspace_not_found`, `pane_not_found`, `server_not_running`, ...). It unwraps to the `*exec.CommandError` |
| herdr exits 0 but its reply is the error envelope | the same `*herdr.Error`, unwrapping to nil because no command failed. This holds for every call that reads its reply, including `MoveWorkspace`, `FocusWorkspace`, and `FocusTab` |
| stderr is truncated, has log lines before the JSON, or is not JSON | the wrapped `*exec.CommandError` (never an `*Error` with an empty code) |
| the child was killed or timed out (exit -1), even with an error object on stderr | the wrapped `*exec.CommandError`, never an `*Error`. The runner reports a kill as `signal: killed`, not as `context.DeadlineExceeded`, so tell a timeout by checking `ctx.Err()` |
| `tab move` exits 0 with `move_result.changed=false` | `*herdr.Declined{Reason}`, for example `last_tab_in_workspace` |
| a move reply that lacks `move_result` (except an index move), lacks `changed`, or names no tab or workspace | an error; `MoveTab` fails closed |
| the response has no `result`, or a list reply lacks its list (`panes`, `tabs`, ...) | an error. A renamed key must not read as an empty session; an empty `[]` is fine |

`pane read` is the one call that prints raw text on success, so `ReadPane` returns stdout without decoding it. `exec.Runner` trims trailing newlines, so trailing blank terminal rows are not preserved. The text is whatever another pane displays: it can hold secrets typed or printed there, and terminal control sequences, so do not log it or render it to a terminal unfiltered.

`Error.Error()` and `Declined.Error()` escape control characters in herdr's text, because herdr can echo pane-controlled values (labels, titles) in a message. `Code` stays as herdr sent it; `Message` and `Reason` are stored redacted.

`MoveWorkspace`, `FocusWorkspace`, and `FocusTab` read only herdr's error envelope from a successful exit: no success reply for them has been captured. If herdr declines one quietly the way `tab move` does (`changed:false`), the client cannot report it yet.

## Shared wire decoding

`internal/herdr/wire` is the one reader of herdr's reply envelopes, shared by this client and `internal/surface/herdradapter`: the error envelope (`DecodeError`), the `result` envelope (`DecodeResult`, where a missing or null `result` fails closed), and the operand floor (`CheckOperand`: non-empty, no leading `-`, at most 64 bytes, valid UTF-8, and none of the control, bidi, or invisible characters the hub picker refuses, except ZWJ, ZWNJ, and the VS15/VS16 presentation selectors, which real labels carry in emoji and Indic or Persian text). Envelope keys (`error`, `code`, `message`, `result`) match by exact name, and so do the result's own top-level members: a case-folded `ERROR` or `Workspaces` fails closed rather than reading as herdr's key. `CheckOperand` checks the client's ids and labels, the adapter's session name (which also has to fit its own `[A-Za-z0-9._-]` charset), and the `[herdr.organize]` labels in config, so a dry run cannot promise a move the client then refuses. The adapter's workspace, tab, and pane ids go through `backend.validHerdrID` instead, which is stricter (printable ASCII only). Both packages' tests run against the captured fixtures in `internal/herdr/testdata`, including the `changed_*_envelope.json` pair, so a change to herdr's envelope shows up in both.

## Ids move

Moving a tab between workspaces renumbers the tab and its panes. `MoveResult.TabID` is the id after the move; the id you passed in is stale. It is not known whether herdr reuses a freed id, so re-list after every mutation and resolve a target by `terminal_id` immediately before acting on it. The one measured exception is an index move (`tab move --index`): it keeps every tab id, and its reply carries the workspace's tab list in the new order, so that list can stand in for a re-list as long as it holds the moved tab (`testdata/move_index.json`). `terminal_id` is stable across moves. Only `Panes()` and `Agents()` carry it (with the pane's `tab_id`); `Tabs()` and `TabGet()` do not, so join through `Panes()`.

There is no `FocusPane`. herdr's `pane focus` is directional only, so `FocusTab` is the finest focus grain available by id.

## Logging

`exec.OSRunner` logs any non-zero exit at `ERROR`, including the probe's expected exit 2. A caller that runs `CheckFork` will see one `ERROR` line per call even on success.

## Testing

Unit tests replay fixtures captured from a live session and sanitized with an allowlist (`internal/herdr/testdata`). One test checks that every fixture joins on its ids and carries no live values. To check the client against the real session, from inside a herdr pane:

```bash
HERDR_LIVE=1 go test ./internal/herdr/ -run TestLiveSession -v
```

That test is read-only.

## Worker readiness

`internal/herdr/ready` decides whether a coordinator worker's harness is at its input prompt, for `forgectl surface ready`. It is a pure function of three inputs: the pane's visible text, the agent herdr detects in the pane, and herdr's `agent_status`. A harness is ready only when no blocking screen matches, herdr names the expected agent as `idle` or `done`, and the harness's own prompt pattern matches. herdr's status alone is a hint: it reports `blocked` at an idle prompt and `done` until someone views the pane.

The predicates are TOML (`internal/herdr/ready/predicates.toml`), compiled into the binary and replaceable by `<config dir>/surface-ready.toml` (see [configuration.md](configuration.md)). Blocking patterns are broad, because a false match only makes the worker wait. Prompt patterns are strict, because a false match lets forgectl type into a dialog. The fixtures in `internal/herdr/ready/testdata` are live captures from Claude Code 2.1.289, Codex 0.160.0 and npm on herdr 0.9.1; rows the TOML marks `uncaptured` have no fixture yet.

Reading the worker's pane is not this package's job. `internal/surface/herdradapter`'s `WorkerScreen` does it on the pinned server, through the sensitive runner (`herdr.screen-read` and `herdr.pane-status`), and only for a pane forgectl owns: the workspace must carry forgectl's ownership marker, the pane must be the root pane its create response named, and `pane get` must place that pane in that workspace. A workspace that is provably absent returns `ErrWorkerGone`; any other failure returns `ErrScreenUnreadable`, which is never a verdict about the worker.

## Briefs, waits and reports

A worker's first brief goes in at launch. `surface launch --brief` appends it to the harness argv after `--`, and the trampoline socket carries that argv to the pane, so the brief is never typed into a running TUI. The brief then stays in the harness's process arguments for the whole session, where `ps -ww` can read it, so a launch brief must not hold a secret. Both harnesses treat what follows `--` as the prompt: Claude Code 2.1.289 answered `-- mcp` as a prompt rather than running the `mcp` subcommand, and Codex 0.160.0 took `-- --help` as one.

`surface brief` types the follow-ups. It writes through `TypeText` (`herdr.send-text`) and `PressEnter` (`herdr.send-keys`). Each call re-runs the ownership check `WorkerScreen` uses, so forgectl types only into the root pane of a workspace it owns. Three herdr and Claude Code facts shape it, measured on herdr 0.9.1 and Claude Code 2.1.289:

- `pane send-text` does not honour `--`; it types the `--`. A text starting with `-` types correctly without one. The sensitive runner still refuses such an operand, so `CheckBrief` refuses a typed brief that starts with `-`.
- Claude Code collapses typed text between 700 and 900 characters into `[Pasted text #N]`, which the read-back cannot compare, so a typed brief is capped at 600 characters with its report instruction.
- herdr reported `working` within 0.4 s of Enter, and `done` when the turn ended.

Every brief carries a random 12-character marker, recorded in the ledger row before the brief is sent. The brief spells the REPORT line out in words, so its own echo never matches `REPORT <marker>:`. `worker.FindReport` also takes only report lines below the last line that names the marker. A report is still the worker's claim, and the coordinator checks it against git.

`surface wait` does not compare screen text for stability, because a harness's status line and any mod drawing above the prompt change every second. It needs the ready verdict to hold for `--settle`, plus one of: a `working` status seen during the wait, the report on screen, or `--quiet` at the prompt.

## What a claude worker loads

A claude worker loads only what forgectl gives it (ADR-0010, forgectl#1050). Its argv carries:

- **`--setting-sources ""`:** no user, project or local settings load. The branch's `.claude/settings.json` hooks do not run, and the operator's plugins, hooks and skills do not load. Only Claude Code's built-in plugins, skills and agents remain.
- **`--strict-mcp-config` with an empty `--mcp-config`:** no MCP server loads. That covers the branch's `.mcp.json`, the operator's servers, and plugin servers, a herdr-driving one included.
- **`--no-chrome`:** turns off Claude in Chrome. It is enabled from `~/.claude.json`, so the flags above leave it on.
- **`--safe-mode`:** stops the worker loading the operator's project auto-memory (`MEMORY.md` under `~/.claude/projects`), which every session writes and the operator's later sessions read. It does not stop a write there: that path is outside the worktree, so under the allowed modes a write prompts. It keeps the `--settings` deny rules (measured with `claude -p`: with the deny, neither tool is listed; without it, both are). It also sets `CLAUDE_CODE_SAFE_MODE` and `CLAUDE_CODE_DISABLE_CLAUDE_MDS`, so a `claude` the worker runs from its shell starts in safe mode too.
- **No `--ide`:** the worker does not ask to connect to the operator's editor.
- **An environment allowlist:** a worker inherits only `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `TERM`, `COLORTERM`, `LANG`, `LC_*`, `TZ`, `TMPDIR`, `CLAUDE_CONFIG_DIR`, the CA and config-home paths (`NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, `CODEX_HOME`, `XDG_CONFIG_HOME`) and `SSH_AUTH_SOCK`, plus herdr's pane ids. `SSH_AUTH_SOCK` is a deliberate grant: the remotes push over SSH, so it stays until a per-worker GitHub token replaces the operator's identity. When the launcher is a Claude Code session, the rest of its environment carries its user settings' `env` block and its own handles: the cross-session messaging socket and token, and the herdr and cmux sockets. The profile's `env` and forgectl's injected values still apply, so a variable a worker needs goes in config. This removes the handles from the worker's environment, not its access to them: until the sandbox slice, a worker's shell can still find the sockets by path and read another process's environment with `ps -E`. Under the worker modes allowed today, every such shell command prompts.
- **`--settings`:** sets `useAutoModeDuringPlan: false` (forgectl#1060) and denies `SendMessage` and `RemoteTrigger`. Without the deny, a worker could message another Claude session on the machine, the coordinator included, or start a cloud session.

These are measurements on Claude Code 2.1.289, not tests: a unit test pins the argv, the settings JSON and the environment allowlist, and nothing re-checks the behavior on a Claude Code upgrade. With `claude -p` in a repo carrying a hook, an MCP server, a skill, a subagent that declares an MCP server, and a command, none of them loaded. A live herdr worker listed no MCP server, its tool list held neither denied tool, and its process environment held only the allowlist, forgectl's injected values and the pane ids. The built-in plugins' contents are not checked.

Instruction files: a live worker on the argv above without `--safe-mode` loaded no `CLAUDE.md` or `AGENTS.md` at any level, including one the branch committed; its `instructions` record listed only the operator's project auto-memory. With `--safe-mode` added, the record was empty. `claude -p` without `--safe-mode` did load the cwd's `CLAUDE.md`. These are session-start records: a `CLAUDE.md` in a subdirectory loads lazily, and is covered only by `CLAUDE_CODE_DISABLE_CLAUDE_MDS`, read from the 2.1.289 binary, not measured. A codex worker gets only the environment allowlist so far (forgectl#1092).

## Where a worker branch starts

A `--worktree` branch that does not exist yet starts at the head of the repository's GitHub default branch (forgectl#1061), not at the checkout's `HEAD`, which is often stale or on another branch. forgectl reads the branch and its head commit from the GitHub API, fetches that branch from `origin`, checks the fetched commit is the one GitHub named, and passes the hash to `git worktree add`. When `origin` is not a github.com repository, the branch starts at the checkout's `HEAD` as before. An existing branch is checked out as it is, and needs no GitHub call.

This is a correctness fix, not a security control. Workers in one repository share its `.git` and are mutually trusting (ADR-0010): an earlier worker can change what the next one starts from.

## Listing and closing workers

`surface list` probes each ledger row's workspace through `Adapter.Probe`, the same lookup `Close` uses: the server incarnation must match the reference, the listing must be complete, and the workspace must carry forgectl's ownership marker. A workspace missing from that listing is `gone`. Any herdr error is `unreadable`, and so is an identity mismatch: after a herdr restart, or when a workspace id now names a workspace without forgectl's marker, nothing proves forgectl's own workspace gone. `--orphans` keeps the rows `close` should act on: a `gone` workspace, or any stage other than `launched` (`failed`, `closed`, or a launch stopped at `pending` or `worktree`). A launch at `pending` or `worktree` for less than ten minutes may still be running, so it is never an orphan, and `close` refuses it.

A claude worker is started with a forgectl-generated `--session-id`, and its row records the transcript path Claude Code writes: `<CLAUDE_CONFIG_DIR or ~/.claude>/projects/<slug>/<id>.jsonl`, where the slug is the worktree path with every character other than an ASCII letter or digit replaced by `-`. The path comes from the environment the harness was started with. Codex workers have neither field.

`surface close` closes the workspace first, so the harness stops before its worktree is judged. It refuses, touching nothing, when the close comes back unreadable, failed, or an identity mismatch. A row at stage `closed` skips herdr: an earlier close already closed its workspace. It then removes the worktree only when `git status --porcelain --ignored` is empty, HEAD is on a branch, the branch has nothing beyond its upstream (or, with none, beyond the commit the worktree started at), and no stash entry names the branch. The path comes from `git worktree list`, not the ledger, and `git worktree remove` runs without `--force`, so git refuses on its own if a tracked or untracked file changed after the checks. Git does delete ignored files, so an ignored file written after the check is lost; closing the workspace first stops the harness that would write one. The ledger row is removed or marked `closed` only if it is still the row close read, so a launch that reused the name meanwhile is left alone. The branch is never deleted. A kept worktree leaves the row at stage `closed`; a later close retries it.
