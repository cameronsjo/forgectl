## ADDED Requirements

### Requirement: Help and error frames respect the probe policy
When stdout and stdin are terminals and the probe policy refuses a background query (an explicit `[theme] mode`, `NO_COLOR` set, `TERM` starting with `screen` or `tmux`, or `TERM=dumb`), forgectl MUST render `--help`, `--version` and error frames without sending the OSC 11 or DA1 query. The frames SHALL be the ones fang renders, minus the query bytes.

#### Scenario: Help inside tmux
- **WHEN** `forgectl --help` runs on a pty with `TERM=tmux-256color` that never answers
- **THEN** no `ESC ] 11 ; ?` is written and the command returns without waiting for a reply

#### Scenario: Refusal under TERM=dumb
- **WHEN** `forgectl desk` runs on a pty with `TERM=dumb`
- **THEN** the refusal prints with no background query before it
