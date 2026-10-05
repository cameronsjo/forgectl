# 0010. Worker `auto` permission mode: per-machine opt-in behind a hardening floor

**Status: Proposed**

Date: 2026-10-05

## Context

The herdr coordinator plan (`docs/plans/2026-09-28-forgectl-herdr-coordinator.md`) starts claude workers at `acceptEdits`. The interim worker floor in `applyWorkerFloor` (`internal/launch/invocation.go:136-178`) allows only `plan`, `default`, and `acceptEdits` for claude, and `read-only` or `workspace-write` with `untrusted` or `on-request` approvals for codex. Anything else is refused.

An `acceptEdits` worker edits files without asking but stops at every shell command. A queue that dispatches workers unattended (the foreman drain) then spends most of its time in `needs-you`: every `go test`, `git commit`, and `gh pr create` waits for the operator. Claude Code's `auto` mode runs shell commands without a prompt, which removes that stall and also removes the operator from every shell decision the worker makes.

Today an unattended worker can still reach more than its worktree:

- It loads its branch's `.claude/settings.json` hooks, `.mcp.json` servers, and `CLAUDE.md` with no review (forgectl#1050). A branch fetched from a remote can therefore run code in the worker before the first prompt.
- It inherits the operator's keychain `gh` login, which can merge, push to any branch, and write to any repo the operator can.
- It can reach the herdr socket every pane can reach, and so can type into the coordinator's pane or any other.
- Its shell runs as the operator's user account, with read and write access to the home directory.

`auto` widens each of these from "the operator approves each command" to "nothing approves it". This ADR decides when a worker may run in `auto`, and what must be true first.

## Decision

1. **`auto` is a per-machine opt-in, off by default.** A worker may start in `auto` only when the machine's forgectl config sets `[surface.worker] allow_auto = true`. A missing file or key means `false`. The value is read at each launch, never cached by a running drain. Work machines keep it `false`.
2. **`auto` is allowed only when the whole hardening floor below holds at launch.** The launch checks each item and refuses naming the first one missing. It never quietly falls back to `acceptEdits`: a refused launch is a `failed` row the operator sees.
3. **`dontAsk` and `bypassPermissions` stay refused for workers**, whatever the config says. The worker allowlist becomes `plan`, `default`, `acceptEdits`, `auto`, with `auto` gated by items 1 and 2. A mode Claude Code adds later stays refused until an ADR amendment names it. The mode ranking that the stricter-of merge needs is forgectl#1043, and its test includes the mutation "add `dontAsk` to the allowlist goes red".
4. **The hardening floor:**
   - **Branch config is not loaded unreviewed** (forgectl#1050 fixed first). The worker starts with `--strict-mcp-config` and a forgectl-supplied MCP config. Project settings and hooks from the worktree are ignored, or checked byte-for-byte against the same files on the pinned base ref. The worktree's base is pinned to the repo's default branch, not to a ref named by the queue row.
   - **Bash sandbox.** Shell writes are limited to the worktree and the temp dir. Network goes through an allowlist (GitHub, the package registries the repo profile names). The sandbox denies the herdr socket path and forgectl's config and state directories, so a worker cannot drive other panes or edit the drain's queue, ledger, or merge policy.
   - **Deny rules** in the worker settings file for `gh pr merge`, `gh api` with a write method, and `git push` to the default branch. These are a second layer under the token scope, not the main control.
   - **Scoped token.** The worker gets a fine-grained token scoped to the one repo (contents and pull requests write, no admin, no workflows), passed through the environment. The keychain `gh` login is not reachable from the sandbox. A runbook covers creating and rotating the token.
   - **Refused pane sends.** A worker's attempt to `herdr pane send` into the coordinator's pane is refused (the sandbox socket deny), and a test proves it.
5. **A security review gates the switch.** A security review on an Opus-class model reads the file set that implements the floor (the launch posture code, the worker settings file, the sandbox profile, and the token path) before `allow_auto` is set to `true` on any machine.

## Consequences

- The remaining risk is stated plainly: **an `auto` worker holds the operator's full user account minus what the sandbox removes.** Anything the sandbox profile misses (a readable secret in the home directory, a local socket not on the deny list, a tool that escapes the sandbox) is reachable by a worker following instructions planted in an issue body, a fetched branch, or a dependency.
- The drain's `needs-you` rate falls on machines that opt in, and stays as it is on machines that do not. The pane shows each worker's mode so the difference is visible.
- T5 (worker profile) grows: the floor's items are T5 work, and T5 moves ahead of the queue and drain so no unattended run happens at `auto` before the floor exists.
- Turning `auto` off is one config edit (`allow_auto = false`) and takes effect at the next launch. Running workers keep their mode until closed; `forgectl surface close` ends one.
- `forgectl doctor` reports the effective `allow_auto` value and each floor item's status, so the posture is printable (ADR-0008 rule 4).

## Alternatives considered

- **Keep workers at `acceptEdits` permanently.** Safe, and the drain stalls at every shell command. Declined by Cameron in the foreman plan.
- **`dontAsk` or `bypassPermissions`.** No permission layer at all; the sandbox would be the only control. Declined.
- **Turn `auto` on everywhere once the floor ships.** Makes the work machine's posture depend on a home-machine decision. Declined: per-machine opt-in.
