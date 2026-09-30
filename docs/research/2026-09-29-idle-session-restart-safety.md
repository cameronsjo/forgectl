# Is an idle Claude Code session safe to restart?

Findings for forgectl#725 (roadmap item 1 of `docs/plans/2026-09-29-harness-update-hooks-roadmap.md`). Measured 2026-09-29 on macOS against Claude Code 2.1.285 (`--model haiku`) and herdr 0.9.1. The throwaway session ran in a separate tmux server (`tmux -L ccprobe`), with its shell started under `env -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT -u CLAUDE_CODE_SESSION_ID zsh -f`.

## Answer

No, not on its own. `status: idle` in `~/.claude/sessions/<pid>.json` means no turn, tool call, shell command, or permission prompt is in progress. It says nothing about text typed into the input box and not yet sent. Every stop method measured drops that text, and it is recorded nowhere. A restart must also read the pane's input line.

## Registry `status` values observed

| State | `status` | `updatedAt` moves? |
|---|---|---|
| Fresh session, empty input | `idle` | — |
| Text typed, not sent | `idle` | no |
| Model turn in progress | `busy` | yes |
| Permission prompt waiting for an answer | `waiting` | yes |
| Turn finished, background Bash task still running | `shell` | yes |
| `!` shell command running | `busy` | yes |
| Resumed session taking an automatic turn (see below) | `busy` | yes |

`waiting` and `shell` were not known when forgectl#726 was written. The "unknown status means busy" rule held up, and only the exact value `idle` should ever be restartable.

## Stopping

| Method | Result |
|---|---|
| SIGTERM | Exits at once. Registry file removed. Transcript kept. Prints `claude --resume <id>`. Pane returns to its shell in the same cwd. |
| SIGINT (the signal, not a keypress) | Same as SIGTERM, on the first signal, even with a draft present. |
| Typing `/exit` | Not used. Text sent to the pane is appended to any existing draft, so it cannot be done safely. |

Unsent input is lost under both signals. After `claude --resume <id>` the draft is gone, and it appears in neither the transcript nor `~/.claude/history.jsonl`.

SIGTERM also kills the session's background shells (`sleep 90` started with `run_in_background` died with it). A `shell` session must never be restarted.

## Resuming

`claude --resume <id>` in the same pane brings back the same `sessionId` and the conversation. It can also start a model turn by itself: after the background shell was killed, the resumed session immediately took a turn to report it (`busy` for about 20 seconds). A restart tool should expect this and not treat it as a failure.

## Mapping a session to its pane

- `herdr pane list` / `herdr pane get <id>` report `agent_session.value` (the Claude session id) and `agent_status` (`working` / `idle` / `done` / `blocked`) for each pane. That is a cheaper join than reading process environments with `ps eww`.
- **Both sources can be wrong in the same way.** The probe session inherited the parent's `HERDR_PANE_ID`, and its hooks re-labelled the parent's herdr pane with the probe's session id. `ps eww` on the probe would have named the same wrong pane. The pane must be confirmed from both sides: registry pid → `HERDR_PANE_ID`, and herdr pane → `agent_session.value` equal to the registry `sessionId`. On a mismatch, report the session and leave it alone.
- `herdr pane read <id> --source visible` returns the screen. The input line is the `❯` line between the two horizontal rules above the status line; an empty draft renders as `❯` with nothing after it.

## The predicate forgectl#727 should implement

A session is safe to restart only when all of these hold, re-checked immediately before the signal:

1. The registry pid is alive and is still the `claude` process that wrote the file (compare `procStart` to guard against pid reuse).
2. `status` is exactly `idle`.
3. The herdr pane from the process environment reports the same `sessionId`.
4. The pane's visible input line is empty.

Stop with SIGTERM, wait for the pid to exit and the registry file to disappear, then run `forgectl resume <sessionId>` in the pane. A session that fails 2 or 4 is queued and checked again. A session that fails 1 or 3 is reported with the command to run by hand.

## Not measured

- `herdr pane run` into a real herdr pane after its `claude` exits. The tmux equivalent returns to a shell prompt in the session's cwd, and running it in herdr would have meant splitting a live layout. Measure it in the first herdr test for #727.
- Multi-line drafts and pasted-image drafts in the input box.
- Background subagents and agent-team teammates. They were not started, so their `status` is unknown.
