## Why

`projects list --json` is the only forgectl verb whose row keys use camelCase (`sshUrl`, `localPath`). Every other verb uses snake_case. On 2026-10-14 Cameron ruled that JSON keys are snake_case and that changes stay additive under ADR-0008: keys are added beside the old ones, never renamed (cameronsjo/forgectl#1095, Linear CAM-243).

## What Changes

- Each `projects list --json` row also carries `ssh_url` and `local_path`, with the same values as `sshUrl` and `localPath` and the same omission rules. The camelCase keys stay.
- `--fields` accepts both spellings.
- `surface status` and `surface usage` on an unknown worker name point to `forgectl surface list`, as `surface close` and `surface read` already do.

## Capabilities

### Modified Capabilities

- `projects`: JSON rows gain the snake_case keys.

## Impact

- JSON output: two keys added to `projects list --json` rows. The change is additive.
- Docs: `docs/commands/projects-and-review.md`.
