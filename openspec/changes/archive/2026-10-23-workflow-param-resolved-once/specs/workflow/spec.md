## ADDED Requirements

### Requirement: Param values are substituted once
A param's value SHALL be substituted into a step exactly once and MUST NOT be read again as a `${...}` reference, so the step receives what `--dry-run` printed. Only references written in the workflow file resolve to params or exports.

#### Scenario: Param value that looks like a reference
- **WHEN** a workflow runs with `--param note='${workspace}'` and a `run` step's args reference `${note}`
- **THEN** the step receives the literal text `${workspace}`, not the sandbox path
