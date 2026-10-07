# Desk: queue a script for the operator to run

`forgectl desk` is an operator queue. When you would otherwise ask the operator to copy and paste a command (a merge, `sudo`, a production or homelab change, anything a guard blocks or the operator owns), queue it here instead. The operator reads it on the dashboard and presses `y` to run it or `s` to skip it.

This skill ships with the binary, so these commands exist in the build that printed it. If `forgectl desk status` fails as an unknown command, the binary on `PATH` is older than the skill you read: say so, suggest upgrading, and hand the operator the script path. A handed-over file has no hash check.

## Find the desk the operator watches

Run `forgectl desk status` first. Its first line names the desk directory. The directory comes from `--dir`, else `$DESK_DIR`, else `$CLAUDE_DESK_DIR`, else `$XDG_STATE_HOME/forgectl/desk` (`~/.local/state/forgectl/desk`). If it differs from a directory the operator named, or you cannot confirm it, pass `--dir` or ask. A queue the operator is not watching waits forever.

## Queue it

1. Write the script to a scratch `.sh` file outside the repo, so it is never committed.
2. Queue it with one line on what it does and one on why it needs a person:

   ```bash
   forgectl desk add ./merge-1201.sh --what "Merge PR 1201 once checks are green" --why "You own merges"
   ```

   When a guard blocked the action, `--why` carries the first line of the guard's verdict, which names the guard, and the chat report quotes the whole verdict. `--why` must be one line, so never pass the full multi-line verdict. Wrap it in **single** quotes: verdicts contain backticks and `$`, which double quotes would run as commands, executing the very command the guard blocked. Delete any `'` from the text first. For example: `--why '🚫 git-guardrails: gh write command in loop without explicit -R flag'`. Queue a guard-blocked command as its own item, never as a step in a batch with unrelated work.

   Add `--tty` when the script needs a terminal (`sudo`, a password prompt); it then runs in the dashboard's own pane. `add` prints `name=`, `kind=` and `sha256=` lines and exits 0 only when queued.
3. For a multi-step batch, write a `.manifest` (one `<step> [after=a,b] [timeout=S] [private] -- <command>` per line). `add` plans it and refuses one that cannot run; `forgectl desk plan FILE` shows the waves and `warning:` lines first if you want them. A batch cannot take `--tty`.
4. Report in chat the desk directory, the item name, and the full `sha256=` line. The focus panel shows the same hash, so the operator can confirm the item they approve is the one you reported. The hash does not show the script does what `--what` says; the operator reads the script for that.

`add` never deduplicates. After an uncertain result (a timeout, lost context), run `forgectl desk status` and look for your item before running `add` again.

## Watch the run

NAME is the `name=` value exactly as `add` printed it (`01-merge-1201`, no extension). Arm the Monitor tool on:

```bash
forgectl desk watch NAME --deadline 540
```

A 540-second deadline keeps each watch short; re-arming continues it. Never sleep-poll. With no Monitor tool, report the name and stop.

| Code | Meaning | Do |
|---|---|---|
| 0 | the run ended with rc 0 | read the outcome |
| 1 | the run failed, was lost, or was skipped; or no such item | read the outcome |
| 75 | the deadline passed first | re-arm Monitor on the command after `resume=` on the last line (strip the prefix; keep `--skip` and `--dir` as printed) |
| 141 | stdout closed; the monitor went away | re-arm Monitor on the `resume=` command in the stderr error |
| other | usage error (2) or interrupted (130) | read stderr |

On 75, if `status` still shows the item `waiting`, re-arm. After three re-arms (about 30 minutes), tell the operator in chat the item is waiting and stop watching. Never skip it to unblock yourself.

Read the outcome with `forgectl desk status NAME --json` (exit code, times, log path, a batch's per-step results).

## What the desk guarantees

- The hash covers only the item's own bytes. It does not cover what the script sources, calls, or downloads (`source ./lib.sh`, a binary on `PATH`, `curl ... | sh`, files a manifest step reads). Put what the operator must review inside the item, and say in `--what` what it fetches.
- An item whose bytes change after queueing is skipped as `changed` and cannot be re-armed. Never edit a queued item. To replace one you have not edited, queue the replacement, then skip the old waiting item with `forgectl desk skip NAME --reason "<why>"`; `--reason` is required (one line, at most 200 characters).
- The desk defends against accidents, not a same-uid process. It is not a security boundary for you: never press keys in the desk pane, never approve your own item, and never `skip` an item the operator queued. `skip` is only for your own superseded items that are still waiting. A lost run may have partly executed, so clearing it is the operator's call.
- `private` batch steps reduce exposure in logs; they are not a secret store. Keep secrets out of desk items.
