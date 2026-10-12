# Paperclip integration

A pnpm workspace, outside the Go build, that connects [Paperclip](https://github.com/paperclipai/paperclip) to `forgectl surface`. Paperclip decides what runs and when; forgectl starts, briefs, waits on and reads the pane. Status and results: [`docs/plans/2026-10-12-paperclip-components.md`](../../docs/plans/2026-10-12-paperclip-components.md).

## `herdr_forgectl` adapter

`packages/adapter-herdr` is a Paperclip server adapter. Each `execute()` is one turn in a long-lived herdr worker:

1. No live worker for the agent and task: `forgectl surface launch <repo> --surface herdr --worktree paperclip/<name> --name <name> --harness <claude|codex> [--profile <p>]`, then `surface ready`.
2. Write the brief to `${XDG_STATE_HOME:-~/.local/state}/forgectl-paperclip/briefs/<run>.md` (0600 in a 0700 dir; issue text fenced with a per-turn nonce) and `surface brief` the worker to read that absolute path. Nothing is written into the worker's worktree, which the worker can write and race.
3. The turn ends on the worker's Claude Code `Stop` hook (claude), or on `surface wait` plus the brief's REPORT line (codex). The hook reads a turn file the adapter keeps outside every worktree, `${XDG_STATE_HOME:-~/.local/state}/forgectl-paperclip/turns/<claude session id>.json`, and POSTs the run id and a one-time token to `http://127.0.0.1:<port>/stop`. The adapter accepts it only for the worker's own session. A dialog that appears meanwhile fails the run as `worker_blocked`.
4. `surface read --report` becomes the run summary; Claude usage is summed from the transcript path forgectl recorded, capped at 32 MiB per turn. The turn files are removed on every exit.

The worker runs as the operator and can read the turn file during its turn, so the token proves the callback came from a local process during this turn, not that it came from the hook. That is accepted for a loopback, single-operator setup.

Build and test:

```bash
pnpm install
pnpm --filter @forgectl/paperclip-adapter-herdr build   # one bundled dist/index.js
pnpm --filter @forgectl/paperclip-adapter-herdr test
```

Register it in `$PAPERCLIP_HOME/adapter-plugins.json`:

```json
[{"packageName": "@forgectl/paperclip-adapter-herdr", "localPath": "<abs path>/integrations/paperclip/packages/adapter-herdr", "type": "herdr_forgectl", "installedAt": "2026-10-12T00:00:00.000Z"}]
```

Agent `adapterConfig`: `repo` (absolute path, required), `harness` (`claude` or `codex`), `profile` (a `[surface.profiles]` name, claude only), `herdrSession` (default `paperclip`), `callbackPort` (default `3199`), `turnTimeoutSec` (30-86400, default `1800`), `forgectlPath` (`forgectl` or an absolute path). pi agents use Paperclip's built-in `pi_local` adapter, because forgectl refuses pi as a worker.

## Worker repo settings

The repo a worker runs in needs, in its committed `.claude/settings.json`:

```json
{
  "hooks": { "Stop": [{ "hooks": [{ "type": "command", "command": "\"$CLAUDE_PROJECT_DIR\"/.claude/hooks/paperclip-stop.sh", "timeout": 10 }] }] },
  "promptSuggestionEnabled": false,
  "skillOverrides": { "auto-mode-setup": "off" }
}
```

plus `.claude/hooks/paperclip-stop.sh` (copy `packages/adapter-herdr/hooks/paperclip-stop.sh`; it needs `jq` and `curl`). The hook is a file in the worktree, so a worker can edit it during a turn; that grants nothing while workers run unsandboxed as the operator, and moving it out of the worktree is tracked in forgectl#1244. Turn interactive plugins off for workers with `enabledPlugins` until forgectl supports a curated worker profile. Each harness account must trust the repo root once.
