## ADDED Requirements

### Requirement: Manifest timeout bound
A manifest step's `timeout=` MUST be at most 9223372036 seconds, the largest whose deadline fits a duration. A larger value SHALL be refused as a manifest error, never run with a deadline that has already passed.

#### Scenario: Timeout too large
- **WHEN** a manifest step reads `a timeout=9223372037 -- sleep 1`
- **THEN** the manifest is refused with a bad timeout error naming the line and the step
