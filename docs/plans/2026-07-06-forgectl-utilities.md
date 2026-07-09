# forgectl utilities batch — implementation plan

> Plan PR (draft). One `Refs cameronsjo/forgectl#N` line per module below — do **not** use `Closes` on any of these (batch ships incrementally; the umbrella #1 stays open). Implementer note: you have zero prior context — read this document top to bottom, then the cited source files, before writing a line. Planned 2026-07-06 against `forgectl` @ `main` (`go 1.25.0`, verified `go build ./...` green at plan time).

## Context

`forgectl` is a public Go 1.25 Cobra CLI with a strict two-layer architecture. This plan covers **six independent utility command modules** — issues #4, #5, #23, #24, #26, #28 — each a new domain under `internal/<domain>/` plus a thin verb under `internal/cli/`. They share no domain logic; the only files touched in common are `internal/cli/root.go` (one `AddCommand` line each), `internal/forgive/forgive.go` (alias maps), and `internal/config/config.go` (additive TOML sections for the modules that need config). Issue **#28** is the exception: it re-scopes the existing `internal/projects/` module rather than adding a new one.

### House pattern (verified against source — replicate exactly)

- **Ops layer** `internal/<domain>/` — no Cobra import. Constructor `New(run exec.Runner, opts ...Option) *Client`, functional-options pattern. Reference: `internal/tmux/tmux.go:42-56` (`New` + `Option` + `WithBins`/`WithInsideTmux`), `internal/projects/projects.go:24-34`, `internal/workflow/exec.go:81-88` (`NewExecutor`).
- **Every shell-out goes through the `exec.Runner` seam** — `internal/exec/exec.go:22-25` defines `Run(ctx, name, args...) (string, error)` and `RunInteractive(...)`. Never call `os/exec` directly in an ops package.
- **Thin verb layer** `internal/cli/<domain>.go` — parses flags, calls ops, holds no domain logic (`internal/cli/root.go:1-4` states this). Register the parent command in `newRoot` (`internal/cli/root.go:33-37`). Sub-verbs follow `internal/cli/projects.go:13-26` (parent `AddCommand`s children, `applyProjectAliases`).
- **Config** — TOML sections in `internal/config/config.go`. Each section is a struct with a `toml:"..."` tag on `Config` (lines 32-38) and an `IsZero()` guard (e.g. `LaunchConfig.IsZero` line 73-75, `WorkflowConfig.IsZero` line 86-88). A missing file is never an error (`Load` line 103-114). Config base dir is `os.UserConfigDir()/forgectl` (`configDir` line 226-232) — reuse it for any state file (e.g. docker last-tag cache); do **not** invent a new path root.
- **Tests** — table-driven, asserting on `exec.FakeRunner.Calls` (`internal/exec/fake.go:11-15,36-44`; `.Last()` line 55-62). The pure-logic test files open with a **`// Test plan for <file>.go`** checklist comment (`internal/cli/projects_list_test.go:3`, `internal/config/config_test.go:3`). Honest note: this header is a convention on the *pure-logic* test files, **not** every test file (`tmux/sessions_ops_test.go`, `workflow/exec_test.go` open bare). Follow it for any new pure-logic helper (parsers, classifiers, tag-derivers).
- **slog narrative logging** — `slog.Debug/Info/Warn/Error` with "Preparing to X." / "Successfully X." / "Failed to X." phrasing (see `internal/exec/exec.go:33,42,48`, `internal/projects/projects.go:143,204,213,243`). Quiet by default: `log_level` defaults to `"off"` → discard handler (`config.go:139-142`).
- **Alias layer** `internal/forgive/forgive.go` — canonical-verb→aliases maps (`ProjectAliases` line 23-26, `TmuxAliases` line 47-56). Add a `<Domain>Aliases` map if the module wants short forms; `internal/forgive` is pure stdlib (no Cobra).
- **Security idioms** (all in `internal/workflow/exec.go`): `rejectOptionLike(field, value)` (line 336-341) rejects a leading `-` before a value becomes a git positional; the `git … -- <positional>` separator (lines 220, 229, 231) ends option parsing; `withinWorkspace(workspace, target)` (line 317-331) re-checks a resolved path stays in-sandbox after `EvalSymlinks`. Any module shelling `git`/`docker`/`gh` with user- or repo-derived positionals **MUST** reuse the `--` separator and reject option-like values.

### Environment constraints

- **No test CI.** The repo has only `release.yml` (goreleaser). Verification is **local** — every module's Definition of Done includes a pasted `go build ./... && go vet ./... && go test ./...` transcript in the PR body. Use annotated tags only if releasing (`git tag -a`; lightweight tags fail goreleaser silently).
- **Cross-platform honesty.** #26 (clipboard/history) is macOS-specific by nature; guard or document the platform boundary rather than silently no-op on Linux.

## Issues covered

| # | Module | Lands in | Size | Disposition |
|---|---|---|---|---|
| #24 | `pip` — comment-preserving pip.conf editor | `internal/pip/`, `internal/cli/pip.go` | **S** | Ready. Fully self-contained; pure INI-lite parser + `remove`/`restore`. Best first / warm-up module. |
| #28 | `proj clone` — canonical `{host}/{org}/{repo}` layout | `internal/projects/` (re-scope) + `internal/cli/projects_clone.go` | **S–M** | Ready. Extends the existing `projects` module; touches shared `projects.go`. |
| #23 | `docker` — build/run/shell with git-derived tags | `internal/docker/`, `internal/cli/docker.go` | **M** | Ready. Self-contained; new last-tag cache state file. |
| #5 | `branch` — prune stale/orphaned branches | `internal/branch/`, `internal/cli/branch.go` | **M** | Ready. Encodes documented squash-merge/PR/worktree gotchas. |
| #4 | `clean` — reclaim dev cruft across projects | `internal/clean/`, `internal/cli/clean.go` | **M (upper)** | Ready, but **recommend splitting** — ship dep/build-dir reclaim first; defer package-manager-cache + docker-prune to a follow-on. |
| #26 | `y` — clipboard + shell-history utilities (macOS) | `internal/clip/`, `internal/cli/y.go` (+ shell shim) | **M, partly blocked** | **Partial-defer recommended.** Pure-Go clipboard half is ready; the shell-history half depends on an eval-wrapper *shim precedent that does not exist in the repo yet* (the issue's referenced `proxy` command is absent — verified). Ship clipboard now; defer history behind a design decision. |

Sizing legend: **S** ≈ one ops file + one verb + tests (½–1 session). **M** ≈ multi-file ops with real branching, external-tool orchestration, and a config/state surface (1–2 sessions).

---

## Module #24 — `forgectl pip` (INI-lite pip.conf editor)

**Refs cameronsjo/forgectl#24**

### Scope

Edit `pip.conf` with a **comment- and whitespace-preserving INI-lite parser** so edits round-trip byte-clean. `remove` / `restore` toggle index-url entries **reversibly with no backup sidecar** (the parser retains removed lines as commented-out markers it can un-comment on restore, or an equivalent reversible encoding — designer's call, documented in the ops doc-comment).

### Exact files

- `internal/pip/pip.go` — `Client` with `New(run exec.Runner, opts ...Option) *Client`. Even though pip.conf editing is file-I/O not shell-out, keep the constructor signature for house consistency; the Runner may be unused (or used to resolve `pip config debug` locations). If genuinely zero shell-outs, still expose `New(...)` returning a `*Client` over a configurable `pip.conf` path (inject the path via an `Option` for testability, mirroring `WithBins`).
- `internal/pip/inifile.go` — the pure INI-lite parser: parse → in-memory model preserving comments/blank lines/ordering → serialize. This is the table-testable core.
- `internal/pip/inifile_test.go` — **opens with `// Test plan for inifile.go`** checklist. Round-trip property: `serialize(parse(x)) == x` for a corpus of real pip.conf shapes (comments, `[global]`/`[install]` sections, `index-url`/`extra-index-url`, trailing whitespace, no-final-newline).
- `internal/pip/pip_test.go` — `remove`/`restore` reversibility: `restore(remove(x)) == x`.
- `internal/cli/pip.go` — `newPipCmd()` parent with `remove`/`restore` (and likely `show`/`path`) subcommands; register at `internal/cli/root.go:37`.
- `internal/forgive/forgive.go` — optional `PipAliases` map if short forms wanted.

### pip.conf location resolution

pip reads config from a documented precedence (`PIP_CONFIG_FILE` env, then OS-specific site/user/global paths). Resolve via an injectable path (default: user-level `pip.conf`), and prefer shelling `pip config list -v` / `pip config debug` through the Runner to discover the *effective* file rather than hardcoding — but keep the parser itself pure and path-agnostic.

### Verification transcript (paste in PR)

```
go build ./... && go vet ./... && go test ./internal/pip/... ./internal/cli/... && go test ./...
```

Plus a manual round-trip demo: run `remove` then `restore` on a fixture pip.conf, `diff` original vs restored → empty.

### Suggested implementer model

**Sonnet.** Bounded, spec-complete, pure-logic-heavy. Good warm-up module.

---

## Module #28 — `forgectl proj clone` (canonical host/org/repo layout)

**Refs cameronsjo/forgectl#28**

### Scope

Add `forgectl proj clone` cloning into a **canonical lowercased `~/Projects/{host}/{org}/{repo}`** hierarchy, annotating which candidates are already on disk. Keep `proj pick`/`list` working over **both** the canonical tree and the legacy flat layout during transition. This is a **re-scope of the existing `internal/projects/` module**, not a greenfield command (verified: `Clone` currently lands flat at `filepath.Join(c.Dir, r.Name)` — `internal/projects/projects.go:217`).

### Ground truth to build on

- `Repo` identity is `(Host, Owner, Name)` — `internal/projects/project.go:26-35`; `Repo.Key()` already lowercases `host/owner/name` (`project.go:112-116`). The canonical path is the filesystem mirror of `Key()`.
- `parseRemoteURL` (`internal/projects/project.go:129`) derives host/owner/name from an origin URL — reuse for classifying on-disk clones under the canonical tree.
- `Clone` (`projects.go:212-245`) has a collision guard (`originMatches`, line 259-266) that already reasons about same-name repos from different hosts — the canonical layout *dissolves* that collision (github/x and gitea/x land in distinct dirs), so preserve/relax the guard accordingly, don't delete its intent.
- `validRepoName` (`projects.go:251-254`) rejects traversal — extend the same guard to `Host` and `Owner` segments before joining them into a path.

### Exact files

- `internal/projects/projects.go` — **shared file, edit carefully.** Change the clone destination derivation: add a canonical-path helper (`canonicalDest(dir, host, owner, name) string`) and route `Clone` through it. Guard `host`/`owner` with the same traversal check as `validRepoName`. Keep a flag/option to fall back to flat for transition.
- `internal/projects/project.go` — `localRepos` discovery (`projects.go:102-130`) walks *immediate* subdirectories (`Discover`, line 40-68); a canonical tree is **3 levels deep** (`host/owner/repo`). Extend discovery to walk the canonical depth **and** the legacy flat depth. This is the substantive part of the work.
- `internal/cli/projects_clone.go` — `newProjectsCloneCmd(client)`, registered under the projects parent (`internal/cli/projects.go:22-23` pattern). Annotate already-on-disk candidates (reuse `openOrClone` shape, `projects_pick.go:66-77`).
- `internal/cli/projects.go` — add `cmd.AddCommand(newProjectsCloneCmd(client))`.
- `internal/forgive/forgive.go` — add `"clone"` to `ProjectAliases` (line 23-26) if a short form is wanted.
- Tests: `internal/projects/projects_test.go` + a new `internal/cli/projects_clone_test.go` (assert `FakeRunner.Calls` show the clone lands at the canonical dest; assert discovery finds both flat and canonical clones; assert host/owner traversal rejection).

### Sequencing note

`Discover` feeds `Inventory` → `proj pick`/`list`. Changing walk depth affects **all three verbs** — verify `pick`/`list` still pass after the discovery change. This is why it's S–M not S.

### Verification transcript

```
go build ./... && go vet ./... && go test ./internal/projects/... ./internal/cli/... && go test ./...
```

Manual: clone one repo, confirm it lands at `~/Projects/github/cameronsjo/<repo>`; run `proj pick` and confirm it's discovered.

### Suggested implementer model

**Sonnet**, but with care — the shared `projects.go`/`project.go` edits and the multi-verb discovery blast radius make this the trickiest "S". An implementer with context on the projects module is ideal.

---

## Module #23 — `forgectl docker` (build/run/shell, git-derived tags)

**Refs cameronsjo/forgectl#23**

### Scope

- `build` — auto-derive tag `{repo}:{branch-slug}-{shortsha}` plus a `:dev` alias, inject OCI labels + a `--platform` flag, **cache the last-built tag**.
- `run` / `shell` — reuse the cached tag when `--tag` is omitted.
- Never edit the Dockerfile — labels attach at the CLI (`docker build --label ...`).

### Exact files

- `internal/docker/docker.go` — `Client` + `New(run exec.Runner, opts ...Option)`. Shell `git` (for repo/branch/sha) and `docker` through the Runner.
- `internal/docker/tag.go` — pure tag/slug derivation (`deriveTag(repo, branch, shortsha) string`, `slugifyBranch`). Table-testable core.
- `internal/docker/cache.go` — last-tag persistence. **Use the config dir** (`config.configDir()` → `os.UserConfigDir()/forgectl`, `config.go:226-232`); write a small state file (e.g. `forgectl/docker-lasttag`), not a new path root. Best-effort read/write (never fatal).
- `internal/docker/*_test.go` — `tag_test.go` opens with `// Test plan for tag.go`; assert `FakeRunner.Calls` show `docker build` receives the derived tag, both tags, the labels, and the `--platform` flag in the right argv positions.
- `internal/cli/docker.go` — `newDockerCmd()` with `build`/`run`/`shell`; register at `root.go:37`.
- `internal/forgive/forgive.go` — optional `DockerAliases`.
- `internal/config/config.go` — **optional** `[docker]` section (default platform, label template) with an `IsZero()` guard, if defaults are wanted. Additive; see collision note.

### Security

Git-derived branch names reach `docker` argv. Sanitize the branch slug (already required for a valid docker tag) and treat the repo path as a positional — apply `rejectOptionLike` semantics (`workflow/exec.go:336`) to any user-supplied `--tag`/path/context.

### Verification transcript

```
go build ./... && go vet ./... && go test ./internal/docker/... ./internal/cli/... && go test ./...
```

Manual (requires a local docker daemon — note if unavailable and rely on FakeRunner assertions): `forgectl docker build` in a git repo, confirm the tag string; `forgectl docker run` with no `--tag` reuses the cache.

### Suggested implementer model

**Sonnet.** Self-contained, clear spec; the only judgment call is the state-file location (answered above).

---

## Module #5 — `forgectl branch` (prune stale/orphaned branches)

**Refs cameronsjo/forgectl#5**

### Scope

Enumerate local + remote branches, classify (merged-to-main, `gone` upstream, worktree-attached, has-open-PR), **dry-run by default** with a grouped report (safe-to-delete / blocked / needs-attention), then `--apply` gated by `huh.NewConfirm`.

### Must-encode gotchas (these are the reason this command exists — do not simplify away)

1. **`git branch --merged main` misses squash-merged branches.** Detect merged-ness via the PR's merged state: `gh pr list --state merged --json headRefName` (or per-branch `gh pr view`). Do **not** trust `--merged` alone.
2. **Deleting a branch an open PR is based on CLOSES that PR.** Before any delete, check for an open PR with that head; skip or warn. For stacks, retarget dependents first.
3. **Worktree order:** `git worktree remove` MUST precede `git branch -d` (else "used by worktree at …").
4. **Verify remote deletion with the SINGULAR endpoint:** `gh api repos/O/R/git/ref/heads/<branch>` → clean 404. The plural `refs` returns `[]`/200 and masks a failed delete.
5. Local `git branch -D` is acceptable once the PR is confirmed merged server-side.

### Exact files

- `internal/branch/branch.go` — `Client` + `New(run exec.Runner, opts ...Option)`. All `git`/`gh` through the Runner.
- `internal/branch/classify.go` — **pure** classification given branch metadata → group. Table-testable; the heart of the tests. `classify_test.go` opens with `// Test plan for classify.go`.
- `internal/branch/branch_test.go` — assert `FakeRunner.Calls`: the singular `git/ref/heads/<b>` verification endpoint is called (not plural); `git worktree remove` is issued before `git branch -d`; a branch with an open PR is never in the delete argv.
- `internal/cli/branch.go` — `newBranchCmd()` with flags `--local`, `--remote`, `--remote-name origin`, `--include-gone`, `--apply`; register at `root.go:37`. The issue suggests possibly grouping under a `git` parent if more git verbs land — **do not** pre-build that parent; ship `forgectl branch` flat, note the future grouping in a doc-comment.
- `internal/forgive/forgive.go` — optional `BranchAliases`.

### Security

Branch names reach `git`/`gh` as positionals — use `--` separators (`workflow/exec.go:220,229`) and `rejectOptionLike` (line 336) so a branch literally named `--foo` can't inject a flag.

### Verification transcript

```
go build ./... && go vet ./... && go test ./internal/branch/... ./internal/cli/... && go test ./...
```

Manual dry-run in a repo with a squash-merged branch: confirm it appears in "safe-to-delete" (proving gotcha #1) and that a branch backing an open PR appears in "blocked".

### Suggested implementer model

**Sonnet**, but the gotchas are subtle — the implementer MUST read the issue body's gotcha list verbatim. If the implementer session lacks that context, prefer **Opus** or pass the gotchas inline in the task prompt.

---

## Module #4 — `forgectl clean` (reclaim dev cruft) — **recommend split**

**Refs cameronsjo/forgectl#4**

### Recommended scoping decision

This is the **upper end of M, bordering L**. Recommend shipping in **two PRs**:

- **PR-1 (this batch):** dep/build-dir reclaim only — scan, size, dry-run report, `--apply` with confirm, dirty-tree skip, `--type`/`--older-than`/`--root` filters. This is the trivially-regenerable, safe core.
- **PR-2 (follow-on, defer):** opt-in package-manager caches (`npm cache clean`, `pnpm store prune`, `pip cache purge`, `go clean -cache/-modcache`, brew) and the docker-prune wrapper. These are opt-in by the issue's own design; each is an independent shell-out surface.

Rationale: the scan/size/delete/safety core is self-contained and testable; bolting five package-manager integrations + docker prune onto the same PR triples the review surface for functionality the issue already marks optional.

### Scope (PR-1)

- Scan a root (default `~/Projects`, `--root` configurable) for reclaimable dep/build dirs: `node_modules`, `.venv`/`venv`, `__pycache__`, `target`, `dist`, `.next`, `build`, `vendor`, `.svelte-kit`.
- **Dry-run by default:** per-target + total reclaimable size, delete nothing. `--apply` (or `huh.NewConfirm`) required before deleting; report actual reclaimed bytes.
- Filters: `--type node|python|go|build`, `--older-than <dur>`, `--root <path>`.
- **Safety (non-negotiable):** never delete `.git`; never follow symlinks out of root; **skip a project with a dirty/uncommitted tree unless `--force`**.

### Key reuse (cited in issue)

Fast single-`find` heavy-dir prune: `find ROOT -type d \( -name node_modules -o … \) -prune` — cut a `~/Projects` walk from ~24s to ~1s. Shell this through the Runner (or replicate in Go with `filepath.WalkDir` + skip-descend on match).

### Exact files

- `internal/clean/clean.go` — `Client` + `New(run exec.Runner, opts ...Option)`. `du`/`find`/`git status` through the Runner.
- `internal/clean/scan.go` — pure target-matching + size aggregation logic where possible; `scan_test.go` opens with `// Test plan for scan.go`.
- `internal/clean/clean_test.go` — assert dry-run issues **zero** delete calls (`FakeRunner.Calls` has no `rm`/`RemoveAll`); assert a dirty-tree project is skipped without `--force`; assert `.git` is never a target.
- `internal/cli/clean.go` — `newCleanCmd()`; register at `root.go:37`.
- `internal/forgive/forgive.go` — the issue asks for an **alias in `internal/forgive`** — add a `CleanAliases` map.
- `internal/config/config.go` — optional `[clean]` section (default root, default type filter) with `IsZero()`. Additive; see collision note.

### Security

Dirty-tree detection reuses `git -C <dir> status --porcelain` (`internal/projects/git.go` shape). The delete path is the sharp edge — prefer Go `os.RemoveAll` on a **validated** absolute path confirmed under `--root` (reuse a `withinWorkspace`-style containment check, `workflow/exec.go:317`) over shelling `rm -rf`.

### Verification transcript

```
go build ./... && go vet ./... && go test ./internal/clean/... ./internal/cli/... && go test ./...
```

Manual: dry-run reports a nonzero total and deletes nothing (diff disk usage before/after → unchanged); `--apply` on a scratch dir reclaims; re-run shows ~0 for cleaned types (issue acceptance criterion).

### Suggested implementer model

**Sonnet** for PR-1 (dep/build core). The safety guards are the review focus — pair with an adversarial security-review pass on the delete path before merge.

---

## Module #26 — `forgectl y` (clipboard + shell-history, macOS) — **partial-defer recommended**

**Refs cameronsjo/forgectl#26**

### Blocker / honest finding

The issue says the history-reading path should "ship a shim (**same pattern as `proxy`'s eval-wrapper**)." **Verified: there is no `proxy` command, eval-wrapper, or shell-shim precedent anywhere in the current `forgectl` tree** (grep for `proxy`/`eval-wrapper`/`shellenv`/`pbcopy` → zero hits). The referenced pattern is aspirational, not extant. Building the history-shim half now means **inventing** the shell-integration convention from scratch — that's a design decision, not a spec'd implementation.

### Recommended scoping

- **Ship now (this batch):** the **pure-Go clipboard half** — `internal/clip/` over `pbcopy`/`pbpaste` through the Runner. Bounded, testable, ready.
- **Defer (design decision → Cameron):** the shell-history-reading half, until the eval-wrapper/shim convention exists (either the `proxy` command lands and establishes it, or Cameron rules on the shim shape). File the deferral as a follow-on note on #26; do not block the clipboard half on it.

### Scope (clipboard half)

- Read/write the macOS clipboard via `pbcopy`/`pbpaste` through `exec.Runner`.
- Guard the platform: on non-Darwin, return a clear "macOS only" error (or feature-gate with a build tag) rather than a confusing `exec: "pbcopy": not found`.

### Exact files

- `internal/clip/clip.go` — `Client` + `New(run exec.Runner, opts ...Option)`; `Copy(ctx, s)` → `pbcopy`, `Paste(ctx)` → `pbpaste`.
- `internal/clip/clip_test.go` — assert `FakeRunner.Calls` show `pbcopy`/`pbpaste` with correct stdin/argv. (Note: `pbcopy` reads stdin — the current `exec.Runner` interface exposes `Run` (captures stdout) and `RunInteractive` (inherits stdio) but **no stdin-injection variant**. **This is a real interface gap** — piping a string to `pbcopy` needs either a new `Runner` method or a documented workaround. Flagged in Decision Points.)
- `internal/cli/y.go` — `newYCmd()`; register at `root.go:37`.
- `internal/forgive/forgive.go` — optional `YAliases`.

### Verification transcript

```
go build ./... && go vet ./... && go test ./internal/clip/... ./internal/cli/... && go test ./...
```

Manual (macOS): `echo hi | forgectl y copy` then `forgectl y paste` → `hi`.

### Suggested implementer model

**Sonnet** for the clipboard half — but the stdin-injection interface gap needs a decision before implementation (below). Do **not** dispatch the history-shim half until the shim convention is settled.

---

## Sequencing & dependencies

**All six modules are independent** — no data dependency, no shared ops logic. They can be built in parallel *if* the shared-file edits are coordinated:

### Intra-batch shared files (every module touches these — coordinate)

- **`internal/cli/root.go`** — each module adds one `root.AddCommand(newXCmd(...))` line at `root.go:37`. Append-only, merge-friendly, but N parallel branches editing the same line region **will conflict textually**. Recommendation: build **sequentially** (each PR rebases on the prior) *or* freeze the ordering up front. Simplest: **serialize the six PRs** — they're small enough that parallel isolation isn't worth the merge tax.
- **`internal/forgive/forgive.go`** — each module optionally adds a `<Domain>Aliases` map. Distinct map declarations don't conflict *semantically*, but adjacent additions can conflict textually. Same mitigation.
- **`internal/config/config.go`** — #4 clean, #23 docker (and possibly #5 branch) add TOML sections: a new field on the `Config` struct (`config.go:32-38`) + a section struct + `IsZero()`. The `Config` struct field additions are the collision point.

### Cross-plan collision with the pr-review-suite plan

The sibling `docs/plans/2026-07-06-pr-review-suite.md` **promotes sandbox helpers from `internal/workflow/exec.go` to a new `internal/sandbox` package** and **touches `internal/config/config.go`**.

- **`internal/workflow/exec.go`** — **no collision.** None of these six utilities need the sandbox helpers. They *reuse the security idioms* (`rejectOptionLike`, `withinWorkspace`, `--` separators) by pattern, not by import — copy the pattern or, once the pr-suite promotes them to `internal/sandbox`, import from there. **The pr-suite plan owns the promotion; this batch does not move those functions.** If the promotion has already landed when a utility here needs `rejectOptionLike`, import `internal/sandbox`; if not, replicate the guard locally and leave a `// TODO: consolidate into internal/sandbox once the pr-suite lands` comment.
- **`internal/config/config.go`** — **genuine co-edit.** Both plans append to the `Config` struct and add section structs. Additive TOML fields rarely conflict *semantically* (different keys), but the shared `Config` struct declaration is a textual conflict magnet. **Recommendation:** let the pr-suite land its config changes first (it's the flagship), then this batch rebases; do **not** have a utility here rename/reorder existing `Config` fields.

### Recommended build order (serial)

1. **#24 pip** (S, zero shared-file risk beyond root.go/forgive.go — pure warm-up)
2. **#28 proj clone** (S–M, isolated to `projects` module + its own verb)
3. **#23 docker** (M, self-contained)
4. **#5 branch** (M, self-contained, gotcha-heavy)
5. **#4 clean PR-1** (M, self-contained)
6. **#26 y clipboard-half** (M, gated on the stdin-interface decision)

Each rebases on `main` after the prior merges, so the `root.go`/`forgive.go`/`config.go` append-conflicts never materialize.

## Verification

### Universal gate (every PR — no test CI exists, so this IS the gate)

Paste the full transcript in the PR body:

```
go build ./...
go vet ./...
go test ./...
```

All three green. `go vet` is **separate and mandatory** — the compiler passes wrong-arity test calls that vet catches. Run both after any signature change.

### Per-module (in addition to the universal gate)

- **#24 pip:** round-trip `serialize(parse(x))==x` test green; manual `remove`→`restore`→`diff` empty.
- **#28 proj clone:** `FakeRunner.Calls` show canonical dest; `pick`/`list` still green after the discovery-depth change; manual clone lands at `~/Projects/{host}/{owner}/{repo}`.
- **#23 docker:** `FakeRunner.Calls` assert derived tag + `:dev` alias + labels + `--platform` in argv; cache reuse verified.
- **#5 branch:** `FakeRunner.Calls` assert singular ref-verify endpoint, worktree-before-branch order, no open-PR branch in delete argv; manual dry-run classifies a squash-merged branch as safe-to-delete.
- **#4 clean:** dry-run issues zero deletes; dirty-tree skip without `--force`; `.git` never targeted; re-run-after-apply shows ~0.
- **#26 y:** `FakeRunner.Calls` assert `pbcopy`/`pbpaste`; manual macOS round-trip.

## Decision points (Cameron calls — recommendations attached)

1. **#4 clean split — approve the two-PR split?** *(Recommend: yes.)* Ship dep/build-dir reclaim now; defer package-manager caches + docker prune to a follow-on PR. Keeps the safety-critical delete-path review tight.
2. **#26 y — approve the partial-defer?** *(Recommend: yes.)* Ship the pure-Go clipboard half; defer the shell-history-reading half until the eval-wrapper/shim convention exists (the referenced `proxy` precedent is absent from the repo). This unblocks the ready half without inventing a shell-integration convention under time pressure.
3. **#26 y — `exec.Runner` stdin gap.** `pbcopy` needs stdin injection; the current `Runner` interface (`exec.go:22-25`) has no stdin variant. Options: **(a, recommended)** add a minimal `RunWithInput(ctx, stdin string, name, args...)` method to `Runner` + `OSRunner` + `FakeRunner` (small, and other modules may want it too); **(b)** special-case in the `clip` package outside the seam (violates "everything shells out through the Runner" — not recommended). If (a), coordinate the interface change with the pr-suite plan since it also lives in `internal/exec`.
4. **#5 branch — `git` parent grouping.** The issue muses about grouping under a `forgectl git` parent "if more git verbs land." *(Recommend: ship `forgectl branch` flat now; don't pre-build the parent.)* Note the future grouping in a doc-comment.
5. **config.go co-edit ordering vs. pr-review-suite.** *(Recommend: pr-suite lands its config changes first; this batch rebases.)* Confirm which plan executes first so the `Config` struct field additions serialize cleanly.
6. **Serial vs. parallel build.** *(Recommend: serial, in the order above.)* The modules are small and share `root.go`/`forgive.go`/`config.go`; the fan-out merge tax exceeds the parallelism win for six S/M modules.

## Out of scope

- **The flagship `pr` suite** (#3/#19/#20/#29-#32) and the `internal/sandbox` promotion — owned by `docs/plans/2026-07-06-pr-review-suite.md`.
- **#4 clean PR-2** — package-manager cache cleaning + docker prune (deferred follow-on; see Decision 1).
- **#26 y history-reading half** — deferred pending the shim convention (see Decision 2).
- **A `forgectl git` parent command** — not built this batch (see Decision 4).
- **Any test-CI workflow** — the repo has only `release.yml`; adding CI is a separate infra task, not part of these utility modules.
- **Migrating `internal/exec.Runner` to a general stdin-capable interface beyond the minimal `RunWithInput`** needed by #26 — only the minimal addition, if Decision 3(a) is taken.
