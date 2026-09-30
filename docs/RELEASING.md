# Releasing forgectl

The full release recipe — goreleaser config, GitHub App auth, quarantine postflight, cask wiring — lives in the personal skill:

```
releasing-to-homebrew-tap
```

Load it in any Claude Code session with `/releasing-to-homebrew-tap` or via the Skill tool.

## How a release ships

Releases are driven by [Release Please](https://github.com/googleapis/release-please) reading Conventional Commits — there is no manual tag step.

1. Every push to `main` runs `.github/workflows/release-please.yml`, which opens or
   updates a standing release PR titled `chore(main): release X.Y.Z`. The PR body
   lists every Conventional Commit since the last release.
2. Merging that PR updates `CHANGELOG.md` and bumps
   `.release-please-manifest.json` — it does **not** tag or publish anything.
3. That manifest bump triggers `.github/workflows/auto-tag.yml`, which reads the
   new version out of `.release-please-manifest.json`, pushes the `vX.Y.Z` tag,
   and flips the release PR's `autorelease: pending` label to
   `autorelease: tagged`. It tags only when the pushed commit is the Forge
   Bellows App's own merge of its release PR: its `gate` job checks, by
   account login, that `artificer-forge-bellows[bot]` opened and merged a PR
   with the exact title and that the PR merged as this commit. The nightly
   `ship.yml` gate is that merge. A bump that reaches `main` any other way (a
   hand merge, a PR that edits the manifest) is left untagged with a notice;
   see "An ungated bump" below.
4. The pushed tag triggers `.github/workflows/release.yml`. Its first job,
   `verify`, fails closed unless the tagged commit is on `main` and the CI
   workflow's `push` run for that commit succeeded. Only then does it run
   `goreleaser release --clean` — builds all targets, signs the macOS bless
   helper, cuts the GitHub Release, and pushes the Homebrew cask to
   `cameronsjo/homebrew-tap/Casks/forgectl.rb`.

`release-please-config.json` sets `skip-github-release: true`, which is what hands
tagging off to `auto-tag.yml` instead of letting release-please tag directly —
this keeps the GoReleaser pipeline untouched, since a hand-pushed tag would fire
`release.yml` exactly the same way.

## Manual tagging is forbidden

Do not run `git tag vX.Y.Z && git push origin vX.Y.Z` by hand, even as a break-glass
option. A manually pushed tag does trigger `release.yml` and produces a working
build — that's exactly what makes it dangerous. It desyncs
`.release-please-manifest.json` from the tag that was actually shipped, and it
never flips the release PR's `autorelease: pending` label.

`auto-tag.yml` documents the resulting wedge directly (lines ~12-17): with
`skip-github-release: true`, release-please's own label flip is skipped too, so a
release PR that never gets tagged through the normal path stays
`autorelease: pending` forever. Every subsequent `release-please` run then aborts
with "There are untagged, merged release PRs outstanding" — inside an otherwise
green job — and no new release PR is ever opened again. Recovering from a
hand-pushed tag means manually reconciling the manifest and the PR label; avoid
creating the wedge in the first place by always releasing through the release PR.

The `v*` tag ruleset now enforces this: only the Forge Bellows App can create,
update, or delete a `v*` tag, so a hand-pushed tag is refused.

## An ungated bump

If the release PR is merged by hand, or another PR edits
`.release-please-manifest.json`, `auto-tag.yml` does not tag and logs a
notice. Nothing is released, and a hand-merged release PR stays
`autorelease: pending`, which wedges release-please as described above.

To recover (first run 2026-09-30, #887 → #900 → #902):

1. Remove the `autorelease: pending` label from the hand-merged release PR.
   Do this first: release-please runs on the revert's push to `main`, and while
   the label is still there it logs `There are untagged, merged release PRs
   outstanding - aborting` and opens nothing. If that happens anyway, re-run
   that Release Please run (`gh run rerun <id>`) once the label is gone.
2. Revert the bump commit on `main` through a normal PR. Its `lint` job fails
   the CHANGELOG ownership check by design, because the revert removes the
   release's CHANGELOG section and only the release bot may edit that file.
   That failure is expected; `main` requires no status checks, so the revert
   can still merge.
3. release-please reopens the release PR. Once its CI is green, let the nightly
   gate merge it, or run it now with
   `gh workflow run ship.yml -R cameronsjo/forgectl -f dry_run=false`. The
   gate's merge is what `auto-tag.yml` tags.

## Release notes

`CHANGELOG.md` is release-please-owned and CI-enforced
(`scripts/check-changelog-owner.sh`) — never hand-edit it in a feature or fix
branch, and never add an `[Unreleased]` section.

For a consumer-visible change that needs richer prose than the squash commit
title, put a `BEGIN_COMMIT_OVERRIDE` / `END_COMMIT_OVERRIDE` block in the PR body
before merge:

```text
BEGIN_COMMIT_OVERRIDE
fix(scope): describe the consumer-visible change
END_COMMIT_OVERRIDE
```

Release Please reads that block from the merged squash PR and generates the
versioned changelog entry from it. See `AGENTS.md` for the full contract.

## Verify a release build locally

```bash
mise exec -- goreleaser check
mise exec -- goreleaser release --snapshot --clean --skip=publish
cat dist/homebrew/Casks/forgectl.rb
```

A clean `check` plus a snapshot that writes `dist/homebrew/Casks/forgectl.rb`
means the next tag will build cleanly. This only proves the build works locally
— it does not ship anything. Merge the release PR to actually release.
