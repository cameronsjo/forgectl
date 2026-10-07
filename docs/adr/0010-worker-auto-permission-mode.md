# 0010. Worker `auto` permission mode: per-machine opt-in behind a hardening floor

**Status: Accepted**

Date: 2026-10-05

## Context

The herdr coordinator plan (`docs/plans/2026-09-28-forgectl-herdr-coordinator.md`) starts claude workers at `acceptEdits`. The interim worker floor in `applyWorkerFloor` (`internal/launch/invocation.go:136-178`) allows only `plan`, `default`, and `acceptEdits` for claude, and `read-only` or `workspace-write` with `untrusted` or `on-request` approvals for codex. Anything else is refused.

An `acceptEdits` worker edits files without asking but stops at every shell command. A queue that dispatches workers unattended (the atelier drain) then spends most of its time in `needs-you`: every `go test`, `git commit`, and `gh pr create` waits for the operator. Claude Code's `auto` mode sends each tool call to a classifier instead of the operator. That removes the stall, and it replaces the operator's judgment on every shell command with the classifier's.

Today an unattended worker can reach far more than its worktree:

- It loads its branch's `.claude/settings.json` hooks, `.mcp.json` servers, and `CLAUDE.md` with no review (forgectl#1050).
- It loads the operator's user settings: every enabled plugin, every user hook (which runs outside any sandbox), and plugin MCP servers that can drive herdr panes without the herdr socket.
- It inherits the operator's keychain `gh` login, which can merge, push to any branch, and write to any repo the operator can.
- It can rewrite the shared `.git` refs of the repo it was branched from, so the next worker's "base" can be a commit it chose (not closed: workers in one repository are mutually trusting; see the 2026-10-06 amendment).
- Its tools run as the operator's macOS user. Claude Code's sandbox covers Bash and its child processes only; Read, Edit, Write, and WebFetch are decided by the permission layer.

`auto` turns each of these from "the operator approves each command" into "the classifier approves it". This ADR decides when a worker may run in `auto`, and what must be true first.

## Decision

