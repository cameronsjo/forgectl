# forgectl `pr local` — offline clean-room review (Phase 2)

## Context

Phase 1 (`pr <ref>`, forgectl#29/#53) shipped a clean-room review core: sandbox a PR
head into a throwaway worktree/clone, quarantine AI-instruction files, write a
deny-by-default agent allowlist, dispatch a review agent into a tmux window, and gate
any `gh pr review` post behind a human approval confirm. Issue #54 (independent
adversarial security review of that core) is open but its scope is #29's shipped
surface — folding it in here means the *new* surface Phase 2 adds gets the same
adversarial scrutiny before merge, not that #54 itself is closed by this PR.

Phase 2 (forgectl#30) is the offline sibling: review LOCAL committed changes with
**no GitHub round-trip at all**. The isolation model is narrower than Phase 1's, and
the enforcement mechanism is different in one important way worth stating up front:

**Phase 1's safety boundary is a human approval gate before a network post.**
**Phase 2 has no post to gate — there is no remote PR to post to.** Its safety
boundary is entirely the allowlist + filesystem isolation: the review agent can read
the worktree, run read-only git, and write to exactly one escape-hatch directory
outside the reviewed tree. Nothing else. `PostReview` is never called for a local
session — reusing it would risk a `gh pr review` shell-out against a session that has
no PR, which is exactly the kind of accidental-network-reach #54 asks about.

## Design decisions (beyond the phase blueprint's literal file list)

The blueprint (`docs/plans/2026-07-06-pr-review-suite.md` §Phase 2) names three
files: `internal/pr/local.go`, an `allowlist.go` addition, `internal/cli/pr_local.go`.
Implementing the escape-hatch dir correctly requires two small, necessary extensions
beyond that list — called out explicitly rather than silently added:

1. **`Session` gains two fields** (`internal/pr/session.go`): `Local bool` and
   `FindingsDir string`. Both are populated only in-process by `PrepareLocal` (never
   persisted to `Breadcrumb` — same pattern `HeadRef`/`HeadOid`/`HeadRepo` already
   follow: meaningful fresh, zero-valued on reload via `List`/`Attach`/`Teardown`).
   No breadcrumb schema change, so `manage.go`/`teardown.go` need no changes at all —
   teardown already only touches `Workspace`, and **must not** touch `FindingsDir`:
   the whole point of the escape hatch is that findings *outlive* the disposed
   workspace.
2. **`launchInline` (`internal/pr/launch.go`) branches on `sess.Local`**: appends
   `sess.FindingsDir` to `profile.AddDir` (the existing `--add-dir` mechanism —
   `internal/launch/launch.go` `BuilderArgs` already threads `Profile.AddDir` through
   to `claude`) and selects a local-specific review prompt instead of the PR one.
   Without `--add-dir`, the permission-scoped `Write(<dir>/**)` rule is moot — Claude
   Code won't expose a path outside the launch cwd at all.

Everything else — `ref.go`, `breadcrumb.go`, `manage.go`, `teardown.go`,
`sandbox`, `quarantine` — is reused completely unchanged.

## `internal/pr/local.go`

```go
type PrepareLocalOpts struct {
    Agent  string
    DryRun bool
}

func (c *Client) PrepareLocal(ctx context.Context, path string, opts PrepareLocalOpts) (Session, error)
```

No `Headless` field — there is no `PostReview` path to gate, so it would be a
no-op flag. `pr_local.go` therefore has no `--headless` flag either.

Steps, mirroring `Prepare`'s structure and failure-path teardown discipline:

1. Resolve `path` to an absolute path (default `.` from the CLI); `sandbox.RejectOptionLike("path", ...)`.
2. Resolve local HEAD **through the Runner** (never a direct shell-out — house
   pattern): `git -C <path> rev-parse --abbrev-ref HEAD` and
   `git -C <path> rev-parse HEAD`. Both are local-only, no-network calls — this is
   forgectl's own orchestration call, distinct from the sandboxed agent's denied
   surface (same distinction Phase 1 draws with its own `gh pr view` dry-run call).
3. Build a synthetic `Ref` identity via `localRef(headOid)`: `Owner: "local"`
   (reserved sentinel), `Repo: <7-char short oid>`, `Number: <derived from the oid's
   first 6 hex chars, always positive>` — all inside `Ref`'s existing validated
   charset, so `ref.String()` round-trips through `ParseRef` exactly like a real PR
   ref (required: `loadSession`/`validateBreadcrumb` call `ParseRef` on the stored
   string, and `parseNumber` rejects `Number <= 0`, so a fixed `0` sentinel would
   fail breadcrumb reload — this is why Number must be derived, not constant).
   Deriving it from the oid also keeps concurrent-session tmux window names
   (`pr-<N>`) distinct per commit under review, same as Phase 1's existing
   accepted same-PR-twice collision behavior.
4. On `DryRun`: return the `Session` (with `Local: true`, no `Workspace`) —
   creates nothing, matching `Prepare`'s dry-run contract.
5. `sandbox.RejectOptionLike("ref", headOid)`, then
   `sandbox.Sandbox(ctx, c.run, absPath, headOid, false)` — `alwaysClone=false` and
   a real local directory always takes `Sandbox`'s worktree path
   (`isLocalRepo` short-circuits true for an absolute path). Pinning to the resolved
   `headOid` (not `"HEAD"`) makes the worktree a deterministic snapshot of what was
   measured in step 2, and — deliberately — excludes uncommitted/staged changes:
   this reviews *committed* changes only, matching the issue's framing.
6. `quarantine.New(c.run).Hide(ctx, workspace, quarantine.SuffixQuarantined, quarantine.DefaultTargets, false)` — identical to Phase 1; teardown workspace on failure.
7. Create the escape-hatch dir: `os.MkdirTemp(filepath.Dir(workspace), "forgectl-findings-")`
   — a sibling of `workspace` under the OS temp root, freshly created (not a
   predictable/reusable name — avoids a symlink-pre-plant risk a deterministic path
   would invite). Teardown workspace + findings dir on any later failure.
8. `writeLocalAllowlist(workspace, findingsDir)` (new function in `allowlist.go`).
9. `writeBreadcrumb` — identical shape to Phase 1 (`Workspace`, `Ref: ref.String()`, `Agent`, `CreatedAt`); no `FindingsDir` field added to the breadcrumb (see Design decisions above).
10. Return the populated `Session{..., Local: true, FindingsDir: findingsDir}`.

## `internal/pr/allowlist.go` — `localProfile`

```go
var localAllowReadOnly = []string{
    "Read", "Grep", "Glob", "LS",
    "Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)",
    "Bash(git status:*)", "Bash(git blame:*)",
    "Bash(cat:*)", "Bash(rg:*)",
}
// No gh entries at all (Phase 1's allowReadOnly permits gh pr view/diff/checks —
// local mode permits none).

var localDenyNetwork = []string{
    "Bash(gh:*)",                    // every gh subcommand, not just posting ones
    "Bash(git push:*)", "Bash(git fetch:*)", "Bash(git pull:*)",
    "Bash(git clone:*)", "Bash(git remote:*)", "Bash(git submodule:*)",
    "Bash(git commit:*)",
    "Bash(curl:*)", "Bash(wget:*)", "Bash(ssh:*)", "Bash(scp:*)", "Bash(nc:*)",
    "Edit", "MultiEdit", "NotebookEdit", "WebFetch",
    // NOTE: no bare "Write" here — Deny wins over Allow (allowlist.go:49-50 comment),
    // so a blanket Write deny would clobber the scoped Write(findingsDir/**) grant
    // below. Write is handled entirely by scoping, not by omission-then-deny.
}

func localProfile(findingsDir string) permissions {
    allow := append(append([]string{}, localAllowReadOnly...),
        fmt.Sprintf("Write(%s/**)", findingsDir))
    return permissions{DefaultMode: "plan", Allow: allow, Deny: localDenyNetwork}
}

func writeLocalAllowlist(workspace, findingsDir string) (string, error) // mirrors writeAllowlist
```

**Amendment (post-implementation, via code review):** the shipped `localAllowReadOnly`
excludes `Bash(rg:*)` — the code sample above is the pre-review draft and is now stale.
ripgrep's `--pre <cmd>` flag executes an arbitrary program per searched file, a real
command-execution primitive; Phase 1's PR mode accepts that risk behind `PostReview`'s
human approval gate, but local mode has no such gate, so it never grants `rg` at all
(the built-in `Grep` tool, already granted, covers search without shelling out). See
`internal/pr/allowlist.go`'s `baseReadOnly`/`allowReadOnly`/`localAllowReadOnly` split
for the shipped shape.

`localDenyNetwork` is deliberately broader than Phase 1's `denyPosting` — it denies
every `gh` subcommand (not just the posting ones) and every network-reaching git verb
(`fetch`/`pull`/`clone`/`remote`/`submodule`), not just `push`. This is the literal
"no network CLI" requirement from the issue, applied as defense-in-depth on top of
`DefaultMode: "plan"` already blocking anything unlisted.

## `internal/cli/pr_local.go`

```go
func newPrLocalCmd(client *pr.Client, cfg config.Config) *cobra.Command
```
`Use: "local [path]"`, `Args: cobra.MaximumNArgs(1)` (default `.`), registered as a
subcommand of the existing `pr` command in `newPrCmd` (`internal/cli/pr.go`)
alongside `list`/`attach`/`open`/`teardown`/`cleanup`/`keys` — reuses the same
`*pr.Client` built once there. Flags: `--agent` (reuses `resolveAgent`), `--dry-run`.
No `--headless` (see Design decisions).

`RunE`: `client.PrepareLocal(ctx, path, pr.PrepareLocalOpts{...})` → on dry-run,
print a one-line plan (`plan: worktree <path>@<headRef> → quarantine → launch
agent(local profile: read-only git, no network CLI, findings/ writable)`) and
return — `Launch` is never called on dry-run, matching Phase 1. Otherwise
`client.Launch(ctx, sess, cfg)` then print `workspace:`, `findings:`, `breadcrumb:`
lines so the user knows where the review output will land.

## Tests

- `internal/pr/local_test.go` (new, `// Test plan for local.go` header, following
  the house `FakeRunner`/`t.TempDir()` pattern):
  - `TestPrepareLocal_DryRunCreatesNothing` — only the two local `git rev-parse`
    calls happen; no sandbox, no allowlist, no breadcrumb.
  - `TestPrepareLocal_UsesWorktreeNotClone` — asserts `git worktree add`, never
    `git clone`, appears in `fake.Calls`.
  - `TestPrepareLocal_PinsToResolvedOid` — the worktree `add` call's ref arg is the
    resolved HEAD oid, not the literal string `"HEAD"`.
  - `TestPrepareLocal_BreadcrumbRoundTripsThroughParseRef` — `ParseRef(ref.String())`
    succeeds for `localRef(...)`'s output (the concrete failure mode a `Number<=0`
    regression would hit).
  - `TestLocalProfile_DeniesAllNetworkCLI` — `Bash(gh:*)` present in Deny; no gh
    entries anywhere in Allow.
  - `TestLocalProfile_FindingsDirIsOnlyWritablePath` — Allow contains exactly one
    `Write(...)` entry scoped to the passed `findingsDir`; Deny contains no bare
    `"Write"` (the precedence trap called out above).
  - `TestPrepareLocal_FindingsDirOutsideWorkspace` — `sandbox.WithinWorkspace(workspace, findingsDir)` is false (it's a sibling, not nested).
- `internal/pr/launch_test.go` (extend): `TestLaunchInline_LocalSessionAddsFindingsDirAndPrompt` — a `Session{Local: true, FindingsDir: "..."}` produces a `tmux new-window` argv containing `--add-dir <findingsDir>`; a non-local session's argv does not.

## Verification

```
go build ./...
go vet ./...
go test ./...
gofmt -l .
```
all zero/clean, then:
```
go build -o /tmp/forgectl .
/tmp/forgectl pr local . --dry-run
```
Expected: a plan line showing network CLI denied and exactly one writable
escape-hatch dir, creating nothing (no workspace, window, or breadcrumb — verify
with `ls $(forgectl config 2>/dev/null | ...)` or simply that the command's own
`(dry-run: ...)` line prints and no `/tmp/forgectl-workflow-*`/`forgectl-findings-*`
dirs appear afterward).

## Process notes

- Branch: already on `feat/pr-local` in this worktree (`/tmp/forgectl-pr-local`) —
  adopt it, do not create a new worktree.
- Before opening the PR: run `cadence-forge:polish` against the worktree diff, then
  dispatch `cadence-forge:security-reviewer` specifically over `local.go` and the new
  `localProfile()`/`launchInline` branch (folding in #54's spirit for this new
  surface), and address every finding.
- PR body: `cadence:redaction` pass (forgectl is public); `-R cameronsjo/forgectl` on
  all `gh` writes; `Refs cameronsjo/forgectl#30` (never `Closes`).
