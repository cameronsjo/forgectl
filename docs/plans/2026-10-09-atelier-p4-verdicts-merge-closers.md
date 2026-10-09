---
status: in-flight
next: "T10.1 merged (cameronsjo/cadence-hooks#1357; release pending). T10.2 in review on cameronsjo/forgectl#1207. T10.3 (closers, prune) built on feat/p4-closers, PR not opened yet. T10.4 (merge, audit, autopilot, brief rule, mutation sweep, cameronsjo/forgectl#1208) built on feat/p4-merge, PR not opened yet. Next: the T10.4 security review and live check."
branch: plan/atelier-p4
pr: "cameronsjo/forgectl#1207"
updated: 2026-10-09
approved_session_id: 30dd3ebb-3720-463f-aa15-9570c1ff88a9  # operator: 'own it and get it done', 2026-10-09; auto-merge approver chosen by the operator the same day
date: 2026-10-09
session_id: 30dd3ebb-3720-463f-aa15-9570c1ff88a9
model: claude-opus-5-5
harness: claude-code 2.1.289
machine: cf6e768835c7
source_plan: "cadence-ecosystem docs/plans/2026-10-05-atelier-a-herdr-work-queue-cockpit-for-claude-code.md § P4; docs/plans/2026-09-28-forgectl-herdr-coordinator.md T10; ADR-0011"
---

# atelier P4: verdicts, usage, merge, closers (forgectl T10)

## Goal

A drain worker's PR can be seen, merged and cleaned up without the operator walking it by hand:

- `forgectl surface status <name> --json` shows the worker's PR, its required checks, the review evidence at its head, the merge policy's verdict with reasons, and the session's cost.
- `forgectl surface merge <name>` merges it when the policy passes, bound to the verified head, with a message forgectl writes, and records an audit line either way. With `mode = "auto"` the drain does the same on its own.
- The drain closes a worker after its PR merges, or 24 hours after it closes unmerged, and `surface prune` clears old rows.

## What changed since ADR-0011

ADR-0011 (2026-10-05) planned a GitHub App posting a required `forgectl/merge-gate` check, a `coderabbit` approver by bot id, and declined the `cadence-review` marker as forgeable. Three facts moved:

