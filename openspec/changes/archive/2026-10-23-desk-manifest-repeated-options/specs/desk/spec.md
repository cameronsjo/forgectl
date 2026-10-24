## ADDED Requirements

### Requirement: Repeated manifest options
Repeated `after=` options on one manifest step SHALL add up, and `timeout=` MUST appear at most once per step.

#### Scenario: Repeated after= options
- **WHEN** a manifest step reads `c after=a after=b -- <command>`
- **THEN** `c` waits on both `a` and `b`
- **AND** a failure of either one skips `c`

#### Scenario: Repeated timeout
- **WHEN** a manifest step gives `timeout=` twice
- **THEN** the manifest is refused with an error naming the line and the step
