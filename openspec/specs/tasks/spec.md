# tasks Specification

## Purpose
`forgectl tasks` is a credentialed Vikunja client: three read verbs over a local cache, one verb that closes a task with recorded evidence, and an MCP server over the same client, built so a token never leaks and never reaches an unapproved host.

## Requirements

### Requirement: Read verbs
`forgectl tasks ls` SHALL list open tasks, `tasks show <id>` one task with its detail and relations, and `tasks ready` the open tasks with no active "blocked" relation ranked by Vikunja's position field. These verbs MUST issue GET requests only. With `--json`, `ls` and `ready` MUST print a bare array of `{id, title, done, position}`.

#### Scenario: List as JSON
- **WHEN** `forgectl tasks ls --json` reads the board
- **THEN** stdout is a JSON array of open tasks, each with `id`, `title`, `done` and `position`
- **AND** the exit code is 0

### Requirement: Token sources and secrecy
The bearer token SHALL be read fresh on every run from the macOS login keychain (`--keychain-service`, default `vikunja-readonly`), except `mcp --http`, which reads `--token-file`. There MUST be no environment-variable token source. The token MUST never appear on argv, in a log line, an error string, the local cache, or a close record.

#### Scenario: Token in the environment
- **WHEN** a Vikunja token is set only in an environment variable
- **THEN** no `tasks` verb uses it

### Requirement: Host allowlist for keychain credentials
A keychain credential SHALL be sent only to the built-in default host or a host listed under `[tasks] allowed_hosts` (plain hostnames only). Any other `--host` on `ls`, `show`, `ready`, `done` or `mcp` over stdio MUST exit 4 before the keychain is read, and MUST append one `{"event":"host_refused", ...}` line to `tasks-closes.jsonl` naming the verb and keychain entry, never a token. A refusal MUST never be served from cache.

#### Scenario: Unlisted host
- **WHEN** `forgectl tasks ls --host other.example` runs and `other.example` is not in `allowed_hosts`
- **THEN** it exits 4 without reading the keychain
- **AND** `tasks-closes.jsonl` gains one `host_refused` line

#### Scenario: Invalid allowlist entry
- **WHEN** `[tasks] allowed_hosts` holds an entry with a port, path or IP address
- **THEN** the config file is invalid and every command refuses it as a config that does not parse

### Requirement: Distinct exit codes
`tasks` verbs SHALL exit 0 on success, 2 when the instance could not be reached, 3 when the server rejected the credential, 4 on a host or drain-worker refusal, 7 when a read verb refuses a stale cache, and 1 for anything else, including usage errors and an unknown verb.

#### Scenario: Revoked credential
- **WHEN** the server answers a read with 401
- **THEN** the verb exits 3 and does not fall back to the cache

#### Scenario: Unknown verb
- **WHEN** `forgectl tasks nosuchverb` runs
- **THEN** it exits 1

### Requirement: Cache fallback with a stale ceiling
On a network failure, the read verbs MAY serve the local cache and MUST print a `STALE DATA` notice with the cache's age to stderr. A cache older than 72 hours MUST be refused: the verb writes nothing to stdout, says `refusing data older than 72h; pass --allow-stale to use it`, and exits 7. `--allow-stale` serves it with the notice. `tasks show --json` MUST add a `stale` object only on a cache fallback.

#### Scenario: Old cache while offline
- **WHEN** the instance is unreachable and the cache is 80 hours old
- **THEN** `tasks ready` exits 7 with nothing on stdout

### Requirement: Close a task with evidence
`forgectl tasks done <id> --evidence TEXT` SHALL read the task, send one update marking it done with a closed-by line (closer, surface, time, evidence) appended to its description, and read it again. `--evidence` MUST be one line of at most 300 characters with nothing token-shaped. An already done task MUST be reported and nothing written. A repeating task MUST be refused. It MUST never read or write the cache.

#### Scenario: Already done
- **WHEN** `tasks done 42 --evidence "merged owner/repo#12"` targets a task that is already done
- **THEN** it exits 0 with `already_done` true and `evidence_recorded` false
- **AND** sends no update

