## 1. Radar

- [x] 1.1 Read `.github/scripts/lint-release-env.sh` on each `release-pr` branch beside the gate copy
- [x] 1.2 Judge the lint copy with the gate's drift rule and report it as `lint_copy`
- [x] 1.3 Resolve the canonical lint hash from `--lint-sha256`, `$FORGECTL_LINT_SHA256`, or the script beside the registry
- [x] 1.4 Add the `LINT COPY` table column

## 2. Tests and docs

- [x] 2.1 Cover a drifted, a matching, a missing, and an unreadable lint copy, and the canonical lint resolution
- [x] 2.2 Update `docs/commands/projects-and-review.md` (the README Usage roster lists no radar flags)
