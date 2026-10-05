# 0011. Worker PR merge policy: per-machine, off by default, approvers a worker cannot impersonate

**Status: Proposed**

Date: 2026-10-05

## Context

The herdr coordinator plan refused auto-merge ("No standing daemon, PR poller or auto-merge. Merge stays manual."). The foreman plan (`cadence-ecosystem` `docs/plans/2026-10-05-foreman-a-herdr-work-queue-cockpit-for-claude-code.md`) reverses that for one case: a detached drain dispatches queued tasks, and a worker's PR may merge without the operator when a written policy allows it.

The obvious approval signal is wrong. Cadence reviews post as a PR comment with a `cadence-review` marker (`head=<sha> crit=N imp=N`), and they post as `cameronsjo`, the same GitHub identity a worker's `gh` uses. GitHub also refuses `--approve` on a self-authored PR, so every PR in this estate carries no blocking review state. A worker that can post a comment can therefore forge the marker, and a gate keyed on it lets the worker approve itself.

A merge gate also races. A check read at one head SHA says nothing about a commit pushed a second later, and a policy file a worker can edit is not a policy.

## Decision

1. **Policy lives in forgectl's per-machine config.** A missing file, a missing `[surface.merge]` table, or any key that fails to parse means `mode = "off"`. Fail closed on every error.

   ```toml
   [surface.merge]
   mode = "auto"                 # off | manual | auto
   approvers = ["coderabbit"]    # plug-in list; v1 implements only coderabbit
   method = "squash"
   repos = ["cameronsjo/forgectl", "cameronsjo/cadence"]   # explicit allowlist
   [surface.merge.required_checks]
   "cameronsjo/forgectl" = ["build-test", "lint"]
   ```

   - `off`: nothing merges through forgectl.
   - `manual`: `forgectl surface merge` and the pane's merge button work when the policy passes; the drain never merges on its own.
   - `auto`: the drain's autopilot step also merges when the policy passes.
2. **A merge passes only when every predicate holds, evaluated fresh for that call:**
   - The repo is on `repos`, and `required_checks` names at least one check for it. A repo with none named is refused.
   - Every named required check has concluded `success` at the PR's current head SHA.
   - At least one configured approver has passed that same head SHA (see 3).
   - The PR is not a draft, is `MERGEABLE`, and targets the repo's default branch.
   - The diff touches none of: `.github/`, `.claude/`, `.mcp.json`, `CODEOWNERS`, any hook directory (`.githooks/`, `.husky/`, `hooks/`), or a lockfile. These change what runs or who reviews, so they always need a human.
   - The PR was opened by a worker the drain owns (a ledger row names it).
3. **Approvers are a plug-in list, matched by identity a worker cannot use.** v1 implements one: `coderabbit`, matched by the bot account's numeric GitHub `user.id`, never by login or display name. Which CodeRabbit signal counts (a check or status at the head, or a bot review whose `commit_id` equals the head with zero actionable comments) is fixed by captured fixtures before code. Empty-body reviews, chat commands, and comments by any other id never count. `cadence-review` is deferred until it posts under an identity workers cannot hold.
4. **The merge is bound to the verified SHA.** forgectl runs `gh pr merge --<method> --match-head-commit <sha>`, where `<sha>` is the head every predicate was read at. A push that races the merge makes GitHub refuse it. Admin bypass is never used.
5. **Every merge and every refusal is audited.** forgectl appends one JSON line to `merge-audit.jsonl` in its state dir: time, actor (`drain`, `cli`, or `pane`), PR, verified SHA, a hash of the policy in force, each required check and its state, the approver evidence (approver, id, SHA, link), and the result with reasons. The file is append-only and never pruned by forgectl; an operator rotates it by hand (runbook). `forgectl surface audit --pr <owner/repo#N>` prints a PR's rows.
6. **Kill switch.** `forgectl surface drain stop` stops the drain. `mode` is re-read on every drain tick and every `surface merge` call, so setting `mode = "off"` stops the next merge without a restart.
7. **The config is guarded.** Worker sandboxes deny forgectl's config dir (ADR-0010). `forgectl doctor` warns when `mode != "off"` and the config file is a symlink or managed by chezmoi, because then a change in a dotfiles repo can turn merging on. Work machines stay `off`.
8. **A security review gates the switch.** A security review on an Opus-class model reads the merge-gate and intake file set, and a mutation sweep forces each predicate true and confirms a test goes red, before any machine sets `mode = "auto"`.

## Consequences

- A worker cannot merge its own PR by posting text: the only approver v1 trusts is a bot account by numeric id, and the worker's token cannot post as it.
- The gate depends on CodeRabbit staying installed and reviewing. If it stops, PRs wait for the operator; nothing merges on a missing signal.
- A repo with no named required checks never auto-merges. Adding a repo is two config edits (`repos` and `required_checks`).
- Every merge decision can be explained after the fact from `merge-audit.jsonl` alone.
- The coordinator plan's "no daemon, poller, or auto-merge" line is reversed by amendment, which names which original concern each part of this ADR answers.

## Alternatives considered

- **Merge on the `cadence-review` marker.** Forgeable by any worker with the operator's identity. Declined.
- **GitHub branch protection plus native auto-merge only.** Keeps the merge decision on GitHub but cannot express the refused-paths rule or record the audit row, and a self-authored PR has no approving review to require. Kept as a complement: required checks in branch protection still apply under `--match-head-commit`.
- **A policy file in each repo.** A worker can edit its own repo. Declined: the policy lives outside every worktree.
