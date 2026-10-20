## Why

A claude worker (`surface launch --worktree`) loads everything an interactive session loads: every plugin's skills and agents, the operator's output style, every mod. A worker with one bounded task needs a fraction of that. Measured on Claude Code 2.1.305, a per-launch `--settings` that turns plugins off and denies unused agents cut a session's first-request context by about 6k tokens, and with a tool list by half (cameronsjo/cadence-ecosystem#665). This change implements cameronsjo/forgectl#1323.

## What Changes

- New config key `[launch.worker] settings = "<path>"`: a JSON file merged into the `--settings` document every claude worker already gets.
- The file may only remove things: `enabledPlugins` (false values only), `skillOverrides` (`name-only`, `user-invocable-only`, `off`), `outputStyle`, and `permissions.deny` (rules in the shape the Claude Code settings schema accepts). Any other key or value refuses the launch.
- A key forgectl sets keeps forgectl's value; deny rules are a union.
- `forgectl launch which` and `forgectl config` show the file on a `settings:` line under the worker row, and `--json` carries it as `worker.settings`. A file the launch would refuse shows in `worker.refused`.
- No file set: no change. Codex and pi workers never read it.

## Capabilities

### Modified Capabilities

- `launch`: workers take an optional narrowing settings file.

## Impact

- Config key: `[launch.worker] settings`.
- `--json` field: `worker.settings` in `launch which` and `config`.
- Docs: `docs/commands/launch.md` (§ Worker posture), ADR-0010 amendment.
