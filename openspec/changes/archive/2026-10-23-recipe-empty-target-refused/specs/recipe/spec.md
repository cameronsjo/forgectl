## ADDED Requirements

### Requirement: Explicit empty target refused
`recipe afk` MUST reject an explicitly empty `--target ""` with exit 2 before making any herdr call, rather than falling back to `HERDR_PANE_ID` or `HERDR_ACTIVE_PANE_ID`.

#### Scenario: Unset shell variable as target
- **WHEN** the user runs `recipe afk --target ""` with `HERDR_ACTIVE_PANE_ID` set
- **THEN** the command exits 2 and no herdr command runs