1. **`auto` is a per-machine opt-in, off by default.** A worker may start in `auto` only when the machine's forgectl config sets `[surface.worker] allow_auto = true`. A missing file or key means `false`. The value is read at each launch, never cached by a running drain. Work machines keep it `false`.
2. **The opt-in is refused on an unsafe machine.** `allow_auto = true` is refused (the launch fails and `doctor` reports why) when the config file is a symlink or chezmoi-managed, or when a TCC probe shows the launch context holds Full Disk Access (a Claude session started under cmux does).
3. **`auto` is allowed only when the whole floor below holds at launch.** The launch checks each item and refuses naming the first one missing. It never falls back to `acceptEdits` silently: a refused launch is a `failed` row the operator sees.
4. **`dontAsk` and `bypassPermissions` stay refused for workers**, whatever the config says. The worker allowlist becomes `plan`, `default`, `acceptEdits`, `auto`, with `auto` gated by items 1 to 3. A mode Claude Code adds later stays refused until an amendment to this ADR names it. The mode ranking the stricter-of merge needs is forgectl#1043; its test includes the mutation "add `dontAsk` to the allowlist goes red".
5. **The hardening floor:**
   - **Only forgectl's settings load.** The worker starts with `--setting-sources` limited so user and project settings do not apply, a forgectl-supplied `--settings` file with plugins disabled, and `--strict-mcp-config` with a forgectl-supplied MCP config. A test proves a herdr-driving plugin tool is unreachable from a worker.
   - **Branch config is not loaded unreviewed** (forgectl#1050 fixed first). Project settings and hooks from the worktree are ignored, or checked byte-for-byte against the base commit.
   - **A new branch starts at GitHub's default head (forgectl#1061).** A correctness fix, not a control: it stops a stale local `HEAD` being the base, and does not stop an earlier worker choosing the next one's base (see the 2026-10-06 amendment).
   - **Bash sandbox, deny by default.** Reads under the home directory are denied except the worktree and named toolchain caches. Writes are limited to the worktree and the temp dir. Network goes through an allowlist (GitHub, the package registries the repo profile names). `allowUnsandboxedCommands = false` with no `excludedCommands`, so a failed command cannot retry outside the sandbox.
   - **Non-Bash tools denied the same paths.** `permissions.deny` rules cover Read, Edit, and Write on forgectl's config and state dirs (including the legacy `claunch` path and any `XDG_*` override), `~/.claude`, `~/.dotfiles`, `~/Library/LaunchAgents`, shell startup files, and `~/.local/bin`. WebFetch and WebSearch are denied.
   - **`useAutoModeDuringPlan = false`** (forgectl#1060) in the worker settings file for every worker mode, so a `plan` worker's shell commands do not go to the classifier.
   - **Worker GitHub identity.** Workers do not use the operator's GitHub identity. Each worker gets a short-lived installation token from a forgectl "worker" GitHub App, minted by the drain at launch, scoped to the one repo, with contents write (push branches) and pull-requests write (open PRs). The token is masked from the sandbox's environment and substituted only on requests to `api.github.com`. The keychain `gh` login is unreachable. `drain stop` and `surface close` revoke the token. Merging is closed to this identity by the ruleset (ADR-0011), not by deny rules.
   - **Deny rules as a second layer** for `gh pr merge`, `gh api` with a write method, and `git push` to the default branch. They catch mistakes; the ruleset is the control.
6. **A security review gates the switch.** A security review on an Opus-class model reads the file set below before `allow_auto` is set to `true` on any machine: `internal/launch/{invocation,profile,launch}.go` (`applyWorkerFloor`, `ResolveBinary`), `internal/surface/worker/{worktree,ledger}.go`, `internal/surface/herdradapter/`, `internal/exec/sensitive.go`, `internal/gitenv/gitenv.go`, `internal/config/{config,usage_base}.go`, the worker settings file, the sandbox profile, and the token-minting path.

## Consequences

- The remaining risk is stated plainly: **an `auto` worker runs as the operator's macOS user, minus what the sandbox and deny rules remove, with the classifier as its only per-call check.** Anything the deny-by-default read rule allows and anything the classifier approves is reachable by a worker following instructions planted in an issue body, a fetched branch, a dependency, or a review comment. Running workers as a separate macOS user would remove most of this; it is the next step if the floor proves leaky.
- A worker cannot merge anything, because its GitHub identity holds no merge path the ruleset accepts. (Deferred: see the second 2026-10-06 amendment; workers use the operator's identity, which can merge.)
- The drain's `needs-you` rate falls on machines that opt in and stays as it is elsewhere. The pane shows each worker's mode.
- T5 (worker profile) grows to carry the floor, and moves ahead of the queue and drain so no unattended `auto` run happens first.
- Turning `auto` off is one config edit (`allow_auto = false`), effective at the next launch. `drain stop` also revokes every live worker token. Running workers keep their permission mode until closed.
- `forgectl doctor` prints the effective `allow_auto` value and each floor item's status (ADR-0008 rule 4).

## Alternatives considered

- **Keep workers at `acceptEdits` permanently.** Safe, and the drain stalls at every shell command. Declined by Cameron in the atelier plan.
- **`dontAsk` or `bypassPermissions`.** No permission layer at all; the sandbox would be the only control. Declined.
- **A long-lived fine-grained PAT per repo.** Pull-requests write can merge through the API whatever the deny rules say, and stopping the drain leaves it live. Replaced by the worker App's short-lived tokens.
- **A machine-user account pushing to a fork.** Cannot merge, but is a long-lived credential and does not make GitHub enforce the merge policy. Declined in favor of the App pair.
- **Turn `auto` on everywhere once the floor ships.** Makes the work machine's posture depend on a home-machine decision. Declined: per-machine opt-in.

## Amendment 2026-10-06: workers in one repository are mutually trusting

Workers run in linked worktrees under `<repo>/.claude/worktrees/`, so they share the main checkout's `.git`. A worker has to write that `.git` to commit: its objects, its refs, and its worktree's admin directory. A worker that is sloppy or compromised can therefore change what the next worker in the same repository sees, and no check that reads the shared `.git` can detect it. The security review of forgectl#1126 named four such paths:

- **C1:** rewrite `remote.origin.url`, an `insteadOf` rule, or an `include.path`, so forgectl reads a different repository's default branch.
- **C2:** plant a forged object under the real head's hash. Git does not re-hash an object on a local read.
- **I1:** add config or hooks that a later git call in the main checkout runs.
- **I2:** plant `refs/heads/<branch>` or `refs/remotes/origin/<branch>`, so the next worker's branch "exists" at a chosen commit.

Cameron's decision: keep shared worktrees and accept this boundary. The threat this floor addresses is accidents and a sloppy worker, not an adversarial one. Per-worker clones were declined: a separate `.git` per worker would remove these paths, at a cost of disk space, clone time, and branches that reach the operator's checkout only through GitHub. forgectl#1061's base from GitHub stays as a correctness fix, because a checkout's `HEAD` is often stale. It is not a control against these paths.

Known-boundary rule: a review finding that needs a hostile earlier worker, or another attacker chain past this boundary, is recorded here, not designed against.

## Amendment 2026-10-06: workers run as the operator, unsandboxed

Cameron's decision: skip the worker GitHub App, the Bash sandbox, and the ruleset change. Workers keep running as the operator.

- **Identity.** A worker uses the operator's keychain `gh` login and SSH keys (`SSH_AUTH_SOCK` stays in the environment allowlist). It can push to any branch, merge, and write to any repository the operator can, and the SSH agent signs for every host the operator's keys reach, not only GitHub.
- **No OS sandbox.** A claude worker's tools run as the operator's macOS user with no Claude Code sandbox. A codex worker runs in Codex's own sandbox at its configured `--sandbox` level.
- **What bounds a worker.** Its brief, which forbids merging its own PR and touching the default branch, plus what forgectl already enforces: the settings and MCP isolation, `--safe-mode`, the environment allowlist, and the `SendMessage` and `RemoteTrigger` deny rules (`docs/herdr.md`, "What a claude worker loads"). The isolation applies to claude workers only; a codex worker gets just the environment allowlist (forgectl#1092). Nothing enforces "a worker cannot merge or push to `main`". The brief asks for it; the operator's identity allows it.
- **`auto` stays refused.** Decision 3 still holds: `auto` needs the whole floor, and the floor's sandbox and worker-identity items are not built. Workers stay capped at `acceptEdits` (`workerMaxPermissionMode`). Claude Code's built-in read-only commands and, under `acceptEdits`, file commands inside the worktree run without a prompt; commands that act outward, such as `gh pr merge` and `ssh`, prompt the operator (`git push` is pre-approved for `acceptEdits` workers since the 2026-10-07 amendment).

The floor items "Bash sandbox", "Non-Bash tools denied the same paths", "Worker GitHub identity", and "Deny rules as a second layer", and ADR-0011's restrict-updates ruleset, are deferred, not rejected. forgectl#1134 lists where each would change if workers need a sandbox later.

## Amendment 2026-10-07: a fixed allow list for `acceptEdits` workers

Approved with the atelier P2 plan (forgectl#1137): more autonomy by an allow list, not a broader mode. An `acceptEdits` worker's `--settings` pre-approves `go test`, `go build`, `make`, `git add`, `git commit`, `git push`, `gh pr create`, and `gh pr view`. A merge, `gh api`, or any other command typed on its own still prompts. Workers in other modes get no list. This loosens the default above ("every outward command prompts") on purpose, within the "accidents, not adversaries" boundary:

- `git push *` cannot be limited to the worker's branch; only a repository ruleset refuses a push to `main`. forgectl's and cadence's `estate-main` rulesets do. A repository without one does not.
- Four listed commands can run any other command with no prompt, so the list does not bound what a worker can do: `go test` and `make` run repository code the worker can write itself, `go test -exec` and `go build -toolexec` run a named program, and `git push --receive-pack` (or `--exec`) runs a command through `sh -c`. A worker can therefore merge or push with the operator's identity without a prompt; the prompts catch an accident, not a determined worker.

Widening the list needs its own security review. `auto` stays refused.
