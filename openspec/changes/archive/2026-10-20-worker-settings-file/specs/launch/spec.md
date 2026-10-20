## ADDED Requirements

### Requirement: Worker settings file merges into a claude worker's settings
When `[launch.worker] settings` names a file (absolute, or `~/`-relative), a claude worker launch MUST read it as JSON and merge it into the worker's `--settings` document. A key forgectl already sets SHALL keep forgectl's value, and deny rules SHALL be the union of both lists. Codex and pi workers MUST NOT read the file.

#### Scenario: Plugin turned off for workers
- **WHEN** the file is `{"enabledPlugins": {"artificer-voice@workbench": false}}`
- **THEN** a claude worker's `--settings` carries that entry beside forgectl's own keys

#### Scenario: No file
- **WHEN** `[launch.worker] settings` is unset
- **THEN** the worker's `--settings` is unchanged

### Requirement: Worker settings file can only narrow
The file MUST only carry `enabledPlugins` with `false` values, `skillOverrides` with `name-only`, `user-invocable-only` or `off`, `outputStyle`, and `permissions.deny` rules shaped `Tool`, `Tool(specifier)` or `mcp__…`. Any other key or value, a missing or unreadable file, or a file over 64 KiB MUST refuse the launch.

#### Scenario: Widening file refused
- **WHEN** the file carries `permissions.allow` or `enabledPlugins` with `true`
- **THEN** the worker launch is refused

### Requirement: launch which shows the worker settings file
`forgectl launch which` and `forgectl config` SHALL show the configured file under the worker row, and `--json` SHALL carry it as `worker.settings`. A file the launch would refuse MUST be reported in `worker.refused`.

#### Scenario: Refused file reported
- **WHEN** the file carries `hooks`
- **THEN** `forgectl launch which --json` reports `worker.refused` naming `[launch.worker] settings`
