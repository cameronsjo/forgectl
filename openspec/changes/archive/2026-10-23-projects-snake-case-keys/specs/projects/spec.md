## MODIFIED Requirements

### Requirement: JSON output keeps stdout clean
`projects list --json` SHALL write rows (`host`, `owner`, `name`, `sshUrl`, `mirror`, `private`, `cloned`, `localPath`, `status`, `ssh_url`, `local_path`) to stdout as a bare array, `[]` when empty, and MUST send degradation notes to stderr only. `ssh_url` and `local_path` SHALL carry the same values as `sshUrl` and `localPath`, and SHALL be left out whenever those keys are. `--fields` SHALL accept either spelling.

#### Scenario: A degraded host under --json
- **WHEN** one host is unreachable and `--json` is set
- **THEN** stdout holds only the JSON rows that loaded and the note goes to stderr

#### Scenario: Snake-case keys beside camelCase
- **WHEN** the operator runs `forgectl projects list --json` and a row has a clone URL
- **THEN** the row carries both `sshUrl` and `ssh_url` with the same value
