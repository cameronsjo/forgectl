## ADDED Requirements

### Requirement: Unknown command under --json
When `--json` is passed and the verb or subverb named does not exist, stderr MUST carry exactly one `{"error","code","path"}` object with `code` `unknown_command` and `path` `""`, and no styled error frame. The exit status MUST equal the one the same call gets without `--json`.

#### Scenario: Unknown top-level verb
- **WHEN** `forgectl zzbogus --json` is run
- **THEN** stderr is one object with `code` `unknown_command` naming `zzbogus`, and the exit status is 2

#### Scenario: Unknown subverb under tasks
- **WHEN** `forgectl tasks zzbogus --json` is run
- **THEN** stderr is one object with `code` `unknown_command`, and the exit status is 1, as without `--json`

## MODIFIED Requirements

### Requirement: JSON failure object on stderr
When a `--json` verb fails before emitting its verdict, stdout MUST stay empty and stderr MUST carry exactly one JSON object on one line, `{"error","code","path"}`, with all three keys present. `code` SHALL be `usage_error` when nothing was attempted (`unknown_command` when the verb named does not exist) and `failed` otherwise; `path` is `""` unless one resolved file is to blame.

#### Scenario: Bad flag with --json
- **WHEN** `forgectl status --json --bogus` is run
- **THEN** stdout is empty and the last line of stderr is one object with `code` `usage_error`
- **AND** the exit status is 2

#### Scenario: Bad flag before --json
- **WHEN** `forgectl status --bogus --json` is run
- **THEN** the failure is still reported as the JSON object, even though parsing stopped before `--json`
