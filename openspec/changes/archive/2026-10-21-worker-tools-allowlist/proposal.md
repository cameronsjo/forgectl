## Why

The built-in tool listing is the largest block in a claude worker's fixed context. Measured on Claude Code 2.1.305, ten core tools in place of the cloud set of 38 cut about 18k first-request tokens (cameronsjo/cadence-ecosystem#665). `[launch.worker] settings` (cameronsjo/forgectl#1323) cannot reach it, because the tool set is a flag, not a setting.

## What Changes

- New config key `[launch.worker] tools = [...]`: a claude worker gets `--tools` with the listed names, joined by commas.
- Each name must be a capitalized identifier. Claude Code drops unknown names without an error, and lowercase names left a session with no tools, so anything else refuses the launch. An empty list and `"default"` refuse the launch too.
- `forgectl launch which` and `forgectl config` show the list on a `tools:` line under the worker row, and `--json` carries it as `worker.tools`. A list the launch would refuse shows in `worker.refused`.
- No list set: no change. Codex and pi workers ignore it.

## Capabilities

### Modified Capabilities

- `launch`: claude workers take an optional built-in tool allowlist.

## Impact

- Config key: `[launch.worker] tools`.
- `--json` field: `worker.tools` in `launch which` and `config`.
- Docs: `docs/commands/launch.md` (§ Worker posture), ADR-0010 2026-10-20 amendment.
