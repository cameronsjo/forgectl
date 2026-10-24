## ADDED Requirements

### Requirement: Worker settings deny board closes
A claude worker's `--settings` MUST deny `Bash(forgectl tasks done:*)` in every permission mode, so a worker cannot close a board task through that shell command even where the operator's own allow rules would let it. Board MCP tools are not denied.

#### Scenario: Worker cannot run the board-close shell command
- **WHEN** a claude worker launches in any permission mode
- **THEN** its `--settings` deny list holds `Bash(forgectl tasks done:*)`
