## Why

With `--json`, an unknown verb or subverb (`forgectl zzbogus --json`) still printed fang's styled `ERROR` frame and said `Unknown flag: --json`: cobra parses flags before it validates positionals, and no group declares `--json`. The message names the wrong problem, and a caller probing for a verb this forgectl lacks (cadence's atelier mod) has to scrape the human frame. This is part of the owned-concessions sweep (cameronsjo/cadence-ecosystem#649); there is no forgectl issue.

## What Changes

- An unknown verb or subverb asked for with `--json` writes one `{"error","code","path"}` object on stderr with the new code `unknown_command`, and nothing else.
- The exit code is the one the same call gets without `--json` (2, or 1 under `tasks`).
- Without `--json` (or with `--json=false`, or `--json` after `--`) the human error is unchanged.
- `docs/json-contract.md` lists the code.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `agent-contract`: adds a requirement for the `unknown_command` failure object, and names that code in the JSON failure object requirement.

## Impact

- Changed: `internal/cli/unknown_help.go` (`unknownCommandJSON`), `internal/cli/execute.go`, `internal/cli/json_errors.go`, `docs/json-contract.md`.
- New `--json` failure code `unknown_command`. No exit code changes.
