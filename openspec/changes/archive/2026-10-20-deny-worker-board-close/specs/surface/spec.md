## ADDED Requirements

### Requirement: Worker settings deny board closes
A claude worker's `--settings` MUST deny `Bash(forgectl tasks done:*)` in every permission mode, so a worker cannot close a board task even where the operator's own allow rules would let it.

#### Scenario: Worker cannot close a board task
- **WHEN** a claude worker launches in any permission mode
- **THEN** its `--settings` deny list holds `Bash(forgectl tasks done:*)`
