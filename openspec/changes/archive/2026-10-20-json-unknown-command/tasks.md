## 1. Behavior

- [x] 1.1 Answer an unknown verb or subverb asked for with `--json` with the failure object, code `unknown_command`, before cobra reports the flag
- [x] 1.2 Keep the exit code of the same call without `--json`
- [x] 1.3 Test the root and every group, both `--json` positions, and the no-JSON cases

## 2. Docs

- [x] 2.1 List `unknown_command` in `docs/json-contract.md` (no verb or flag changed, so no `docs/commands/` page or README roster change)
- [x] 2.2 Archive this change so the requirement lands in `openspec/specs/agent-contract/`
