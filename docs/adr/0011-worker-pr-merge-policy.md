# 0011. Worker PR merge policy: a gate check GitHub enforces, off by default

**Status: Accepted**

Date: 2026-10-05

## Context

The herdr coordinator plan refused auto-merge ("No standing daemon, PR poller or auto-merge. Merge stays manual."). The atelier plan (private meta-repo, `docs/plans/2026-10-05-atelier-a-herdr-work-queue-cockpit-for-claude-code.md`) reverses that for one case: a detached drain dispatches queued tasks, and a worker's PR may merge without the operator when a written policy allows it.

Three facts shape the design, each checked against live GitHub on 2026-10-05:

- **GitHub enforces nothing today.** forgectl's `estate-main` ruleset requires 0 approving reviews, no status checks, and has no bypass actors. Any identity with pull-requests write can merge through the API. A policy forgectl checks before *its own* merge call does not stop a merge made by anyone else, including a worker.
- **Comments prove nothing about identity.** Cadence reviews post as `cameronsjo`, the operator's identity, and GitHub refuses `--approve` on a self-authored PR. A worker on the same identity can post the `cadence-review` marker, talk to CodeRabbit with chat commands, and resolve CodeRabbit's threads.
- **CodeRabbit's commit status is not a review.** A rate-limited PR shows a `CodeRabbit` commit status with `state: success`, description `Review rate limited`, and `creator: null`.

## Decision