- **No worker App and no gate App** (ADR-0010 and ADR-0011, 2026-10-06 amendments; forgectl#1134). Workers are full harnesses on the operator's `gh` login (ADR-0010, 2026-10-08), so a worker can run `gh pr merge` itself. This gate stops the drain merging by accident and keeps it inside a written policy; it cannot stop a worker.
- **CodeRabbit rarely reviews.** Of the last 25 merged PRs, CodeRabbit reviewed 3 on forgectl, 2 on cadence and 5 on cadence-hooks; on most of the rest its head status reads `success` "Review rate limited" (measured 2026-10-09).
- **The operator chose the marker (2026-10-09)** over deferring auto-merge or CodeRabbit only, accepting that a session on his identity can post one.

The ADR-0011 amendment is the first commit of T10.2, before any code it authorises.

## Accepted boundary (operator, 2026-10-09)

A session on the operator's identity, a misled worker included, can post a passing `cadence-review` marker, push to its branch, and write forgectl's config and state files. What still holds against an accident: built-in path refusals the config cannot widen, the per-repo path allowlist, required Actions check runs pinned to their workflow file, the head bound to a fresh drain branch, `--match-head-commit`, and a merge message forgectl writes. Drain-merged commits ride the nightly release (`ship.yml`) like any other merge; the built-in refusals keep every top-level file, CI and agent configuration, `scripts/`, `helper/`, the CLI package and every package the merge path compiles in out of that path.

## Design

### Shared core: `internal/surface/merge` (T10.2)

`merge.Evaluate(facts Facts, policy Policy) Verdict` is a pure function: `Facts` holds everything read from GitHub for one PR at one head SHA, `Policy` the resolved `[surface.merge]`, and `Verdict{Result: pass|refuse|off, Reasons []string}`. `surface status`, `surface merge` and the drain all call it; nothing else decides.

### `surface status <name> [--repo <path>] --json` (T10.2)

- **PR discovery** by head branch `worker/<name>` on the row's recorded repository (below). Filter before ordering: `isCrossRepository == false`, head repository id equals the base repository id, author id equals the operator's id. More than one open PR on the head refuses. A PR number from a worker's report is never used.
- **Reads, all bound to one head SHA:** one GraphQL query for state, `isDraft`, `baseRefName`, `baseRefOid`, `headRefOid`, `mergeable`, `mergeStateStatus`, `changedFiles`; check runs from `commit(oid: head)` with their suite's workflow path and event; reviews with author id, state, `submittedAt` and `commit.oid`. Connections are paginated to the end, or the read refuses on a remaining page. Files come from `GET compare/{baseRefOid}...{head}` (both SHAs explicit), paginated, and must match `changedFiles`. File modes come from the git tree at base and head.
- **Shape:** `{name, repo, pr:{number,url,state,isDraft,headSha,baseRef,mergeable,mergeStateStatus,changedFiles}, checks:[{name,workflow,event,conclusion}], reviews:[{reviewer,sha,crit,imp,url}], policy:{mode,verdict,reasons[]}, usage:{costUsd,byModel,priced,unpricedModels}|null}`.
- **Cache:** only head-immutable data (the file list and modes), keyed by head SHA. `surface merge` and the drain's merge never read the cache.
- **Usage:** `cadence-hooks metrics price --transcript <row transcript> --json` when `cadence-hooks` is on `PATH` (T10.1), else `null`.

### Merge policy (`[surface.merge]`, T10.2; used by T10.4)

```toml
[surface.merge]
mode = "off"                       # off | manual | auto; missing table or any error resolves to off
machine = "<12-hex>"               # digest of hostname + salt; a mismatch resolves to off
approvers = ["cadence-review"]     # cadence-review is the one approver implemented
marker_author_id = 0               # the operator's numeric GitHub user id; required whenever mode is not off
required_reviewers = ["cadence-forge-security-reviewer", "polish"]
method = "squash"
repos = ["cameronsjo/forgectl"]
[surface.merge.workflow]
"cameronsjo/forgectl" = ".github/workflows/ci.yml"
[surface.merge.required_checks]
"cameronsjo/forgectl" = ["build-test", "lint", "macos-test"]
[surface.merge.paths]
"cameronsjo/forgectl" = ["internal/tasks/**", "docs/**"]
```

Resolution re-reads the config on every drain tick and every `surface merge`. The config is opened without following a symlink and must be a regular file owned by the user with mode `0600`; otherwise `mode` resolves to `off`. Unknown keys refuse. `**` is allowed only as a whole path segment after at least one literal segment; a bare `*` or `**` refuses at load.

The verdict is `pass` only when every predicate holds at one head SHA:

1. **Mode:** `manual` or `auto` (`auto` for the drain), `machine` matches.
2. **Repository:** the row's recorded repository (canonical `nameWithOwner` and `databaseId` captured from GitHub at launch, never re-derived from `.git/config`) is on `repos`, compared case-insensitively against the canonical name, with `workflow`, `required_checks` and `paths` entries. Any repository a `workbench` marketplace entry sources from, plus `cameronsjo/cadence`, `cameronsjo/cadence-lab`, `cameronsjo/workbench` and the dotfiles repos, is refused whatever the config says.
3. **Row:** non-empty `LaunchID` matching the drain's claimed queue row, `Branch == "worker/" + Name`, `BranchFrom == "new"`, stage launched or reported. The row's `Base` is an ancestor of the PR's `baseRefOid` (`compare/{Base}...{baseRefOid}` is `ahead` or `identical`), and the head descends from `Base`.
4. **PR:** open, not draft, targets the default branch, `mergeable == MERGEABLE`, `mergeStateStatus` is `CLEAN` or `HAS_HOOKS`.
5. **Checks:** only check runs whose suite GitHub ties to this PR count (the suite's head branch is the PR's and its `matchingPullRequests` include the PR); for each required name, at least one run at the head whose suite came from the pinned workflow file on a `pull_request` event concluded `SUCCESS`. Any matching run at the head that is not `SUCCESS`, or a run with no workflow run behind it, refuses; a run from the pinned file on any other event never counts and refuses unless it is `SUCCESS`. Commit statuses never count.
6. **Paths:** every changed path matches the repo's globs, segment-wise, case-sensitively, against the exact bytes. Refused whatever the config says: a path with an empty, `.` or `..` segment, a leading `/`, a backslash, a control byte, a non-ASCII byte or invalid UTF-8; file status other than added, modified, removed or renamed (renames checked under both names); any mode other than `100644` or a mode change; a removed `*_test.go`; any `go.mod`, `go.sum` or `go.work`; other languages' module, lock, build and toolchain files at any depth (see Deviations); every top-level file (any path with no `/`: `main.go`, `go.mod`, `.golangci.yml`, `.goreleaser.yaml`, the release-please files, `AGENTS.md`, `CLAUDE.md`, `.coderabbit.yaml`, and any root configuration added later); `.github/**`, `.claude/**`, `scripts/**`, `helper/**`, `internal/cli/**`, and the bless, signing and self-update helpers with `internal/workflow/**`, `internal/digest/**` and the shipped skill `internal/skill/**` (see Deviations); and every package compiled into the merge path (`internal/surface/**`, `internal/config/**`, `internal/launch/**`, `internal/pr/**`, `internal/module/**`, `internal/termsafe/**`, `internal/privdir/**`, `internal/gitenv/**`, `internal/exec/**`, `internal/githubauth/**` and the rest of their imports, listed in `internal/surface/merge/builtin.go`), derived by a `go list -deps` test.
7. **Approver:**
   - **`cadence-review`:** reviews by `marker_author_id` (`User`, state `COMMENTED` or `APPROVED`, `submittedAt` set, never `PENDING`), whose first line matches `^<!-- cadence-review: [a-z0-9-]{1,40} head=[0-9a-f]{40} crit=(0|[1-9][0-9]{0,3}) imp=(0|[1-9][0-9]{0,3}) -->$` with no BOM or leading space. Each name in `required_reviewers` needs its latest marker at the head, with `head=` equal to the review's commit and `crit=0 imp=0`. Open findings fail closed: any review by `marker_author_id` (any state, any commit) or PR conversation comment by it that mentions `cadence-review:` anywhere, in any case, and is not a strict passing marker at the head refuses unless a later strict passing marker at the head by the same reviewer exists; one naming a reviewer that does not parse refuses outright (see Deviations, T10.2 independent review).
   - **`coderabbit`:** dropped (see Deviations, T10.2 independent review): it reviewed only 2 to 5 of the last 25 merged PRs per repository, and its findings live only in review bodies and its mentions are editable comments, so no rule over them could be made sound. Any `approvers` entry other than `cadence-review` refuses at load; the key stays a list for a future approver with an identity a worker cannot use.

### Merging (T10.4)

- Immediately before merging, re-read `headRefOid`, `baseRefName`, `isDraft`, `state` and the reviews; any change aborts.
- `gh pr merge <n> -R <nameWithOwner> --squash --match-head-commit <sha> --subject <s> --body <b>` from a neutral working directory, never the worker's worktree, never `--admin`. The subject is the PR title only if it matches `^(fix|feat|docs|refactor|test|chore)(\([a-z0-9-]+\))?: [ -~]{1,72}$` (no `!`), else the merge refuses. The body is written by forgectl: the audit hash, policy hash, checks, marker evidence, and a `Merged-By: forgectl-drain` or `forgectl-cli` trailer. No PR text is copied, so no closing keyword, `BREAKING CHANGE` or `Release-As` reaches `main`.
- After merging, confirm the merge commit is on the default branch and record it.
- **Audit:** `merge-audit.jsonl` in the state dir, one line per merge and per refusal (refusals rate-limited to one per PR, head and reason set), each carrying the previous line's SHA-256. The chain detects accidental damage, not tampering by the operator's own user; the squash body puts each merge's hash on `main`, which refuses force-push. `forgectl surface audit --pr <owner/repo#N>` prints a PR's lines through `termsafe` and checks the chain.

### Closers and prune (T10.3)

- **New queue fields:** `pr_closed_at` and `cost_usd`; the queue version is unchanged as in P3 (restart an older drain after upgrading).
- The drain's settle step reads PR state, through the T10.2 discovery filter, for `reported` rows at most every 5 minutes per row:
  - `MERGED`: price the session (when `cadence-hooks` is present) into `cost_usd`, then run the same close `surface close` runs; the row goes to `closed`.
  - `CLOSED` unmerged: record `pr_closed_at`; close 24 hours later; reopened cancels and clears it.
  - No PR: leave it `reported`; `surface status` shows "no PR".
- `forgectl surface prune --older-than 30d [--dry-run] --json` removes `closed`, `failed` and `expired` queue rows older than the cutoff and closed ledger rows. It skips any row whose ledger row still has a live workspace. Before removing a priced row it adds its `cost_usd` to `usage-daily.jsonl` (one line per day). The drain runs prune once a day.
- Dropped from the parent plan: board `ready-to-close` and issue-marker removal (board intake deferred; P3 has no marker label).

### `cadence-hooks metrics price --transcript <path> --json` (T10.1)

Reuses the transcript scan and `by_model_json`; prints `{costUsd, byModel, unpricedModels}` in the `sessions.jsonl` shape (5-minute and 1-hour cache writes separate). Registered with the same `CADENCE_BYPASS` exemption as `metrics grade`. Exit 0 on a readable transcript, 1 on an unreadable one.

## Loop

| Thing created | Opened by | Closed by | Who closes |
|---|---|---|---|
| Worker PR | the worker | `surface merge`, the drain's autopilot step (`auto`), or the operator or chief-of-staff | the operator, or the drain under policy |
| Worker workspace and worktree | the drain (P2) | the T10.3 closer after `MERGED`, or 24 h after `CLOSED`; `surface close` by hand, including a reported worker with no PR | the drain, or the operator |
| Queue and ledger rows | P2 and P3 | `surface prune` (daily by the drain, or by hand) | the drain |
| Status cache entries | `surface status` | prune, with their row | the drain |
| `merge-audit.jsonl` lines | every merge and refusal | none, on purpose: append-only; rotated by hand per runbook | the operator |
| `usage-daily.jsonl` lines | prune | none, on purpose: the long-term cost record; rotated by hand | the operator |

## Alternatives declined

- **Defer the autopilot until workers have their own identity.** Recommended; declined by the operator for the marker.
- **CodeRabbit only.** Rarely fires while CodeRabbit is rate limited. It was first kept as a second approver, then dropped in the T10.2 independent review (see Deviations).
- **The gate App and required ruleset check (ADR-0011 item 1).** Deferred with the worker App (forgectl#1134); without a worker App it adds no protection against a worker that can merge directly.
- **Ship-gate refusing releases that contain a drain merge.** The built-in path refusals keep the gate and CI out of drain merges; the rest is what auto-merge is for.
- **A refused-paths denylist alone.** Kept only as the built-in floor under the per-repo allowlist.

## Tasks

### T10.1: `cadence-hooks metrics price` (cameronsjo/cadence-hooks, one PR)

- [ ] Subcommand beside `metrics grade` with the same `CADENCE_BYPASS` exemption; reuse the scan and price table.
- [ ] Test: a finished transcript fixture matches its `sessions.jsonl` `costUsd` within $0.01; an unpriced model is listed; an unreadable file exits 1.
- [ ] Changelog entry and release through cadence-hooks' normal path.

### T10.2: ADR amendment, `[surface.merge]`, `surface status` (forgectl, one PR)

- [x] First commit: ADR-0011 amendment (2026-10-09): marker approver and its boundary, the deferred gate App, the built-in refusals, the honest audit claim, the composed merge message, and that `mode = "auto"` waits for the T10.4 security review.
- [x] Launch records `BranchFrom`, the repository's canonical `nameWithOwner` and `databaseId` on the ledger row; a drain launch whose `worker/<name>` already exists locally or on origin fails without creating anything.
- [x] `[surface.merge]` with the resolution rules above; `internal/surface/merge` with `Facts`, `Policy`, `Evaluate`.
- [x] `surface status` with the discovery filter, bound reads, cache and usage.
- [x] Fixtures captured first: cameronsjo/forgectl#1204 (merged worker PR), #1203 (markers at head and an earlier crit>0 marker), #1199 (rate-limited CodeRabbit status), #1195 (completed CodeRabbit review), plus a synthetic fork PR on the same head name. One test per predicate.

### T10.3: closers and prune (forgectl, one PR; after T10.2)

- [x] Settle transitions and the 24-hour timer; `pr_closed_at`, `cost_usd`.
- [x] `surface prune` with the live-workspace skip and the usage rollup; daily in the drain; `--dry-run`.
- [x] Tests: each settle transition, a fork PR on the head name ignored, prune keeps live rows, rollup sums.

### T10.4: merge, audit, autopilot (forgectl, one PR; after T10.2)

- [ ] Order: T10.3 (with the SelectPR launch binding) must merge before T10.4 or any PR that lets SelectPR drive a close or a merge (chief-of-staff, 2026-10-09).
- [x] `surface merge <name> [--dry-run]` and the drain's autopilot step (`mode = "auto"` only), both calling `Evaluate` on fresh reads, the pre-merge re-read, the composed message, and the post-merge check.
- [x] `merge-audit.jsonl` and `surface audit --pr`.
- [x] Worker brief rule: never post review markers, approve or merge.
- [x] Mutation sweep: force each predicate true in turn; a named test goes red for each (table below, under "T10.4 mutation sweep").
- [ ] Security review (Opus) of the gate's file set (merge package, status reads, merge path, audit, config resolution, drain autopilot, launch recording, worker brief, `.github/workflows/ci.yml` and `ship.yml`, and the live `estate-main` ruleset) before any machine sets `mode = "auto"`.
- [ ] Live check: `mode = "manual"` on sjomba; merge one real worker PR on cameronsjo/forgectl, from the drain, whose paths are inside a narrow allowlist; confirm the squash body, the audit line, and the T10.3 closer.

## Verification

- forgectl: `go test ./...`, `go vet`, golangci-lint; live-captured fixtures; the mutation sweep.
- cadence-hooks: `cargo test --workspace`; the `costUsd` match.
- Live: the T10.4 manual merge and the T10.3 closer on that PR.

## Orchestrator

**Driver:** opus. Trigger: the merge gate is a security control, and its approver is forgeable by design.

## Panel

Panel: plan-reviewer, security-posture-reviewer (Opus) ran — 2 Critical (the same finding), 24 Important/Low folded in; 2 declined (see below)

## Panel review findings declined

- **[Security posture, C1(c)] Ship-gate refusing releases that contain a drain merge.** Declined (Alternatives declined): the built-in refusals keep the gate, module files and CI out of drain merges.
- **[Security posture, I4] Add `required_status_checks` to the `estate-main` ruleset.** A repository settings change outside this plan; offered to the operator as a separate action.

## Deviations from the parent plan

- **Approver:** `cadence-review` markers, first beside `coderabbit` (operator, 2026-10-09), then alone once `coderabbit` was dropped the same day (see Deviations). The parent plan's Goal, Alternatives declined and Global Constraints still name CodeRabbit only; its Deviations section records this.
- **No gate App or ruleset check** (deferred with the worker App).
- **No board `ready-to-close` and no issue-marker removal.**
- **No CHANGELOG task for forgectl:** release-please writes it from commits.

## Deviations

- **T10.2, plan status:** set to `in-flight` in the first T10.2 commit, not the last: `planned` is outside the plans index's closed set and failed `TestPlansIndex` on the branch tip.
- **T10.2, built-in path refusals:** besides the listed set, `internal/githubauth/**` (the host-pinned runner every gate read goes through), `internal/selfupdate/**`, `internal/bless/**` and `internal/cli/workflow_bless*` (the bless and signing helpers, named), `.coderabbit.yml` and `go.work.sum`; built-in refusals match case-insensitively, the per-repository globs case-sensitively. The ADR-0011 amendment names `internal/githubauth/**`.
- **T10.2, approvers:** any one listed approver passing is enough, and CodeRabbit's thread, resolution and mention rules apply to the `coderabbit` approver only. A CodeRabbit thread counts as resolved only when `resolvedBy` is CodeRabbit's id; no capture shows what GitHub reports when CodeRabbit resolves its own thread, so the T10.4 live check should confirm it.
- **T10.2, checks:** a required-name run from another workflow file or another app refuses; a run from the pinned file on another event (`push`) is ignored.
- **T10.2, reads:** the "head descends from Base" half of predicate 3 is a second compare (`{Base}...{head}`); modes come from one tree listing per changed directory at the compare's merge base and at the head, not from base; the compare API lists files on its first page only (up to 300), so the file list is one page, count-checked. The checks query reads the PR's head and base again and fails the read if either moved.
- **T10.2, launch identity:** an origin not on github.com records no identity and the launch goes on (that row is never eligible to merge); a drain launch also asks GitHub whether `worker/<name>` exists, in the identity query.
- **T10.2 review fix C1, open findings:** the "latest marker with crit or imp above 0 and no later passing marker at the head refuses" rule runs outside the approver loop, so it refuses under every approver set (`coderabbit` alone included). `marker_author_id` is therefore required whenever `mode` is not `off`, not only with `cadence-review`; `required_reviewers` stays a `cadence-review` setting. A marker whose `submittedAt` does not parse now refuses under every approver too.
- **T10.2 review fix I1, non-ASCII paths:** a changed path or a config glob holding a byte above 0x7F, or invalid UTF-8, is refused (both names of a rename). A case-folding checkout folds U+017F onto `s`, so `internal/ſurface/x.go` would land on a refused file while matching no refusal.
- **T10.2 review fix I2, built-in refusals widened:** every top-level file is refused (any path with no `/`), and so are `scripts/**`, `helper/**`, the whole `internal/cli/**` and every package compiled into the merge path. "The merge path" is `internal/surface/merge` plus the module packages that `internal/cli`'s `surface*.go` files and `execute.go` import, closed over `go list -deps` for darwin, linux and windows; `TestBuiltinRefusalsCoverTheGateClosure` fails when a package in that closure is not refused. `internal/cli` itself is refused whole but not followed as a root: the package links every command, so its closure is the whole module, `internal/tasks` (the sample allowlist) included. `MarkerPattern` is unexported. The explicit `.coderabbit.*`, `internal/cli/surface_*` and `internal/cli/workflow_bless*` entries are gone, covered by the top-level and `internal/cli/**` refusals.
- **T10.2 review fix I3, checks tied to the PR:** a run counts only when its check suite's `branch` is the PR's head branch and its `matchingPullRequests` include the PR (verified read-only against the live API on #1207 and #1204 on 2026-10-09; `matchingPullRequests` lists open PRs only, and the REST `workflowRun.pull_requests` is matched the same way, by head branch). The latest-run-only rule is gone: any tied run from the pinned file that is not `SUCCESS` refuses, a queued or in-progress one as "still running". A run at the same commit from another PR on the same head branch name is still matched to this PR by GitHub; discovery already refuses two open PRs on that branch. `checks_1204.json` and `checks_1199.json` gained `branch` and `matchingPullRequests` by hand (as an open PR reads); `checks_1207.json` is a live capture with the new query.
- **T10.2 review nits:** the `@coderabbitai` mention rule also reads the PR description (`body` added to the PR query); `workflow`, `required_checks` or `paths` keys differing only in case refuse; the config file must have one hard link (plan-stage I9); `surface status` never shows `pass` on cached data: a cached read that would pass is read again without the cache. ADR-0011 now names the `machine` digest, not the mode and ownership checks, as what stands in for the chezmoi refusal.
- **T10.2 review fixes, launch identity:** a drain launch whose `worker/<name>` exists stays refused, and the error now names the way out (delete the branch locally and on origin, then dequeue and enqueue; or enqueue under a new name), since `surface close` keeps the branch. A hand launch reads the identity best-effort: a failed read warns once, records none, and goes on. A drain launch's failed read is a new class, `ErrGitHubRead`, that requeues the row with no attempt counted and pauses claiming (`github`); the pause is re-checked each tick by letting one claim through, and a successful launch clears it. This supersedes the earlier "a failed read fails the launch" for hand launches.
- **T10.2 review fixes, status:** a compare GitHub answers 404 (a recorded base it does not have) is a refusal reason, exit 0, not a failed read; `reviews` lists markers by `marker_author_id`, not the gh account, and none with a note when it is unset; `usage` gained `priced` and `unpricedModels`, and an unpriced model makes the cost partial, shown as such; model names are stripped of control, bidi and invisible characters.
- **T10.2, fixtures:** #1203 carries no crit>0 marker; the earlier-crit rule is tested on edited copies. A read-only live check (`FORGECTL_MERGE_LIVE=1`) reads #1204.
- **T10.2 independent review (chief-of-staff), `coderabbit` dropped:** the `coderabbit` approver is removed from the implemented set, config validation (an `approvers` entry other than `cadence-review` refuses at load), `Evaluate`, the docs and ADR-0011's amendment: it reviewed only 2 to 5 of the last 25 merged PRs per repository, and its findings live only in review bodies and its mentions are editable comments, so no rule over them could be made sound. The PR query no longer reads review threads or the PR description, which only its thread and mention rules used, and the `pr_1195.json` and `pr_1199.json` fixtures are gone (`checks_1199.json` stays for the commit-status test). This supersedes the "T10.2, approvers" entry, the `coderabbit` case in "review fix C1" and the PR-description part of "review nits".
- **T10.2 independent review (chief-of-staff), open findings fail closed:** the strict marker regex now decides passes only. Every review by `marker_author_id` (any state, `DISMISSED` and `PENDING` included, any commit) and every PR conversation comment by it (now read with `url` and `createdAt`; the comment list still refuses on a second page) whose body mentions `cadence-review:` anywhere, in any case, and is not a strict passing marker at the head is an open finding for each reviewer name after it, cleared only by a strict passing marker at the head from the same reviewer submitted later; each uncleared one is its own reason, naming the review or comment URL and why it did not pass. A name that is not 1-40 of `a-z0-9-` (`cadence-forge:code-reviewer`) or a missing timestamp refuses outright. Consequence: a passing marker at an earlier head, from any reviewer, now refuses until that reviewer passes at the head, and the real #1203 refuses on its first-format `cadence-review: security ...` and `code` markers. This supersedes "review fix C1"'s "latest marker with crit or imp above 0" rule.
- **T10.2 independent review (chief-of-staff), nits:** built-in refusals also cover other languages' module, lock, build and toolchain files in any directory (`Cargo.toml`, `Cargo.lock`, `build.rs`, `rust-toolchain`, `rust-toolchain.toml`, `package.json`, `package-lock.json`, `pnpm-lock.yaml`, `pnpm-workspace.yaml`, `yarn.lock`, `bun.lockb`, `bun.lock`, `.npmrc`, `.yarnrc`, `.yarnrc.yml`, `requirements*.txt`, `pyproject.toml`, `uv.lock`, `poetry.lock`, `Pipfile`, `Pipfile.lock`, `Gemfile`, `Gemfile.lock`, `.tool-versions`, `.mise.toml`, `mise.toml`, `flake.nix`, `flake.lock`, `Dockerfile` or `Makefile` anywhere, and any path under a `.cargo` directory at any depth), with their own reason; the Go module-file reason is unchanged. A run of the pinned workflow at the head on an event other than `pull_request` still never counts, but now refuses, naming its event, unless it is `COMPLETED`/`SUCCESS`; this supersedes "T10.2, checks"' "ignored".
- **T10.3, the drain's old prune is gone:** the per-tick removal of every terminal row past 30 days is replaced by the daily `surface prune`, so a priced row never leaves without its cost in `usage-daily.jsonl`. A `reported` row is no longer removed by age; it waits for the closers or `surface close`.
- **T10.3, ledger rows:** a ledger row has no closed time, so prune ages a `closed` ledger row by `started_at`. A `closed` ledger row is not probed in herdr (its close already closed the workspace); the live-workspace skip applies to queue rows, through their own ledger row (same name and `launch_id`), and keeps a row herdr cannot rule out. Prune reads every ledger file in the state directory, not only those the queue names.
- **T10.3, status cache entries (Loop row):** rows do not record the head a cache entry serves, so prune removes `status-cache-<head>.json` entries last written before the cutoff instead of "with their row".
- **T10.3, closer reads:** at most 4 rows a tick, oldest read first, each at most every 5 minutes; the read times and once-per-condition events live in the drain process, so a restart reads every reported row again. A repository whose id is no longer the recorded one (`merge.ErrRepoChanged`), two open PRs, or any non-transient failure is an `error` event; a network, server or rate-limit failure an `unreadable` event; neither changes the row nor pauses claiming. The read is a new `merge.Reader.Discover` (the discovery query and `SelectPR` alone).
- **T10.3, a refused close:** the row stays `reported` with one `error` event naming why, and `cost_usd` is still written when the session was priced; the close is tried again at the next read.
- **T10.3, usage rollup:** one line per UTC day per prune run; a later run that removes more rows from the same day appends another line for it (sum per day to read). The lines are appended under the queue lock before the rows go, and nothing is removed when they cannot be written.
- **T10.3, daily prune day:** recorded in `drain-prune-day` before the prune runs, so a failed prune is not retried the same day.
- **T10.3 review fix I1, PRs bound to the launch:** `SelectPR` takes the ledger row's `started_at` (`merge.Row.StartedAt`) and drops every PR created before it, so a reused name's earlier PR cannot close a new worker or show in `surface status`. A PR that passes the rest of the filter with a missing or unparseable `createdAt`, or a row with no `started_at`, fails the read (`merge.ErrUnboundPR`): an `error` event for the closers, exit 1 for status. No clock-skew allowance: a GitHub clock behind the local one by more than the time from launch to PR only leaves the row `reported`. The "head descends from `Base`" check suggested beside it is not added; T10.4's merge already requires it (predicate 3).
- **T10.3 ordering (chief-of-staff, 2026-10-09):** T10.3 (with the SelectPR launch binding) must merge before T10.4 or any PR that lets SelectPR drive a close or a merge.
- **T10.3 review fix, stale `reported` rows:** prune removes a `reported` queue row whose `state_at` is older than the cutoff when its own ledger row shows no live workspace (the same herdr check as the other states; unknown keeps it), so a reported row with no PR that nobody closed cannot live forever. This supersedes "A `reported` row is no longer removed by age" in "T10.3, the drain's old prune is gone".
- **T10.3 review fix N1, unreadable ledger files:** while `ListLedgers` lists any ledger file it cannot read, prune keeps every launched queue row whose ledger is not among the readable ones, with a reason naming the file(s).
- **T10.3 review fix N2, `closed_at`:** the close that first marks a ledger row `closed` records `closed_at` (a later close keeps the first time), and prune ages closed ledger rows by it. A closed row with no `closed_at` (closed before the field existed) goes only when its `started_at` is past the cutoff and its recorded worktree path no longer exists (`os.Lstat` says absent); a path that exists, cannot be checked or was never recorded keeps it with a reason. `closed_at` is a new ledger field without a version change, so an older binary refuses a ledger holding it, as with the P3 queue fields. This supersedes the `started_at` stand-in in "T10.3, ledger rows".
- **T10.3 review fix N3, usage lines per row:** `usage-daily.jsonl` gets one line per pruned priced row, `{day, name, launch_id, costUsd}` (day = UTC day of `state_at`), instead of per-day sums, so a line a failed prune wrote twice can be dropped: a reader sums the unique `(name, launch_id)` lines per day. The lines are still appended under the queue lock before the rows go. This supersedes "T10.3, usage rollup" and the Closers and prune design's "one line per day".
- **T10.3 review fix, close notes:** a drain close whose result carries a note records it: a `note` event and the closed row's `last_error`. When the ledger row could not be marked closed or removed (the brief named "marked closed"; "removed" fails the same way, leaving a ledger row that still says the worker is open, so both are handled alike), the queue row stays `reported` with the note as `last_error` and one `error` event, and the next read retries the close.
- **T10.3 review fix, daily prune:** an unreadable `drain-prune-day` is one `error` event and is rewritten with today before the prune runs. The daily prune is capped at 30 s (`surface prune` by hand keeps 2 minutes). herdr was probed per row (a readiness check and a listing each); prune now reads it once per run through `herdradapter.Adapter.Prober`, which judges every reference against one read under `Probe`'s rules.
- **T10.3 review fix, `surface prune` exit codes:** failing to open the queue, the drain files or the status cache is exit 1 (the state could not be read), as ADR-0015's table puts a verb that ran and failed; exit 2 is for usage errors only.
- **T10.4, cameronsjo/forgectl#1208 (folded in):** marker posts are ordered by their last edit (`lastEditedAt`, read on every review and comment): a post counts at `max(posted, edited)`, an edited post is never a strict pass, and an edited required-reviewer marker never satisfies the approver, so an edit can neither hide a finding behind an earlier pass nor turn a finding into a pass. A marker post whose response carries no `lastEditedAt` field (`EditUnread`), or an unparseable one, refuses outright. The loose scan reads bodies with invisible, zero-width, bidi and control characters and invalid UTF-8 removed, NFKC-normalized and HTML entities decoded (then removed and normalized again), and matches `(?i)cadence[\s\\_\-U+2010..U+2015 U+2212]*review\s*:` (U+2212 minus added beside the asked range). Inline review comments are read from each review in the PR query (`comments(first: 100)` per review, GraphQL, refusing on a remaining page or a missing connection) rather than the REST endpoint, whose pagination gh reports only in headers; any mention there by `marker_author_id` is an open finding. The PR fixtures' review nodes gained `"lastEditedAt": null` and an empty inline comment list by hand (a read-only live check showed both on #1203).
- **T10.4, two audit lines per merge:** the squash body must carry an audit hash, and the outcome (merge commit) is known only after the merge, so a merge writes an attempt line (`merging`) before `gh pr merge` runs, whose hash the body carries, and an outcome line after (`merged`, `merged-unconfirmed` or `merge-failed`) naming the attempt's hash in `attempt`. A refusal is one line. An attempt line that cannot be written stops the merge. A refusal with no PR records `pr` 0.
- **T10.4, merge details:** `-R` is host-qualified (`github.com/<owner>/<repo>`), and the name is GitHub's canonical one read in the same pass (the policy already refuses a rename). The subject also refuses an issue reference (`#N`, `GH-N`), since `fix: #12` or `fix: closes #12` in a commit on the default branch closes the issue. The pre-merge re-read is the full PR query, compared whole: head, base branch, draft, state, the repository id, and every review, conversation comment and inline review comment (an edit counts as a change). The post-merge check reads the PR's `mergeCommit` and the default branch's head, and requires `compare(mergeCommit...head)` to be `ahead` or `identical`, up to three reads 2 s apart; GitHub saying merged at another head is `merged-unconfirmed`. `gh` reporting a failure while GitHub says merged is recorded as merged with the gh error kept as a reason. `--dry-run` writes no audit line. Running `gh` from a temporary directory needed a new seam, `exec.DirRunner` (`RunWithEnvFilteredInDir`), implemented by `OSRunner`, `FakeRunner` and the githubauth pinned runner, which refuses when its base cannot choose a directory.
- **T10.4, the audit file:** read whole to chain and check, so capped at 16 MiB (`worker.MaxMergeAuditBytes`); an append past it, or onto a last line with no newline (a cut-short write), is refused and the merge with it, until the file is moved aside by hand. Refusals are limited to one per actor, PR, head and reason set over the whole file, not per time window.
- **T10.4, autopilot:** at most one attempt a tick and, per row, at most one every 5 minutes (`drain.AutopilotEvery`), since each attempt reads the PR in full; the queue and ledgers are read again after the closers. Candidates also need the ledger row still `launched`. Two new event kinds: `merged` and `merge-refused` (the latter also for a failed or unconfirmed merge). A merge clears the row's closer read time, so the closers read it at the next tick rather than up to 5 minutes later. An attempt runs inside the tick, capped at 3 minutes (`mergeTimeout`), unlike the daily prune's 30 s cap, since cutting a merge short mid-call leaves its outcome to be read back; a slow attempt can make `drain status` read `stale`.
- **T10.4, worker brief rule:** in every launch brief (`worker.Compose`, `ViaLaunch`) and intake's template. A typed follow-up brief does not repeat it: it fits the 600-character typed cap only without it, and goes to a worker that got the rule at launch. The rule names `cadence-review` without a colon, so an echo of it is not a marker mention.
- **T10.4 security review I1, the bless closure:** the built-in refusals add `internal/workflow/**` (the bless ceremony's injection guard and verifier wiring), `internal/digest/**` (the hash a blessing signs and verification recomputes) and `internal/skill/**` (the agent skill the binary embeds and `forgectl --skill --install` writes into an agent's skills directory). `TestBuiltinRefusalsCoverTheGateClosure` now also closes over `go list -deps` of `internal/bless`, `internal/workflow`, `internal/selfupdate` and `internal/skill`, beside the merge path. ADR-0011's decision 4 names the three. A `SKILL.md` at any depth is not refused by name: the skill the binary ships is the one an agent installs from forgectl.

## T10.4 mutation sweep

Each row forces one check to pass with a temporary edit (cp backup, the edit, `go test -count=1` of the package, cp restore, `cmp` against the backup), and names the tests that went red. Run 2026-10-09 on `feat/p4-merge`. The first run had two gaps, fixed before this table: row 7f's edit did not compile (redone as `removedTest && false`), and row 10 stayed green (no test covered `Facts.Unread`; `TestEvaluateUnreadFactsRefuse` added).

| Row | Check forced to pass | Red |
|---|---|---|
| 1a | mode: off | `TestEvaluateMode`, `TestLandRefusesAndAudits` |
| 1b | mode: the drain needs auto | `TestEvaluateMode`, `TestLandRefusesAndAudits` |
| 1c | mode: an unknown mode | `TestEvaluateMode` |
| 2a | machine: a mismatch (`config.ResolveMerge`) | `TestResolveMerge` |
| 2b | machine: unset | `TestResolveMerge` |
| 3a | repository: no recorded repository | `TestEvaluateRepository` |
| 3b | repository: id changed | `TestEvaluateRepository` |
| 3c | repository: renamed | `TestEvaluateRepository` |
| 3d | repository: built-in refused repositories | `TestEvaluateRepository` |
| 3e | repository: not on `repos` | `TestEvaluateRepository` |
| 4a | row: the whole predicate | `TestEvaluateRow`, `TestReaderReadBaseNotOnGitHub` |
| 4b | row: `launch_id` equals the queue's claim | `TestEvaluateRow` |
| 4c | row: `branch_from` new | `TestEvaluateRow` |
| 4d | row: ledger stage launched | `TestEvaluateRow` |
| 4e | row: base is an ancestor of the PR's base | `TestEvaluateRow` |
| 4f | row: head descends from the base | `TestEvaluateRow` |
| 5a | PR: the whole predicate | `TestEvaluatePR`, `TestLandDryRun`, `TestLandRefusesAndAudits`, `TestReaderRead1204` |
| 5b | PR: open | `TestEvaluatePR`, `TestReaderRead1204` |
| 5c | PR: not a draft | `TestEvaluatePR`, `TestLandDryRun`, `TestLandRefusesAndAudits` |
| 5d | PR: mergeable | `TestEvaluatePR` |
| 5e | PR: merge state | `TestEvaluatePR` |
| 5f | PR: author is the operator | `TestEvaluatePR` |
| 5g | PR: targets the default branch | `TestEvaluatePR` |
| 6a | checks: the whole predicate | `TestEvaluateChecks` |
| 6b | checks: runs tied to this PR | `TestEvaluateChecks` |
| 6c | checks: a failed run refuses | `TestEvaluateChecks` |
| 6d | checks: a running run refuses | `TestEvaluateChecks` |
| 6e | checks: the pinned workflow file | `TestEvaluateChecks` |
| 6f | checks: at least one run | `TestEvaluateChecks` |
| 6g | checks: the GitHub Actions app | `TestEvaluateChecks` |
| 7a | paths: the whole predicate | `TestEvaluatePaths`, `TestReaderRead1204` |
| 7b | paths: the per-repository allowlist | `TestEvaluatePaths` |
| 7c | paths: the built-in refusals | `TestBuiltinRefusalsBuildFiles`, `TestBuiltinRefusalsCoverTheGateClosure`, `TestBuiltinRefusalsTopLevel`, `TestEvaluatePaths` |
| 7d | paths: path shape (`CheckChangedPath`) | `TestEvaluatePaths` |
| 7e | paths: file modes | `TestEvaluatePaths` |
| 7f | paths: a removed test file | `TestEvaluatePaths` |
| 7g | paths: the file count | `TestEvaluatePaths` |
| 7h | paths: file status | `TestEvaluatePaths` |
| 8a | approver: the whole predicate | `TestEditedPassClearsNothing`, `TestEvaluateCadenceReview` |
| 8b | approver: the latest marker passes at the head | `TestEditedPassClearsNothing`, `TestEvaluateCadenceReview` |
| 8c | approver: counted reviews only | `TestEvaluateCadenceReview`, `TestEvaluateOpenFindingsFailClosed` |
| 9a | open findings: the whole predicate | `TestEditedPassClearsNothing`, `TestEvaluateCadenceReview`, `TestEvaluateOpenFindingsFailClosed`, `TestInlineReviewCommentsAreFindings`, `TestLooseMarkerScanIsWide`, `TestOpenFindingsOrderedByEdit` |
| 9b | open findings: only a later pass clears | the same six |
| 9c | open findings (#1208): a post counts at its edit | `TestEditedPassClearsNothing`, `TestOpenFindingsOrderedByEdit` |
| 9d | open findings (#1208): an edited post is never a pass | `TestEditedPassClearsNothing` |
| 9e | open findings (#1208): the normalized scan | `TestLooseMarkerScanIsWide` |
| 9f | open findings (#1208): inline review comments | `TestInlineReviewCommentsAreFindings` |
| 9g | open findings (#1208): an unread edit refuses | `TestEditedPassClearsNothing` |
| 9h | open findings: `marker_author_id` required | `TestEvaluateCadenceReview` |
| 10 | facts not read (`Facts.Unread`) | `TestEvaluateUnreadFactsRefuse` |
| 11a | merge path: the subject rule | `TestLandRefusesAndAudits` |
| 11b | merge path: the pre-merge re-read | `TestLandRefusesAndAudits` |
| 11c | merge path: the drain needs auto | `TestLandRefusesAndAudits` |
| 11d | merge path: `--match-head-commit` | `TestLandMerges` |
| 11e | merge path: no issue reference in the subject | `TestLandRefusesAndAudits`, `TestSubject` |
| 11f | merge path: the attempt line before the merge | `TestLandFailures` |
| 11g | autopilot: mode auto only (the kill switch) | `TestDrainAutopilotKillSwitch`, `TestDrainAutopilotOnlyInAuto` |
| 11h | autopilot: candidates only | `TestDrainAutopilotSkipsRowsItCannotVouchFor` |
| 11i | audit: refusals limited | `TestAuditRefusalsAreRateLimited`, `TestLandRefusesAndAudits` |
| 11j | audit: the chain check | `TestAuditChain` |

- **T10.3 independent review (chief-of-staff), closer race:** the closer acts on a ledger row read at the start of the tick, after the watch, the PR reads and pricing. It now checks the queue row is still the one read (`worker.SameRead`) before the close, and the drain's close re-reads the ledger row and requires `worker.SameRow` right before anything is closed or inspected, so a close or relaunch by hand in that window is left alone. A closed ledger row with `closed_at` also stays while the worktree it records exists, like a row without `closed_at`.

## Learnings
