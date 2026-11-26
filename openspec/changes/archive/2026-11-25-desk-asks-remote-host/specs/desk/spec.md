## ADDED Requirements

### Requirement: Asks on a remote host
When `FORGECTL_DESK_ASKS_HOST` is set and none of `--dir`, `DESK_DIR` and `CLAUDE_DESK_DIR` is given, `desk ask`, `desk asks`, `desk respond` and `desk watch --responses` SHALL run on that host over ssh. Every other desk verb SHALL stay local. `desk ask --notify` MUST be refused while asks are remote.

#### Scenario: Notify while remote
- **WHEN** `desk ask --notify self` runs with a remote host set
- **THEN** nothing is sent and the exit code is 2

#### Scenario: A local desk named
- **WHEN** `DESK_DIR` is set as well as the host
- **THEN** `desk ask` records the ask in the local desk

### Requirement: Validated remote host and binary
The host MUST match `^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`. The remote binary (`FORGECTL_DESK_ASKS_REMOTE_BIN`, default `forgectl`) MUST be an absolute path or a bare name. Any other value SHALL be refused with exit 2 and an error naming the expected shape.

#### Scenario: A host that is an option
- **WHEN** `FORGECTL_DESK_ASKS_HOST` is `-oProxyCommand=sh`
- **THEN** ssh is not run and the exit code is 2

### Requirement: Quoted remote command
ssh SHALL be run without a local shell, and every word of the remote command MUST be single-quoted for a POSIX shell.

#### Scenario: A hostile reply
- **WHEN** `desk respond ASK-ID reply -- '$(touch x); y'` runs with a remote host set
- **THEN** the host records the reply text byte for byte and the remote shell runs nothing from it

### Requirement: Remote outcome passes through
The remote output SHALL stream unchanged and its exit code SHALL pass through. An ssh failure SHALL exit 255 with an error naming the host and saying nothing was recorded.

#### Scenario: No connection
- **WHEN** ssh exits 255
- **THEN** `desk ask` exits 255 with an error naming the host and saying the ask was not recorded