1. **GitHub enforces the gate.** A forgectl "merge gate" GitHub App posts a check run named `forgectl/merge-gate` on a PR's head commit. Each eligible repo's ruleset requires that check, pinned to the gate App's id. The App's private key lives outside every worker sandbox (in the operator's keychain), and only the drain and `forgectl surface merge` use it. The gate posts `success` only after every predicate in item 4 holds at that head commit; a later push leaves the new head without the check. Workers use a separate "worker" App (ADR-0010) whose tokens cannot satisfy the gate, so no worker can merge by any path.
2. **Policy lives in forgectl's per-machine config.** A missing file, a missing `[surface.merge]` table, or any key that fails to parse means `mode = "off"`. Fail closed on every error.

   ```toml
   [surface.merge]
   mode = "auto"                 # off | manual | auto
   approvers = ["coderabbit"]    # plug-in list; v1 implements only coderabbit
   method = "squash"
   repos = ["cameronsjo/forgectl"]   # explicit allowlist
   [surface.merge.required_checks]
   "cameronsjo/forgectl" = ["build-test", "lint"]
   [surface.merge.paths]
   "cameronsjo/forgectl" = ["internal/tasks/**", "internal/cli/tasks*.go", "docs/**"]
   ```

   - `off`: the gate posts nothing; nothing merges through forgectl.
   - `manual`: `forgectl surface merge` and the pane's merge button post the gate and merge when the policy passes; the drain never does.
   - `auto`: the drain's autopilot step also does.
3. **Repos that install live from their default branch are never eligible.** A merge to `cameronsjo/cadence` runs in every Claude Code session on every machine at the next start, with no release in between. Such a repo always needs a human merge, and `repos` refuses it.
4. **The gate passes only when every predicate holds, read fresh at one head commit:**
   - The repo is on `repos`, its ruleset requires `forgectl/merge-gate` by the gate App's id, and `required_checks` and `paths` both name at least one entry for it.
   - Each named required check is a check run from the GitHub Actions App (id `15368`), from the expected workflow file, concluded `success` at the head. Commit statuses never count.
   - At least one configured approver has passed the head (item 5).
   - The PR is not a draft, its mergeable state is `MERGEABLE` (`UNKNOWN` fails), it targets the default branch, and its head repo equals its base repo.
   - The PR's head ref is the branch the drain created for a ledger row, and the head descends from the base commit the drain recorded at launch. A PR number from a worker's report is never trusted.
   - **Every changed path matches the repo's `paths` allowlist.** Anything outside it is refused, which covers CI, lint and release config, build files, instruction files, `.coderabbit.yaml`, and the gate's own code without listing them. Renames are checked under both names. Symlinks, submodule entries, and file-mode changes are refused. The file list must be complete: the gate fails closed when the listing hits the API's limit or its count differs from the PR's `changed_files`.
5. **Approvers are a plug-in list, matched by an identity a worker cannot use.** v1 implements `coderabbit`: a pull-request review by user id `136622811` (`coderabbitai[bot]`) whose `commit_id` equals the head, whose body is a completed review (the exact shape pinned by captured fixtures before code), with no unresolved bot threads at any commit. The PR is disqualified if any non-bot identity resolved a bot thread or mentioned `@coderabbitai`. Commit statuses, empty-body reviews, chat replies, and any other id never count. CodeRabbit is necessary, never sufficient: the path allowlist and required checks still apply. `cadence-review` is deferred until it posts under an identity workers cannot hold.
6. **The merge is bound to the gated commit.** forgectl runs `gh pr merge --<method> --match-head-commit <sha>` with the operator's identity, where `<sha>` is the head the gate passed. Admin bypass is never used.
7. **Every merge and every refusal is audited twice.** forgectl appends one JSON line to `merge-audit.jsonl` in its state dir: time, actor (`drain`, `cli`, or `pane`), PR, gated SHA, a hash of the policy in force, each check and its state, the approver evidence (approver, id, SHA, link), and the result with reasons. Each line carries the hash of the previous one. The same row goes into the gate check run's output on GitHub, which a local process cannot edit. forgectl never prunes the local file; an operator rotates it by hand (runbook). `forgectl surface audit --pr <owner/repo#N>` prints a PR's rows.
8. **Kill switch.** `forgectl surface drain stop` stops the drain and revokes live worker tokens. `mode` is re-read on every drain tick and every `surface merge` call, so `mode = "off"` stops the next gate post without a restart.
9. **The config is guarded.** Worker sandboxes and deny rules cover forgectl's config dir (ADR-0010). `mode != "off"` is refused (not warned) when the config is a symlink or chezmoi-managed. Work machines stay `off`.
10. **A security review gates the switch.** A security review on an Opus-class model reads the gate, intake, and App-token code, `.github/workflows/ci.yml`, and the live ruleset of every repo on `repos`; a mutation sweep forces each predicate true and confirms a test goes red. Both happen before any machine sets `mode = "auto"`.

## Consequences

- No identity but the gate App can make a worker PR mergeable, and the gate App's key never enters a sandbox. A worker cannot approve or merge its own PR by any path.
- Setup grows: two GitHub Apps (worker and gate), and a ruleset change per eligible repo. The ruleset change also stops the operator merging a non-worker PR there without the gate, so each eligible repo's ruleset gives the Admin repository role a `pull_request`-mode bypass, matching the existing `cameronsjo/cadence` ruleset. Only the operator holds that role; neither App does, and forgectl never uses the bypass. A coordinator session on the operator's identity could use it, which is the same exposure as today.
- Passing checks prove only that the PR's own tests and lint passed. The path allowlist is what keeps a worker away from the files those checks are configured by.
- The gate depends on CodeRabbit reviewing. When it is rate-limited or uninstalled, PRs wait for the operator.
- Every merge decision can be explained after the fact from the gate check's output, with the local log as a second copy.
- The coordinator plan's "no daemon, poller, or auto-merge" line is reversed by amendment, which names which original concern each part of this ADR answers.

## Alternatives considered

- **forgectl checks the policy and merges, with nothing on GitHub's side.** Any other merge path bypasses it. Declined after the ruleset check above.
- **Merge on the `cadence-review` marker.** Forgeable by any session on the operator's identity. Declined.
- **A refused-paths denylist.** The set of files that change what runs keeps growing (CI scripts, lint config, release config, `go.mod` `replace` lines, instruction files). Replaced by a per-repo allowlist.
- **GitHub's native auto-merge with branch protection alone.** It cannot express the path allowlist or approver identity rules. The gate check carries those, and GitHub's required-check rule enforces it.
- **A policy file in each repo.** A worker can edit its own repo. Declined: the policy lives outside every worktree.

## Amendment 2026-10-06: the worker App is deferred

ADR-0010's amendment of the same date keeps workers on the operator's GitHub identity, and defers the worker App and the restrict-updates ruleset (forgectl#1134). This policy's guarantee that a worker cannot approve or merge its own PR rests on both. Until they exist, a worker can merge with the operator's identity, and only its brief asks it not to. Auto-merge (`mode = "auto"`) does not ship until forgectl#1134 lands or this ADR is revisited.
