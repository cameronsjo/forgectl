# 0003 — Clean-room sandbox is a git worktree (local) / clone (remote) into a temp dir

- **Status:** Accepted (2026-07-01)
- **Context:** #9 workflow DSL design spike. Related: 0002; the `strip` step is a security control (#10, #20).

## Context

The clean-room-review workflow needs an isolated copy of the target repo to review — one it can
`strip` (delete agent-instruction files from) and `teardown` without touching the user's real
checkout. The sandbox is the ground the security control (`strip`) operates on, so its isolation
matters.

## Decision

The `worktree` step creates the sandbox as a **`git worktree` into `os.MkdirTemp`** when the repo
is already local, and falls back to **`git clone`** into a temp dir for a remote `owner/repo`.
Both go through `exec.Runner`, mirroring the existing `internal/projects` clone. `teardown` removes
the temp dir (and, for a worktree, prunes it).

The `strip` runner resolves globs **only inside `${workspace}`** and rejects `..`/absolute paths —
a path-escape guard, since `strip` deletes files.

## Alternatives considered

- **Worktree-into-temp / clone (chosen).** Pros: a worktree is cheap for a local repo (shares the
  object store, no re-download) and its working directory is fully separate, so `strip`/`teardown`
  never touch the main checkout; clone gives full isolation for remote repos. Cons: introduces the
  first git-worktree handling in forgectl (small new code).
- **Always full `git clone`.** Pros: uniform, maximally isolated. Cons: re-downloads a local repo
  unnecessarily; slower for the common local case.
- **Copy the working tree (`cp -r`).** Rejected: drags in `.git` state and uncommitted cruft, and
  gives no clean `ref` selection.
- **Review in place, skip the sandbox.** Rejected: defeats the entire clean-room purpose — `strip`
  would mutate the user's real checkout, and there'd be no prompt-injection isolation.

## Consequences

- forgectl gains a small git-worktree helper (reusable later by #4 `clean` and #29 `pr`).
- `teardown` must be idempotent and safe to run after a partial failure (reentrancy).
- The `strip` path-escape guard is a correctness-and-security requirement, not optional polish.
