## Why

forgectl tracked intent in dated `docs/plans/` files and decisions in `docs/adr/`, but nothing said what the shipped binary does today: a done plan is "history, not a spec", and the command docs drift. Moving to OpenSpec gives every capability a living spec that each change updates as it lands (cameronsjo/forgectl#1305).

## What Changes

- Add `openspec/` with the spec-driven schema and a project context (`openspec/config.yaml`).
- Record the shipped behavior of the main command groups as baseline capability specs, read from the code and checked against `docs/commands/`.
- Install the OpenSpec skills and `/opsx:*` commands for Claude Code (`.claude/`) and Codex (`.agents/`).
- Pin the OpenSpec CLI in `scripts/openspec/` (lockfile with integrity hashes) behind `scripts/openspec.sh`, and run `validate --all --strict` in CI.
- Freeze `docs/plans/`: `TestPlansIndex` rejects a plan dated after 2026-10-20. Plans already there finish in place.
- No change to the binary's behavior.

## Capabilities

### New Capabilities

- `agent-contract`: TTY, `--json`, exit codes and the failure object every verb follows.
- `configuration`: `config.toml`, `config` and `init`.
- `status`: the read-only overview.
- `doctor`: the ecosystem health check.
- `hub-menu`: the interactive hub.
- `theme`: the resolved colour theme.
- `pr-review`: clean-room pull-request review.
- `projects`: cross-host project inventory.
- `review`: cross-project work inventory and the release radar.
- `audit`: read-only posture scans.
- `launch`: the per-project harness launcher.
- `resume`: getting back into a Claude Code session.
- `proxy`: proxy profiles.
- `env`: `.env` management with values never exposed.
- `k8s`: bounded kubectl helpers.
- `desk`: the operator script queue.
- `sweep`: the chief-of-staff ledger.
- `attention`: the per-machine attention queue.
- `tasks`: the Vikunja board, close path and MCP server.
- `bench`: the local dev bench.
- `surface`: harnesses in terminal managers.
- `herdr-workspaces`: grouping herdr tabs into workspaces.
- `workflow`: the declarative workflow DSL.
- `recipe`: built-in workbench recipes.
- `docs-reader`: the local markdown reader.

### Modified Capabilities

None. There were no specs before this change.

## Impact

- New: `openspec/`, `.claude/skills/openspec-*`, `.claude/commands/opsx/`, `.agents/skills/openspec-*`, `scripts/openspec.sh`, `scripts/openspec/`.
- Changed: `.github/workflows/ci.yml` (new `openspec` job), `docs_plans_index_test.go`, `docs/plans/README.md`, `AGENTS.md`, `CONTRIBUTING.md`, `docs/README.md`.
- Groups without a spec yet (`tmux`, `branch`, `clean`, `docker`, `net`, `ghostty`, `pip`, `quarantine`, `preflight`, `update`, `upgrade`, `y`, `sessions`) get one with the first change that touches them.
