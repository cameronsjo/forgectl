## MODIFIED Requirements

### Requirement: Release row states and stall rules
Each tracked repo SHALL be `ok`, `paused` (toggle not `on`), `stalled`, or `unknown` (any failed read, named in the row). A repo SHALL be `stalled` when its toggle is on and the entrypoint has not run in 26h, its last 2 scheduled runs repeat a non-healthy reason, a run reports `half-shipped`, an endpoint trails a release older than 24h, its gate or lint copy hash is missing or differs, or the `no-release-pr` or `release-workflow-stuck` rule fires.

#### Scenario: Gate copy drift
- **WHEN** a repo's `.github/scripts/ship-gate.sh` hash differs from the canonical hash
- **THEN** that repo's row is `stalled`

#### Scenario: No canonical gate hash
- **WHEN** no `--gate-sha256`, `$FORGECTL_GATE_SHA256`, or canonical script beside the registry is available
- **THEN** every `release-pr` row that has a gate copy reads `unknown`, and a missing gate copy is still reported `missing`

#### Scenario: Lint copy drift
- **WHEN** a repo's `.github/scripts/lint-release-env.sh` hash differs from the canonical hash
- **THEN** that repo's row is `stalled` with `lint_copy` `drift`, and the stall names `lint-release-env.sh`

#### Scenario: Lint copy missing
- **WHEN** a `release-pr` repo has no `.github/scripts/lint-release-env.sh` on its branch
- **THEN** that repo's row is `stalled` with `lint_copy` `missing`, as a missing gate copy is

#### Scenario: No canonical lint hash
- **WHEN** no `--lint-sha256`, `$FORGECTL_LINT_SHA256`, or `../scripts/release/lint-release-env.sh` beside the registry is available
- **THEN** every `release-pr` row that has a lint copy reads `unknown`, and a missing lint copy is still reported `missing`
