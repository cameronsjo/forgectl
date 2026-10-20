# recipe Specification

## Purpose
The `recipe` command group (alias `r`) runs small built-in workbench recipes: fixed sequences of other tools' calls. Today the only recipe is `afk`, which journals and compacts a Herdr agent pane.

## Requirements

### Requirement: Recipe command group
forgectl SHALL expose a `recipe` command group with the alias `r`, containing the `afk` subcommand. `recipe afk` MUST accept no positional arguments and MUST accept the flags `--target`, `--rename`, `--prompt`, `--compact-only`, and `--skip-receipt`.

#### Scenario: Alias reaches afk
- **WHEN** the user runs `forgectl r afk --target w1:p2`
- **THEN** it behaves exactly as `forgectl recipe afk --target w1:p2`

#### Scenario: Positional argument refused
- **WHEN** the user runs `forgectl recipe afk extra`
- **THEN** the command fails without contacting herdr

### Requirement: Target resolution
`recipe afk` SHALL resolve its Herdr target from `--target` when given, otherwise from `HERDR_PANE_ID`, otherwise from `HERDR_ACTIVE_PANE_ID`. When no source yields a non-empty value it MUST exit 2 before making any herdr call.

#### Scenario: Environment fallback
- **WHEN** `--target` is absent and `HERDR_PANE_ID` is set
- **THEN** that pane id is the target

#### Scenario: No target anywhere
- **WHEN** no `--target` is given and neither environment variable is set
- **THEN** the command exits 2 and no herdr command runs

### Requirement: Input allowlists
`recipe afk` MUST validate inputs by allowlist and exit 2 on violation before any herdr call. `--target` MUST NOT start with `-` and allows only letters, digits, and `._-:/@`; an error for a bad inherited target MUST name its source variable. `--rename` allows ASCII letters, digits, space, and `._-`, non-blank, at most 128 runes. `--prompt` MUST start with `/`, then only ASCII letters, digits, `:`, `-`, at most 128 runes.

#### Scenario: Control byte in rename
- **WHEN** `--rename` contains an ESC or `\x03` byte
- **THEN** the command exits 2 and nothing is sent to the pane

#### Scenario: Bad inherited target names its source
- **WHEN** `HERDR_PANE_ID` holds a value with a space and no `--target` is given
- **THEN** the command exits 2 and the error names `HERDR_PANE_ID`

### Requirement: Explicit empty prompt refused
`recipe afk` SHALL submit `/go:afk` as the first step when `--prompt` is omitted, and MUST reject an explicitly empty `--prompt ""` with exit 2 rather than substituting the default.

#### Scenario: Empty prompt
- **WHEN** the user runs `forgectl recipe afk --prompt ""`
- **THEN** the command exits 2 and no step runs

### Requirement: Receipt markers cannot be supplied by inputs
`recipe afk` MUST refuse with exit 2 any `--rename` or `--prompt` value that contains one of the compaction receipt markers `Compacting`, `Compacted`, or `Compact summary`, so that the input itself cannot satisfy the receipt check.

#### Scenario: Marker in rename
- **WHEN** `--rename "Compacted-run"` is given
- **THEN** the command exits 2

### Requirement: Preflight before side effects
`recipe afk` SHALL run `herdr agent get <target>` before any step that changes the pane. If the preflight fails, it MUST exit non-zero with an error naming the preflight and MUST NOT submit any prompt.

#### Scenario: Herdr unreachable
- **WHEN** `herdr agent get` fails
- **THEN** no `herdr agent prompt` call is made and the error names the preflight

### Requirement: Step order
`recipe afk` SHALL submit, through `herdr agent prompt <target>`, first the prompt (skipped under `--compact-only`), then `/rename <name>` (only with `--rename`), then `/compact` (always). Every step but `/compact` MUST pass `--wait` unless the target is the caller's own pane. If a step fails, later steps MUST NOT run and the error names the failing step.

#### Scenario: Compact only
- **WHEN** `--compact-only` is set without `--rename`
- **THEN** the only prompt submitted is `/compact`

#### Scenario: Prompt step fails
- **WHEN** the first `herdr agent prompt` call fails
- **THEN** `/compact` is never submitted

### Requirement: Self-target detection
`recipe afk` SHALL treat the target as the caller's own pane only when `HERDR_PANE_ID` is set and equals the target or the pane id reported by the preflight response. `HERDR_ACTIVE_PANE_ID` MUST NOT establish self-ness, and an unparseable preflight response MUST be treated as not-self. On a self-target, `--wait` is dropped from every step and the receipt check is skipped.

#### Scenario: Agent name naming own pane
- **WHEN** `--target reviewer` resolves in the preflight to the pane in `HERDR_PANE_ID`
- **THEN** no step carries `--wait` and no receipt read is made

#### Scenario: Focused pane is not self
- **WHEN** only `HERDR_ACTIVE_PANE_ID` is set and the target is that pane
- **THEN** steps keep `--wait` and the receipt check runs

### Requirement: Compaction receipt check
Unless `--skip-receipt` is set or the target is self, `recipe afk` SHALL read the pane before submitting and then poll it up to 10 times, 2 seconds apart, succeeding only when a receipt marker appears that was absent from the pre-submission read. When no new marker appears it MUST exit non-zero, saying herdr accepted the request but compaction was not observed.

#### Scenario: Stale marker on screen
- **WHEN** the pane already showed `Compacted` before submission and no new marker appears
- **THEN** the command exits non-zero

#### Scenario: Receipt skipped
- **WHEN** `--skip-receipt` is set
- **THEN** the command exits 0 after the last step is accepted without reading the pane
