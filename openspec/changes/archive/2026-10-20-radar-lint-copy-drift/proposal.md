## Why

cadence-ecosystem vendors two release scripts byte for byte into every `release-pr` repo: `.github/scripts/ship-gate.sh` and `.github/scripts/lint-release-env.sh`. The release radar checks only the gate copy for drift, so a lint copy that falls behind its canonical bytes goes unnoticed. That matters now: cameronsjo/cadence-ecosystem#661 changes the canonical lint, and the vendored copies will need re-vendoring. There is no forgectl issue; the gap is tracked in the owned-concessions sweep (cameronsjo/cadence-ecosystem#649).

## What Changes

- `review releases` reads `.github/scripts/lint-release-env.sh` on each `release-pr` repo's branch and judges it the same way as the gate copy: missing or drifted stalls the row, an unknowable hash makes it `unknown`.
- New flag `--lint-sha256` and env `$FORGECTL_LINT_SHA256` for the canonical lint hash; without them it is the sha256 of `../scripts/release/lint-release-env.sh` beside the registry.
- `--json`: each row gains `lint_copy` (`match`, `drift`, `missing`); the report gains `canonical_lint_sha256`. Existing fields keep their meaning.
- The table gains a `LINT COPY` column after `GATE COPY`.
- Exit codes are unchanged. A registry with no canonical lint script beside it and no flag or env now reads every `release-pr` row `unknown`, which `--fail-on-stall` exits 1 on.

## Capabilities

### New Capabilities

### Modified Capabilities

- `review`: the release radar's drift rule covers the vendored lint copy as well as the gate copy.

## Impact

- `internal/review/releases.go`, `internal/review/releases_github.go`, `internal/cli/review_releases.go` and their tests.
- `docs/commands/projects-and-review.md`.
- cadence-ecosystem's `release-radar.yml` must sparse-check-out `scripts/release/lint-release-env.sh` before it pins a forgectl release with this change, or every `release-pr` row reads `unknown`.
