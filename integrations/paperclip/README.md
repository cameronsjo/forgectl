# Paperclip integration

A pnpm workspace, outside the Go build, that connects [Paperclip](https://github.com/paperclipai/paperclip) to `forgectl surface`. Paperclip decides what runs and when; forgectl starts, briefs, waits on and reads the pane. Status and results: [`docs/plans/2026-10-12-paperclip-components.md`](../../docs/plans/2026-10-12-paperclip-components.md).

## `herdr_forgectl` adapter

`packages/adapter-herdr` is a Paperclip server adapter. Each `execute()` is one turn in a long-lived herdr worker:

1. No live worker for the agent and task: `forgectl surface launch <repo> --surface herdr --worktree paperclip/<name> --name <name> --harness <claude|codex> [--profile <p>]`, then `surface ready`.
2. Write the brief to `.paperclip/brief-<run>.md` in the worker's worktree (issue text fenced with a per-turn nonce) and `surface brief` the worker to read it.
3. The turn ends on the worker's Claude Code `Stop` hook, which POSTs the run id and a one-time token to a loopback listener (claude), or on `surface wait` plus the brief's REPORT line (codex). A dialog that appears meanwhile fails the run as `worker_blocked`.
4. `surface read --report` becomes the run summary; Claude usage is summed from the transcript lines the turn appended. The `.paperclip/` turn files are removed.

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

Agent `adapterConfig`: `repo` (required), `harness` (`claude` or `codex`), `profile` (a `[surface.profiles]` name, claude only), `herdrSession` (default `paperclip`), `callbackPort` (default `3199`), `turnTimeoutSec` (default `1800`), `forgectlPath`. pi agents use Paperclip's built-in `pi_local` adapter, because forgectl refuses pi as a worker.

## Worker repo settings

The repo a worker runs in needs, in its committed `.claude/settings.json`:

```json
{
  "hooks": { "Stop": [{ "hooks": [{ "type": "command", "command": "\"$CLAUDE_PROJECT_DIR\"/.claude/hooks/paperclip-stop.sh", "timeout": 10 }] }] },
  "promptSuggestionEnabled": false,
  "skillOverrides": { "auto-mode-setup": "off" }
}
```

plus `.claude/hooks/paperclip-stop.sh` (copy `packages/adapter-herdr/hooks/paperclip-stop.sh`) and `.paperclip/` in `.gitignore`. Turn interactive plugins off for workers with `enabledPlugins` until forgectl supports a curated worker profile. Each harness account must trust the repo root once.
