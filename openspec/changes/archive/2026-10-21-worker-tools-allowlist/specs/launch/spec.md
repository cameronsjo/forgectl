## ADDED Requirements

### Requirement: Worker tools list limits a claude worker's built-in tools
When `[launch.worker] tools` is set, a claude worker launch MUST pass `--tools` with the listed names joined by commas, duplicates dropped. When it is unset, the launch MUST NOT pass `--tools`. Codex and pi workers MUST ignore the list.

#### Scenario: Core tools only
- **WHEN** `tools = ["Bash", "Read"]`
- **THEN** a claude worker's argv carries `--tools Bash,Read`

#### Scenario: No list
- **WHEN** `[launch.worker] tools` is unset
- **THEN** a claude worker's argv carries no `--tools`

### Requirement: Worker tools list refuses names Claude Code would drop
Each entry MUST be a capitalized identifier (letters and digits, starting with an uppercase letter). An entry of any other shape, an empty list, or the value `default` MUST refuse the launch.

#### Scenario: Lowercase name refused
- **WHEN** `tools = ["read"]`
- **THEN** the worker launch is refused

#### Scenario: Empty list refused
- **WHEN** `tools = []`
- **THEN** the worker launch is refused

### Requirement: launch which shows the worker tools list
`forgectl launch which` and `forgectl config` SHALL show the configured list under the worker row, and `--json` SHALL carry it as `worker.tools`. A list the launch would refuse MUST be reported in `worker.refused`.

#### Scenario: Refused list reported
- **WHEN** `tools = ["read"]`
- **THEN** `forgectl launch which --json` reports `worker.refused` naming `[launch.worker] tools`
