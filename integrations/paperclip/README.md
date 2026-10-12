# Paperclip integration

A pnpm workspace, outside the Go build, that connects [Paperclip](https://github.com/paperclipai/paperclip) to `forgectl surface`. Paperclip decides what runs and when; forgectl starts, briefs, waits on and reads the pane. Status and results: [`docs/plans/2026-10-12-paperclip-components.md`](../../docs/plans/2026-10-12-paperclip-components.md).

## `herdr_forgectl` adapter

`packages/adapter-herdr` is a Paperclip server adapter. Each `execute()` is one turn in a long-lived herdr worker:

1. No live worker for the agent and task: `forgectl surface launch <repo> --surface herdr --worktree paperclip/<name> --name <name> --harness <claude|codex> [--profile <p>]`, then `surface ready`.
2. Write the brief to `~/.local/state/forgectl-paperclip/briefs/<run>.md` (0600 in a 0700 dir; issue text fenced with a per-turn nonce) and `surface brief` the worker to read that absolute path. Nothing is written into the worker's worktree. The state dir is outside the worktree, not out of the worker's reach: an unsandboxed worker runs as the operator and can write it.
3. The turn ends on the worker's Claude Code `Stop` hook (claude), or on `surface wait` plus the brief's REPORT line (codex). The hook reads a turn file the adapter keeps outside every worktree, `~/.local/state/forgectl-paperclip/turns/<claude session id>.json`, and POSTs the run id and a one-time token to `http://127.0.0.1:<port>/stop`. The adapter accepts it only for the worker's own session. A dialog that appears meanwhile fails the run as `worker_blocked`.
4. `surface read --report` becomes the run summary; Claude usage is summed from the transcript path forgectl recorded, capped at 32 MiB per turn. The turn files are removed on every exit.

The worker runs as the operator and can read the turn file during its turn, so the token proves the callback came from a local process during this turn, not that it came from the hook. That is accepted for a loopback, single-operator setup. `XDG_STATE_HOME` is not used, because forgectl does not pass it to workers and the hook could not follow it.

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

Agent `adapterConfig` holds only per-agent choices: `repo` (required; must be one of `FORGECTL_PAPERCLIP_REPOS`), `harness` (`claude` or `codex`), `profile` (a `[surface.profiles]` name, claude only), `turnTimeoutSec` (30-86400, default `1800`). pi agents use Paperclip's built-in `pi_local` adapter, because forgectl refuses pi as a worker.

An agent can edit its own `adapterConfig` through Paperclip's API, so anything that decides what the server executes or where panes open is set in the Paperclip server's environment, and the adapter refuses it in `adapterConfig`:

| Variable | Meaning | Default |
|---|---|---|
| `FORGECTL_PAPERCLIP_REPOS` | `:`-separated absolute repo roots agents may use | required; unset means no repo is allowed |
| `FORGECTL_PAPERCLIP_FORGECTL` | absolute path of the forgectl binary | `forgectl` on `PATH` |
| `FORGECTL_PAPERCLIP_HERDR_SESSION` | herdr session for every pane | `paperclip` |
| `FORGECTL_PAPERCLIP_CALLBACK_PORT` | loopback port for the Stop-hook listener | `3199` |
| `FORGECTL_PAPERCLIP_PROFILES` | `:`-separated `[surface.profiles]` names agents may pick besides `main` | unset: any configured profile |

Under Paperclip's `local_trusted` mode any local process, a worker included, is the instance admin, so these controls bound mistakes, not a hostile worker. Run `authenticated` mode before relying on them.

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
