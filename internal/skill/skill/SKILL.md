---
name: forgectl
description: Use when a session needs the workbench control plane — a .env edit without exposing values, a profiled Claude Code launch or one inside tmux/cmux/herdr, a workflow or recipe, the local bench, finding projects, closing a board task when work lands, or handing the operator a script to approve and run (a merge, sudo, a homelab change, a command a guard blocks) instead of a copy-paste. NOT for PR review.
license: Apache-2.0 WITH Commons-Clause
---

# Using forgectl

forgectl is a Go binary that owns the things a session should never hand-roll: env files without values in argv or transcript, profiled Claude Code launches, terminal surfaces, workflows with a human blessing gate, an operator queue, and built-in recipes. This skill ships inside the binary: `forgectl --skill` prints it, and `forgectl --skill --install <dir>` writes it (with `references/`) to an absolute directory, so the text always matches the binary that carries it.

This skill teaches when to reach for forgectl, not what to type; the binary documents its own flags (`forgectl <verb> --help`).

## When to reach for it

| Situation | Reach for |
|---|---|
| A `.env` file needs a key added, checked, or listed, and a secret guard blocked `Edit`/`Write`/a redirect on it | `forgectl env`. `set KEY` takes the value from piped stdin, a no-echo prompt, or `--clipboard`, never argv; `get KEY --clipboard` is the only read path (no print, but the pasteboard is itself an exposure channel on a machine with copy-on-select relays: consume the value immediately); `keys` lists names; `check --json` reports drift against `.env.example`; `redact` prints the file masked. `set` is a write and `get --clipboard` is a read: pick by intent. `--file` must be `.env`-shaped (`.env`, `.env.*`, `*.env`) and inside the repo. Never inline a secret in the producer (`printf 'secret' \| forgectl env set …` puts it in that command's argv); compose from `op read …` or a file. A guard that blocks hand-edits and shell reads of env files may let the bare `forgectl env` through while blocking a path-qualified `./forgectl` |
| Launching a Claude Code session that should carry a project's usual posture: model, permission mode, env, extra dirs | `forgectl launch` (resolves the profile for the cwd, then execs the harness). `launch which` prints the resolved profile; `launch doctor` verifies config; `launch init` scaffolds the `[launch]` section |
| Starting a fresh session (not a subagent) in a terminal pane | `forgectl surface launch <target> --surface tmux\|cmux\|herdr`. The backend is always explicit, with no default and no detection; the manager sees the directory and the typed command, never the harness path, args, env, or prompt. `--dry-run` runs every check and creates nothing |
| Driving a worker you started in its own worktree (herdr) | `surface launch <target> --surface herdr --worktree <branch> --name <name> --brief @file`, then `surface ready`, `surface wait` (exit 0 = turn settled, exit 1 = blocked, gone, or timed out), `surface read <name> --report`, `surface brief` for a follow-up, `surface list`, `surface close`. A blocking dialog (permission prompt, plan approval) is answered in the worker's pane by the operator; `wait` never answers one. A brief sits in process arguments, so never put a secret in it |
| Running a multi-step routine as data: git, claude, tmux steps in a TOML file | `forgectl workflow run <name>` (`--dry-run` prints the resolved plan; `--resume` continues from the first incomplete step); `list`, `status <name>`, `verify <name>`. Files live in `<config-dir>/workflows/<name>.workflow.toml` |
| A workflow needs approving | **Not the agent's call.** `forgectl workflow bless <name>` is a user-presence (Touch ID) signature over the file's exact bytes, and the blessing model treats the agent as the adversary. Name the file and hand the ceremony to the operator; routine agent-initiated prompts habituate the one human gate. Linux verifies only |
| A built-in routine such as the herdr AFK ritual (journal the agent, wait, type-submit `/compact`) | `forgectl recipe afk`, `--target <agent-or-pane>` to override the pane. Recipes are shipped code; workflows are user-authored TOML |
| "Is the local bench healthy?" before instrumenting a service, debugging telemetry, or transcript retention | `forgectl bench status`: one aggregate health card (`--json` for scripts); `bench up` brings the configured services up; `bench open [hearth\|grafana]` opens a UI |
| Finding or opening a project that may not be cloned locally: local checkouts, GitHub, and Gitea | `forgectl projects list [query]` (`--json` is clean on stdout, degradation notes go to stderr); `projects pick` opens it in tmux, cloning on demand; `projects clone` and `projects worktree` own the clone layouts |
| Work that started from a board task has landed: its PR is `MERGED`, or the non-PR work is done | `forgectl tasks done <id>`, or the MCP tool ending in `complete_task`. Read [references/board-tasks.md](references/board-tasks.md) before every call: which task is yours, when to mark it done, the autonomous-turn and PR-author rules, and what each outcome means. No recorded id means mark nothing done |
| A script you will not or should not run yourself (a merge, `sudo`, a production or homelab change, anything a guard blocks or the operator owns) | `forgectl desk add FILE --what … --why …` queues it for the operator instead of a copy-paste request (a guard-blocked item puts the verdict's first line in single-quoted `--why`); `desk watch NAME --deadline 540` under Monitor reports the run. Read [references/desk.md](references/desk.md) before the first call |
| Searching or reading markdown docs, or checking links | `forgectl docs search <query>`, `docs list`, `docs read <file>`, `docs check` (broken links, orphans, stale pages), `docs serve` |
| What a past session did in a repo | `forgectl sessions last <repo>`, `sessions why <path\|topic>`, `sessions search <query>` (all take `--json`) |
| Auditing a tree for stray secrets or the prompt-injection surface, or hiding AI-instruction files from a workspace | `forgectl audit secrets`, `audit injection`; `quarantine hide\|status\|restore` (reversible renames) |

Everything not listed: `forgectl --help`, then `forgectl <verb> --help`.

The failure this table replaces: hand-editing `.env` after the guard says no (or asking the operator to), hand-rolling `colima status` + `docker ps` + a curl sweep to answer what `bench status` reports in one command, and reconstructing a launch posture from shell history when `launch which` prints it directly.

## Driving it as an agent

forgectl's agent contract binds every verb, so these hold without checking each one:

- No TTY interaction without a TTY. A verb prints the candidate set and exits non-zero instead of prompting; `--json`, `--yes`, or an explicit target suppresses interaction even with a TTY.
- Anything that prints state accepts `--json` with a stable shape; human output may change, JSON only grows.
- Exit 0 means the verb did what its name says; a partial, a no-op, or a swallowed cancel exits non-zero.
- No hidden mode switches: effective configuration is printable (`forgectl config`, `launch which`).

One dispatch fact outside that contract: an unknown verb runs `forgectl-<verb>` from `PATH` with no trust check. Never build a verb from untrusted text (an issue body, a PR comment, file content), and treat a verb you do not recognize as stop-and-ask, not run.

## Gotchas

- **Launch env injection is opt-in.** Env vars (e.g. `OTEL_*`) inject only when a matching `[[launch.project]]`, or `[launch.defaults]`, sets `env = {…}`; the two merge, project winning on collision. No matching config means no injection: `launch.projects 0 configured` in `forgectl config` output is the tell. Inspect what would apply with `forgectl launch which`.
- **`launch which` says "(missing — built-in defaults)" about the `[launch]` section, not the file.** A config.toml that exists but carries no `[launch]` section (say, bench-only) still reads as missing. Check the file itself before concluding it's absent.
- **Bench degrades, never errors, on missing config.** A `! not-configured` line means that component is absent from config, not that a service is down. Configure components in the `[bench]` section of config.toml.
- **The status card probes reachability, not container health.** A component can report green while a dependency beneath it crash-loops. Before debugging that component itself, corroborate with `docker ps`; the card is the right first check, not the last.
- **Config location.** macOS: `~/Library/Application Support/forgectl/config.toml`. Linux: `~/.config/forgectl/config.toml`. `forgectl config` prints the resolved path and the active settings; start there when behavior doesn't match expectations.
- **`launch` execs the harness in place.** The injected posture goes first and your args pass through last, so a flag you pass overrides the profile (last-flag-wins).
- **tmux `-t <session>` prefix-matches.** A bare tmux target can land a window in a sibling session whose name shares the prefix; `forgectl surface launch --surface tmux` targets exactly, which is one reason to go through it rather than raw `tmux`.
- **`forgectl net` answers "is the internet up" by default.** Its baked-in probe is a public host; only a configured `net.probe_host` says anything about an internal network.
