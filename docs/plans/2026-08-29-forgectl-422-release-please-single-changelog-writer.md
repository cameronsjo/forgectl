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
- Preserve PR #421's existing body byte-for-byte and append only the override.
- Do not add a custom Release Please plugin, changelog parser, workflow token,
  or runtime dependency.
- Historical plan files remain historical; do not rewrite their old
  `[Unreleased]` requirements as though they were never the rule.

## Checklist

- [x] Revalidate #422, release PR #423, v0.14.1, and current `origin/main`.
- [x] Record Cameron's approval of the single-writer approach.
- [x] Persist this plan in its own commit before implementation.
- [ ] Migrate #421's detail into a PR-body commit override without altering its
      existing body.
- [ ] Re-run Release Please and prove #423 consumes the override.
- [ ] Add project-local agent and pull-request guidance for future overrides.
- [ ] Remove `[Unreleased]` and add the single-writer regression guard.
- [ ] Run focused guard tests, the full relevant Go suite, workflow lint, and
      mutation/control checks that prove the guard can fail.
- [ ] Run polish, fix or disposition every finding, and update this checklist in
      the implementation commit.
- [ ] Push, open the #422 PR, inspect the exact remote head/diff, and monitor CI
      and review to terminal state.
- [ ] Merge only after the verified gates pass; then prove #422 closes and #423
      regenerates without an `[Unreleased]` block or lost #421 detail.
