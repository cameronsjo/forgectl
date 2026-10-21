## Why

Five cadence skills need one file from forgectl's embedded skill (`references/board-tasks.md` or `references/desk.md`). The only way to read one today is `forgectl --skill --install` into a scratch directory, then a read of the file from there. This is part of the owned-concessions sweep (cameronsjo/cadence-ecosystem#649); there is no forgectl issue.

## What Changes

- `forgectl --skill <reference>` prints one file from the skill's `references/` to stdout and exits 0. The name is the file name (`desk.md`) or the path SKILL.md links it by (`references/desk.md`).
- An unknown name, a second name, or a name with `--install` exits 2. The unknown-name error lists the valid names.
- `forgectl --skill` alone and `forgectl --skill --install <dir>` are unchanged.
- The `--skill` help line, SKILL.md and the README say a reference can be printed by name.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `agent-contract`: adds the embedded agent skill, which no spec covered: `--skill`, `--skill <reference>` and `--skill --install <dir>`.

## Impact

- Changed: `internal/skill/skill.go`, `internal/cli/skill.go`, `internal/cli/root.go` (the root accepts a positional only with `--skill`), the skill's SKILL.md, `README.md`.
- No new exit code, no `--json` change.
