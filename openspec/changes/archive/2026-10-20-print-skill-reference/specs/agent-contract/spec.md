## ADDED Requirements

### Requirement: The binary prints its own agent skill
`forgectl --skill` SHALL print the embedded SKILL.md to stdout and exit 0, so an agent's instructions match the binary it runs.

#### Scenario: Print the skill
- **WHEN** `forgectl --skill` is run
- **THEN** stdout is the embedded SKILL.md and the exit status is 0

### Requirement: The binary prints one skill reference by name
`forgectl --skill <reference>` SHALL print one file from the skill's `references/`, named by its file name or by `references/<name>`, and exit 0. An unknown name, a second name, or a name with `--install` MUST exit 2 with nothing on stdout. The unknown-name error MUST list every valid name.

#### Scenario: Print one reference
- **WHEN** `forgectl --skill desk.md` is run
- **THEN** stdout is the embedded `references/desk.md` and the exit status is 0

#### Scenario: Unknown reference
- **WHEN** `forgectl --skill nope.md` is run
- **THEN** the exit status is 2, stdout is empty, and the error names `board-tasks.md` and `desk.md`

### Requirement: The binary installs its own agent skill
`forgectl --skill --install <dir>` SHALL write SKILL.md and `references/` into an existing absolute directory. It MUST exit 2 for a relative, missing or empty directory, and for `--install` without `--skill`.

#### Scenario: Install the whole skill
- **WHEN** `forgectl --skill --install /abs/dir` is run and `/abs/dir` exists
- **THEN** `/abs/dir/SKILL.md` and `/abs/dir/references/desk.md` are written and the exit status is 0

#### Scenario: Relative install directory
- **WHEN** `forgectl --skill --install rel/dir` is run
- **THEN** the exit status is 2 and nothing is written