### Requirement: Separate write credential
`tasks done` SHALL read only the write keychain entry (`--write-keychain-service`, default `vikunja-write`) and MUST never fall back to the read entry. It MUST refuse `--keychain-service` as a usage error. A missing write entry MUST fail with code `credential_missing`.

#### Scenario: Read flag on done
- **WHEN** `tasks done 42 --evidence "x" --keychain-service vikunja-readonly` runs
- **THEN** it fails with `usage_error` before any credential is read

### Requirement: done JSON contract
With `--json`, `tasks done` SHALL print `{"id","project_id","title","done","already_done","evidence_recorded"}` on success, and on failure one `{"error","code","path"}` object on stderr with `path` `""` and `code` one of `usage_error`, `credential_missing`, `not_found`, `repeating_task`, `trailer_too_long`, `write_refused`, `not_confirmed`, `unauthorized` (exit 3) or `failed`.

#### Scenario: Unknown task
- **WHEN** `tasks done 999999 --evidence "x" --json` names no visible task
- **THEN** stderr's last line is a JSON object with `code` `not_found` and the exit code is 1

### Requirement: Close record
Every `tasks done` call that sends an update SHALL write one JSON line to stderr and append it to `tasks-closes.jsonl` in the config directory, whatever `log_level` is. The line MUST hold time, task and project id, `surface`, `closer`, evidence, the keychain entry's name, host and `outcome`, never the token. The file MUST be created `0600`, and an append to a symlink or a group- or world-accessible file MUST be refused without changing the exit code.

#### Scenario: Close succeeds
- **WHEN** `tasks done` closes a task
- **THEN** stderr carries one line starting `{"time":` with `outcome` `closed`
- **AND** the same line is appended to `tasks-closes.jsonl`

### Requirement: Drain workers never write the board
`tasks done` and `tasks mcp` SHALL refuse with exit 4 when `FORGECTL_DRAIN_WORKER` is set, before anything else runs.

#### Scenario: Close from a drain worker
- **WHEN** `tasks done 42 --evidence "x"` runs with `FORGECTL_DRAIN_WORKER` set
- **THEN** it exits 4 and sends nothing

### Requirement: MCP server tools
`forgectl tasks mcp` SHALL serve seven tools over stdio, or streamable HTTP with `--http`: `list_projects`, `list_tasks`, `get_task`, `ready_tasks`, `create_task`, `add_comment` and `complete_task`. Every title, description and comment it returns MUST be wrapped in a per-response `<board-text-NONCE>` fence with any occurrence of the delimiter inside escaped.

#### Scenario: Board text returned
- **WHEN** an MCP client calls `get_task`
- **THEN** the task's title and description are inside a `board-text-` fence unique to that response

### Requirement: MCP per-session write limits
Within one MCP session, `complete_task` SHALL send at most 10 updates, and `create_task` plus `add_comment` together at most 20 writes. A call past either limit MUST send nothing, return a tool error, and write a line with outcome `cap`. Each sent write MUST write a record line to stderr, and on stdio also to `tasks-closes.jsonl`, never carrying the token, title, description or comment text.

#### Scenario: Write limit reached
- **WHEN** an MCP session calls `create_task` a 21st time
- **THEN** the call returns an error starting `create_task: write_cap:`
- **AND** nothing is sent to the board

### Requirement: MCP over HTTP
`tasks mcp --http ADDR` SHALL require `--token-file`, refusing a file readable or writable by group or other, and SHALL require at least one `--pin-ip`, which is rejected without `--http`. An address MUST be dialed only when it is both in the pin list and admitted by the host-pinning policy; a public address is refused even when listed.

#### Scenario: HTTP without a pin
- **WHEN** `tasks mcp --http :3000 --token-file /run/secrets/token` runs with no `--pin-ip`
- **THEN** the server refuses to start

### Requirement: Healthcheck ping
`tasks mcp --http ADDR --ping` SHALL send an initialize request, have the server read the board once, end the session, and print only `ok` on success. It MUST exit 0 when both work, 2 when the listener did not answer, 3 when the board refused the server's credential, and 1 otherwise. It MUST never print board text.

#### Scenario: Credential revoked after startup
- **WHEN** the server is up and its board read is answered with 401
- **THEN** `--ping` exits 3
