# Paperclip integration

A pnpm workspace, outside the Go build, that connects [Paperclip](https://github.com/paperclipai/paperclip) to `forgectl surface`. Paperclip decides what runs and when; forgectl starts, briefs, waits on and reads the pane. Status and results: [`docs/plans/2026-10-12-paperclip-components.md`](../../docs/plans/2026-10-12-paperclip-components.md).

## `herdr_forgectl` adapter

`packages/adapter-herdr` is a Paperclip server adapter. Each `execute()` is one turn in a long-lived herdr worker:

1. No live worker for the agent and task: `forgectl surface launch <repo> --surface herdr --worktree paperclip/<name> --name <name> --harness <claude|codex> [--profile <p>]`, then `surface ready`.
2. Write the brief to `~/.local/state/forgectl-paperclip/briefs/<run>.md` (0600 in a 0700 dir; issue text fenced with a per-turn nonce) and `surface brief` the worker to read that absolute path. Nothing is written into the worker's worktree. The state dir is outside the worktree, not out of the worker's reach: an unsandboxed worker runs as the operator and can write it.
3. The turn ends on the worker's Claude Code `Stop` hook (claude), or on `surface wait` plus the brief's REPORT line (codex). The hook reads a turn file the adapter keeps outside every worktree, `~/.local/state/forgectl-paperclip/turns/<claude session id>.json`, and POSTs the run id and a one-time token to `http://127.0.0.1:<port>/stop`. The adapter accepts it only for the worker's own session. A dialog that appears meanwhile fails the run as `worker_blocked`.
4. `surface read --report` becomes the run summary; Claude usage is summed from the transcript path forgectl recorded, capped at 32 MiB per turn. The turn files are removed on every exit.

**Status reporting.** The adapter sets `supportsLocalAgentJwt`, so Paperclip mints a run-scoped JWT per turn. The adapter writes it into a 0700 report script in the state dir (`turns/<run id>.report.sh`), removed when the turn ends, and the brief names the script and the exact command. The worker runs `bash <script> in_review "<summary>"`: the script comments, then moves the issue to `in_review` assigned to `FORGECTL_PAPERCLIP_REVIEWER_USER_ID` (Paperclip refuses an agent's `in_review` without a review path, and an issue has one assignee). The bearer header reaches curl through a file descriptor, so no process argv carries the token and the script never prints it. Paperclip cancels the agent's run when its issue is reassigned, so a successful handoff shows as a cancelled run with `issue_reassigned`.

**What the token can do.** The worker runs as the operator and can read the script, so treat the token as the worker's. Paperclip's run JWT has standard scope: it acts as this agent on any visible issue in the company (comment, change status, reassign), reads company-wide, can edit its own `adapterConfig` (the operator-only keys above are refused), and can hire agents when the agent may. The script's `in_review`-only rule is a convenience, not a control. Writes are revoked when the run ends `cancelled` (the reviewer handoff does this); otherwise the token stays valid until it expires, 48 h by default. Set `PAPERCLIP_AGENT_JWT_TTL_SECONDS` on the Paperclip server a little above the longest `turnTimeoutSec` (instance-wide; for example `3600`).

The worker runs as the operator and can read the turn file during its turn, so the token proves the callback came from a local process during this turn, not that it came from the hook. That is accepted for a loopback, single-operator setup. `XDG_STATE_HOME` is not used, because forgectl does not pass it to workers and the hook could not follow it.

Build and test:

```bash
pnpm install
pnpm --filter @forgectl/paperclip-adapter-herdr build   # one bundled dist/index.js
pnpm --filter @forgectl/paperclip-adapter-herdr test
```

`scripts/install-service.sh` registers it (see [Running as a service](#running-as-a-service)). By hand, add an entry to `$PAPERCLIP_HOME/adapter-plugins.json` whose `localPath` is a directory holding `package.json` and the built `dist/index.js`, with `type` `herdr_forgectl`.

Agent `adapterConfig` holds only per-agent choices: `repo` (required; must be one of `FORGECTL_PAPERCLIP_REPOS`), `harness` (`claude` or `codex`), `profile` (a `[surface.profiles]` name, claude only), `turnTimeoutSec` (30-86400, default `1800`). pi agents use Paperclip's built-in `pi_local` adapter, because forgectl refuses pi as a worker.

An agent can edit its own `adapterConfig` through Paperclip's API, so anything that decides what the server executes or where panes open is an operator setting, and the adapter refuses it in `adapterConfig`. Operator settings come from the Paperclip server's environment or from a JSON file of the same keys (string values only), `FORGECTL_PAPERCLIP_CONFIG` or `~/.config/forgectl/paperclip.json`. The file must be a regular file owned by the server's user with no group or other access; any other key, or a non-string value, fails the run. The environment wins over the file.

| Variable | Meaning | Default |
|---|---|---|
| `FORGECTL_PAPERCLIP_REPOS` | `:`-separated absolute repo roots agents may use | required; unset means no repo is allowed |
| `FORGECTL_PAPERCLIP_FORGECTL` | absolute path of the forgectl binary | `forgectl` on `PATH` |
| `FORGECTL_PAPERCLIP_HERDR_SESSION` | herdr session for every pane | `paperclip` |
| `FORGECTL_PAPERCLIP_CALLBACK_PORT` | loopback port for the Stop-hook listener | `3199` |
| `FORGECTL_PAPERCLIP_API_URL` | loopback Paperclip API the report script calls | `http://127.0.0.1:3100` |
| `FORGECTL_PAPERCLIP_REVIEWER_USER_ID` | Paperclip user an agent hands its issue to at `in_review` | unset: the agent comments only |
| `FORGECTL_PAPERCLIP_PATH` | `:`-separated absolute directories used as `PATH` for forgectl (it finds `herdr` on `PATH`; a launchd service has no shell `PATH`) | the server's own `PATH` |
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

plus `.claude/hooks/paperclip-stop.sh` (copy `packages/adapter-herdr/hooks/paperclip-stop.sh`; it needs `jq` and `curl`). The hook is a file in the worktree, so a worker can edit it during a turn; that grants nothing while workers run unsandboxed as the operator, and moving it out of the worktree is tracked in forgectl#1244. Each harness account must trust the repo root once.

## Curated worker profile

`scripts/make-worker-profile.sh` builds `~/.claude-worker`: plugins, rules, skills and project memory shared with `~/.claude` by symlink, and its own `settings.json` copied from `~/.claude/settings.json` with the session mods (`sploot`, `c3`, `atelier`, `board`, `verbs`, `guard-toast`, `herdr-bridge`) off, `promptSuggestionEnabled` false and the `auto-mode-setup` skill off. It registers `[surface.profiles.worker]` in forgectl's config. `--refresh` rebuilds the settings after `~/.claude/settings.json` changes; `--dry-run` shows the plan. Then log in once with `CLAUDE_CONFIG_DIR=~/.claude-worker claude` (`/login`), trust each worker repo root, and set an agent's `adapterConfig.profile` to `worker`.

## Running as a service

`scripts/install-service.sh --repos <abs repo>[:<abs repo>...] [--reviewer <user id>] [--dry-run]` runs Paperclip as a launchd (macOS) or systemd service, instance `forgectl` on `127.0.0.1:3110` (embedded Postgres on 54339), next to any other instance without touching it. Each step skips what is already in place:

1. Installs the pinned CLI (`paperclipai install --version 2026.1005.0`; shim at `~/.local/bin/paperclipai`).
2. Onboards the instance once (loopback, `local_trusted`), stops that first server when healthy, pins the ports and adds `PAPERCLIP_AGENT_JWT_TTL_SECONDS=7200` to the instance `.env`.
3. Builds the adapter and copies the bundle to `~/.local/share/forgectl-paperclip/adapter-herdr`, so no checkout has to stay in place.
4. Registers `herdr_forgectl` in `~/.paperclip/adapter-plugins.json`.
5. Writes the operator settings file (0600) once, with absolute `forgectl`, the herdr session, the API URL and a `FORGECTL_PAPERCLIP_PATH` that finds `herdr`; edit it by hand afterwards.
6. `paperclipai service install --instance forgectl`.

The herdr session must be running: `herdr --session paperclip server`. Nothing restarts it after a reboot yet. Remove the service with `paperclipai service uninstall --instance forgectl`.

## Worktrees

forgectl owns every worker's worktree (`surface launch --worktree paperclip/<name>`). Do not attach a Paperclip project whose workspace strategy creates git worktrees to `herdr_forgectl` agents: Paperclip would create a second worktree the pane never uses.

## pi agents and `in_review`

`pi_local` agents follow their `AGENTS.md` instructions bundle, whose default checklist ends issues at `done`. `scripts/apply-in-review-rule.sh <agent id> <reviewer user id>` appends `instructions/in-review.md` to that agent's `AGENTS.md` (once; it checks for a marker): never set `done`, hand the issue to the reviewer at `in_review`, and start self-created issues in `backlog`. It needs `PAPERCLIP_API_URL`, and `PAPERCLIP_BOARD_TOKEN` in authenticated mode.
