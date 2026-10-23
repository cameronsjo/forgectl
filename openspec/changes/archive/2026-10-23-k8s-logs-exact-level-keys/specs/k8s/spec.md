## MODIFIED Requirements

### Requirement: Severity floor filters only recognized JSON
`--log-level` SHALL accept `trace`, `debug`, `info`, `warn`, `error`, `fatal` and refuse anything else. A line MUST be suppressed only when it parses as JSON with a top-level string `level` or `severity` ranked below the floor; plain text, malformed JSON, and unknown severities MUST always print. The key names MUST match exactly, in lower case.

#### Scenario: Floor at warn
- **WHEN** `--log-level warn` is set and kubectl emits `{"level":"info"}`, `{"level":"error"}`, and `starting server`
- **THEN** only the error line and the plain text line are printed

#### Scenario: Key in another case
- **WHEN** `--log-level warn` is set and kubectl emits `{"Level":"debug"}` and `{"level":"error","Level":"debug"}`
- **THEN** both lines are printed
