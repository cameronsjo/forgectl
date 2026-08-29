# Forgectl #422 — Release Please as the Single Changelog Writer

## Goal

Eliminate the unreconciled two-writer changelog workflow: Release Please must be
the only process that writes `CHANGELOG.md`, while consumer-visible pull requests
retain richer release prose through Release Please's supported commit-message
override in the merged pull-request body.

## Current evidence

- Release PR #423 inserts `0.14.2` above the populated `## [Unreleased]` block,
  leaving #421's already-shipped-intent prose labeled unreleased.
- Release Please documents `BEGIN_COMMIT_OVERRIDE` / `END_COMMIT_OVERRIDE` in a
  merged squash PR body as the supported way to replace the commit message used
  for generated release notes.
- The repository squash-merges, so the override mechanism applies here.
- Release PR #423 is still open and v0.14.1 is still the latest release, leaving
  a window to migrate #421 before the next tag.

## Chosen approach

Use one changelog writer:

1. Append a valid Release Please commit override to merged PR #421 containing
   the detailed `docs serve` note currently under `[Unreleased]`.
2. Re-run the existing Release Please workflow and verify #423 generates that
   detail from #421's PR body before deleting its file-based source.
3. Remove the standing `[Unreleased]` block from `CHANGELOG.md`.
4. Add project-local agent guidance and a pull-request-template reminder that
   consumer-visible detail belongs in a PR-body override, not in the generated
   changelog.
5. Add a non-vacuous repository test that rejects any future `[Unreleased]`
   heading in `CHANGELOG.md` and confirms generated version sections remain.
6. After the migration PR lands, add a PR-level CI gate that rejects every
   `CHANGELOG.md` edit except changes from the exact Release Please bot and
   same-repository release branch. Delivering that gate second avoids weakening
   it with a permanent bootstrap exception for this migration.

This retains rich notes without a custom parser, another credentialed workflow
writer, or a manual post-release relocation step.

## Alternatives declined

- **Workflow post-processor:** preserves `[Unreleased]`, but adds a second
  credentialed writer plus Markdown section-merging and idempotency logic — the
  same ownership shape behind #422 with more machinery.
- **CI detection only:** makes the defect red but still requires a human to move
  entries at exactly the release boundary.
- **Generated subjects only:** gives Release Please sole ownership but discards
  the richer explanation the existing convention was created to preserve.

## Boundaries

- Do not merge release PR #423 as part of this issue; verify its regenerated
  diff and leave the actual release decision separate.
- Deliver the migration and the PR-level ownership gate as two reviewed PRs.
  The migration references #422; only the enforcement PR closes it.
- Preserve PR #421's existing body byte-for-byte and append only the override.
- Do not add a custom Release Please plugin, changelog parser, workflow token,
  or runtime dependency.
- Historical plan files remain historical; do not rewrite their old
  `[Unreleased]` requirements as though they were never the rule.

## Checklist

- [x] Revalidate #422, release PR #423, v0.14.1, and current `origin/main`.
- [x] Record Cameron's approval of the single-writer approach.
- [x] Persist this plan in its own commit before implementation.
- [x] Migrate #421's detail into a PR-body commit override without altering its
      existing body.
- [x] Re-run Release Please and prove #423 consumes the override.
- [x] Add project-local agent and pull-request guidance for future overrides.
- [x] Remove `[Unreleased]` and add the single-writer regression guard.
- [x] Run focused guard tests, the full relevant root-package suite, workflow
      lint, and mutation/control checks that prove the guard can fail.
- [x] Attempt the repository-wide Go suite, record the listener sandbox
      boundary, and retain GitHub CI as the authoritative full-suite gate.
- [x] Finish migration-PR polish, fix or disposition every finding, and update
      this checklist in the implementation commit.
- [ ] Push the migration PR with `Refs #422`, inspect its exact remote head/diff,
      and monitor CI and review to terminal state.
- [ ] Merge the migration only after its verified gates pass; prove #423
      regenerates without `[Unreleased]` or lost #421 detail.
- [ ] Branch from migrated main and add the PR-level bot/branch ownership gate.
- [ ] Mutation-test both ordinary-PR refusal and Release Please allowance.
- [ ] Polish, deliver, and merge the enforcement PR with `Closes #422` after CI
      and review pass.
- [ ] Prove #422 is closed and #423 remains a valid generated release PR.

## Execution evidence

- PR #421's body grew from 5,148 to 5,499 characters; removing the exact
  351-character override suffix reproduces the original length and ending, and
  exactly one begin/end marker exists.
- Release Please run `33279915839` passed on main `9d76e49`; #423 regenerated at
  `e01dad6` with the detailed `docs serve` override and without the old terse
  squash subject.
- The focused guard was mutation-tested: restoring `[Unreleased]` made
  `TestChangelogHasNoUnreleasedLedger` fail at its ownership assertion;
  removing it made the same test pass.
- `go test . -count=1`, `actionlint .github/workflows/*.yml`, and
  `git diff --check` pass with the matching Go 1.26.5 toolchain. `go test ./...
  -count=1` was also run; packages that do not require listeners passed, while
  the sandbox refused TCP/Unix listener creation (`operation not permitted`)
  in the known bench/docs/cli/surface/tmux integration tests. GitHub CI remains
  the authoritative full-suite gate.
- Polish found that rejecting `[Unreleased]` alone does not enforce the stronger
  single-writer rule: an ordinary PR could still edit a generated version
  section. The plan therefore split delivery so the post-migration PR can add a
  strict author-and-branch CI gate without a lasting one-time exception.
- The migration fix-round found no remaining phase-1 issue. Phase 2 must verify
  the exact bot login and same-repository head repository in addition to the
  exact release branch name; a branch-name match alone is not an identity gate.
