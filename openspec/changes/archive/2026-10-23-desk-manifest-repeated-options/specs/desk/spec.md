## MODIFIED Requirements

### Requirement: Batch manifests
A `.manifest` item SHALL hold one step per line as `<step> [after=a,b] [timeout=S] [private] -- <command>`, run in dependency waves at most 4 at a time. Repeated `after=` options on one step SHALL add up, and `timeout=` MUST appear at most once per step. A timed-out step MUST receive SIGTERM to its process group, SIGKILL 5 seconds later, and rc 124. A failed step MUST skip its dependents. A `private` step's output MUST be kept out of the combined log, and its `STEP_OUT` values of 6 or more characters replaced in later output with `<redacted:OUT_<step>_<key>>`.

#### Scenario: Step fails
- **WHEN** step `build` exits non-zero in a batch where `stage` has `after=build`
- **THEN** `stage` is skipped
- **AND** the batch ends with rc 1

#### Scenario: Repeated after= options
- **WHEN** a manifest step reads `c after=a after=b -- <command>`
- **THEN** `c` waits on both `a` and `b`
- **AND** a failure of either one skips `c`

#### Scenario: Repeated timeout
- **WHEN** a manifest step gives `timeout=` twice
- **THEN** the manifest is refused with an error naming the line and the step
