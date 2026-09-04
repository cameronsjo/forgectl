---
updated: "2026-09-04"
body_sha256: "ebbf957cb685f06b84ef226b7111cd6dc3ec1a11ce1c57396fdfbf6015a21b06"
session: "sober-fugue"
session_id: "5701aa2e-65f8-45a6-ba11-0c444d897940"
model: "claude-fable-5-1"
harness: "claude-code 2.1.261"
machine: "cf6e768835c7"
approved_in: "woven-lyre"
approved_session_id: "1488f160-f360-414b-952e-1f3846a08602"
status: in-progress
next: "Task 2 types + scanDoc → Task 3 ResolveLink → Task 4 Backlinks → Task 5 cleanup + polish"
branch: plan/link-substrate
pr: "cameronsjo/forgectl#451"
issue: cameronsjo/forgectl#443
epic: cameronsjo/forgectl#442
---

# Link resolution substrate for `forgectl docs` (forgectl#443)

## Goal

Give `internal/docs` a resolver that maps every link form in an indexed document to another indexed document or a typed miss, plus a reverse index, so Obsidian rendering, `docs check`, and backlinks can all stand on one deterministic answer. A spike against the live vault settles the semantics and the scope before the resolver is wired.

## Alternatives declined

- **Hand-written wikilink parser** — `go.abhg.dev/goldmark/wikilink` v0.6.0 (same author as the `goldmark/frontmatter` dependency already in `go.mod`) parses `[[note|alias]]`, `![[embed]]`, `[[note#heading]]`, and `[[note#^block]]`; only its `Resolver` is written here.
- **Vault-wide resolution for a root that sits inside a vault** — `ResolveLink` returns a `*Doc`, and every `*Doc` must be servable; a hit outside the root is a link `Index.Resolve` then 404s. Root-only is the default and the spike measures whether it is too narrow (Task 1 stop conditions).
- **Incremental index updates from the watcher** — `Rebuild()` already routes through `NewIndex`, and a second builder reintroduces the drift `excludedDir`'s comment (`index.go:249`) warns about. The new tables ride the existing full rebuild; the spike budgets the cost.
- **A cobra verb for the spike** — a `t.Skip`-gated test needs no CLI surface, no help text, and deletes in one `git rm`.
- **`Resolve` as the method name** — collides with the path-containment check at `index.go:431`; the name is `ResolveLink`.

## Panel

Panel: plan-reviewer, red-team-reviewer ran — 13 findings, 13 folded in, 0 declined

## Panel review — findings declined

- none declined — all findings folded in

## Architecture

`scanDoc` (new, `internal/docs/linkscan.go`) replaces both `titleFor` call sites (`walkRoot` at `index.go:306`, `indexFileRoot` at `index.go:205`) and returns, in one file read, the title, frontmatter `aliases`, headings (text + goldmark auto-ID slug), `^block-id` suffixes, and outbound link targets. `NewIndex` builds three per-root lookup tables (`byRel`, `byName`, `byAlias`) and then the reverse index by running `ResolveLink` over every outbound link, so the forward and reverse answers can never disagree. `detectRootKind` (new, `internal/docs/vault.go`) walks up from a root for `.obsidian/`, stopping at `$HOME`, a device change, or `/`, and sets `Root.Kind`; a config override wins over detection. `ResolveLink` consults only the calling doc's root tables; cross-root fallback does not exist as code. The watcher is untouched: `Rebuild()` → `NewIndex` → pointer swap carries the new tables. `.obsidian/` is already skipped by `excludedDir` (every dot-directory), and `docs list --json` marshals its own `docJSON` wire struct (`internal/cli/docs_list.go:39-44`), so new `Doc` fields change no consumer-visible output.

## Tech Stack

- Go 1.26 (per `go.mod`), goldmark v1.8.4 pipeline in `internal/docs/render.go`
- `go.abhg.dev/goldmark/wikilink` v0.6.0 — new dependency, parser + `Resolver` seam (`ResolveWikilink(*Node) ([]byte, error)`; `Node{Target, Fragment []byte; Embed bool}`); its parser splits `#` with `bytes.LastIndex` (`parser.go:83-85`)
- `gopkg.in/yaml.v3` (already present via frontmatter) for `aliases` as list or scalar
- `go test` with `testdata/` fixtures; `runtime.MemStats` for the spike

## Global Constraints

- MUST NOT change `Index.Resolve(rootLabel, relPath)` at `internal/docs/index.go:431` or any caller; existing `index_test.go`, `server_test.go`, `watcher_test.go` stay green untouched.
- MUST keep one index builder: all new tables are built inside `NewIndex`; `watcher.go` is not modified.
- MUST resolve within the calling document's root only; `idx.byRoot[from.RootLabel]` is the only table `ResolveLink` reads.
- MUST reconstruct Obsidian's fragment from the parser's output: `wikilink` splits on the **last** `#`, so for `[[note#A#B]]` it yields `Target="note#A"`, `Fragment="B"`. The resolver splits `Target` on its **first** `#` and rejoins: path `note`, fragment `A#B`. A fragment containing `#` is a nested heading path; match its last segment as the heading.
- MUST bound the vault walk-up: stop at `$HOME`, a `st_dev` change, or `/`. `~/.obsidian` exists on this machine and an unbounded walk calls the home directory a vault.
- MUST match anchors against goldmark's auto-ID (`parser.WithAutoHeadingID`, enabled at `render.go:73`) for docs roots, pinned by a test that renders a heading through `Render()` and asserts the emitted `id=` equals the slug the resolver computes.
- MUST keep the spike out of CI: `t.Skip` unless `FORGECTL_LINK_SPIKE=1`. CI (`.github/workflows/ci.yml`) runs `go test ./...` with only `FORGECTL_REQUIRE_TMUX` set and no `-race`/`-count` matrix; the gate holds as long as no workflow sets that variable.
- MUST NOT write anything into the vault or under `$CADENCE_FIELD_REPORTS_DIR`; the spike is read-only against those paths.
- **MUST emit integers only from the spike.** No target, path, title, basename, or alias string may appear in any `t.Log`, `t.Errorf`, report line, or this plan; the measurement run drops `-v` (kept only for the skip-count check). This binds the spike's own output, not just the plan and PR.
- No new runtime dependency beyond `goldmark/wikilink`. `go.sum` churn from its own `go.mod` (testify, older goldmark) is expected; the assertion is `git diff --stat go.mod` shows exactly one added `require` line.
- Conventional commits; every commit carries the producer-tuple trailers.

## Orchestrator

**Driver:** sonnet — flat spec'd tasks after the spike; the spike read-out and the scope verdict return to the seat (Fable/Opus) before Task 2 dispatches. The orchestrator substitutes the vault root path into Task 1's invocation from `$OBSIDIAN_VAULT` at dispatch time; the path never enters this document.

---

## Tasks

### Task 0 — Worktree, branch, and the plan's committable home

**Files:**
- Create: `docs/plans/2026-09-04-link-substrate.md` (this document)

**Dispatch:** In-context · performed by the `persist-plan-approval` hook at approval (worktree `.claude/worktrees/link-substrate`, branch `plan/link-substrate` from `origin/main`, plan committed, draft PR opened). Fallback if the hook's context line does not appear: `git worktree add .claude/worktrees/link-substrate -b plan/link-substrate origin/main`, copy this file to the path above, commit, `git push -u --no-follow-tags`, `gh pr create --draft -R cameronsjo/forgectl`.

**Report:** —

**Steps:**
- [x] Confirm the worktree and branch exist and the plan is committed (`git -C .claude/worktrees/link-substrate log --oneline -1 -- docs/plans/2026-09-04-link-substrate.md`)
- [x] Every later "this document" or `## Learnings` means that path; all `go get`, edits, and commits happen in that worktree

---

### Task 1 — Spike: resolve every link in the live vault and `forgectl/docs`

**Files:**
- Create: `internal/docs/linkspike/spike_test.go` (package `linkspike_test`, throwaway)
- Modify: `go.mod`, `go.sum` (`go get go.abhg.dev/goldmark/wikilink@v0.6.0`)

**Interfaces:**
- Consumes: *(none)*; a private first cut of the scan + tables lives in the spike package and is replaced by Tasks 2-3
- Produces: the read-out table below, committed into this plan's `## Learnings`

**Dispatch:** Serial (wave 1) · fresh Sonnet subagent — spec'd harness; the read-out is judged by the seat. The orchestrator passes the three concrete root paths in the dispatch prompt.

**Report:** `<reports-dir>/task-1.md` — reports-dir is the orchestrator's out-of-tree `mktemp -d`; write the integer read-out table there; reply with only the output of `wc -l` against that path

**Steps:**
- [x] `go get go.abhg.dev/goldmark/wikilink@v0.6.0`; `go mod tidy`; assert `git diff --stat go.mod` shows one added line
- [x] Write the spike test: repeated `-roots` flag via `flag.Var` at package init (verified workable through `go test` package-list mode); for **each root separately** build a first-cut index (scan + tables + root-kind detection), `runtime.GC()`, read `MemStats`, resolve every link, collect integer counters, `runtime.GC()`, read `MemStats` again. Roots are never combined into one index; the field-reports root sits inside the vault root and would double-count.
- [x] Gate with `t.Skip` unless `FORGECTL_LINK_SPIKE=1`; verify the gate (recipe: `go test ./internal/docs/linkspike -v 2>&1 | grep -c SKIP` → `1`)
- [x] Measurement run, no `-v`: `FORGECTL_LINK_SPIKE=1 go test ./internal/docs/linkspike -run TestLinkSpike -timeout 300s -roots "$CADENCE_FIELD_REPORTS_DIR" -roots ./docs -roots "$OBSIDIAN_VAULT"` — the test writes its table to the path in `FORGECTL_LINK_SPIKE_OUT` (the report path) rather than to the test log
- [x] Record per root, integers only: `kind` (0/1), walk-up stop reason (0 found, 1 home, 2 device, 3 root); `docs`, `links` by form (plain, alias, embed, `#heading`, `#^block`, relative-path); `resolved`, `missNoTarget`, `missAmbiguous`, `missOutsideRoot`, miss ‰; `indexMS`, `resolveMS`; `heapAllocDelta`, `heapAllocPeak`; count of unresolved targets whose basename exists elsewhere in the vault root; count of ambiguous basenames; count of filenames containing `#`
- [x] Commit the read-out to `## Learnings` in this document with one provenance line: host digest, Go version, vault md count, field-reports md count: `chore(docs): link resolution spike read-out`
- [x] **Stop conditions, decided by the seat before Task 2:** (a) field-reports-root miss rate > 30% *and* more than half of the missed targets exist elsewhere in the vault → reopen the root-only scope decision; (b) `indexMS` > 1000 on the field-reports root or > 5000 vault-wide → stop, incremental indexing is a different issue; (c) `heapAllocDelta` > 50 MB vault-wide → stop, table representation needs interning; (d) ambiguous basenames > 5% of vault docs → Obsidian's same-folder-then-shortest-path tiebreak becomes a Task 3 requirement and `MissAmbiguous` narrows to true ties; (e) any filename containing `#` → the first-`#` split gets a documented caveat, otherwise none

---

### Task 2 — Types, root kind detection, and the one-pass document scan

**Files:**
- Create: `internal/docs/links.go` (`RootKind`, `Miss`, `LinkRef`, `Heading`, `rootIndex`)
- Create: `internal/docs/vault.go` (`detectRootKind`)
- Create: `internal/docs/linkscan.go` (`scanDoc`)
- Modify: `internal/docs/index.go` — `Root` gains `Kind RootKind`, `VaultPath string`; `Doc` gains `Aliases []string`, `Headings []Heading`, `BlockIDs []string`, `Links []LinkRef`; **both** `titleFor` callers switch to `scanDoc`: `walkRoot` (`index.go:306`) and `indexFileRoot` (`index.go:205`); `indexDirRoot` (~156) and `indexFileRoot` (~174) call `detectRootKind`; `titleFor` is deleted once unreferenced
- Test: `internal/docs/vault_test.go`, `internal/docs/linkscan_test.go`, fixture `internal/docs/testdata/links/`

**Interfaces:**
- Consumes: spike read-out (Task 1) for the tiebreak decision
- Produces: `type RootKind uint8` (`RootDocs`, `RootVault`); `type Miss uint8` (`MissNone`, `MissNoTarget`, `MissAmbiguous`, `MissOutsideRoot`); `scanDoc(path, rel string) (docMeta, error)`; `detectRootKind(canonical string) (RootKind, string)`

**Dispatch:** Serial (after Task 1) · fresh Sonnet subagent — spec'd; owns the new files

**Report:** `<reports-dir>/task-2.md`

**Steps:**
- [ ] Build the fixture tree: `vault/` with `.obsidian/app.json`, `index.md`, `notes/Alpha.md` (aliases list), `notes/beta.md` (alias scalar), `notes/deep/Alpha.md` (ambiguous basename), `notes/anchors.md` (`## Some Heading`, a paragraph ending `^blk-1`, and `### Sub` under it), `notes/orphan.md`; `repo/` with `index.md`, `guide.md` (`## Getting Started`), `sub/nested.md`; `single.md` at the fixture top for the single-file-root case
- [ ] Failing tests for `detectRootKind`: fixture `vault/` → `RootVault`; `repo/` → `RootDocs`; a synthetic `.obsidian` placed above a temp root but below a simulated `$HOME` boundary → still `RootDocs`
- [ ] Failing tests for `scanDoc`: title, aliases (list and scalar), headings with slug, block ids, outbound links by form
- [ ] Failing test: a single-file root (`indexFileRoot`) yields a `Doc` with non-nil `Headings` and `Links`
- [ ] Implement; run — expect GREEN; existing `index_test.go` green untouched
- [ ] Commit: `feat(docs): typed roots and one-pass document scan`

---

### Task 3 — `ResolveLink` and the per-root tables

**Files:**
- Modify: `internal/docs/index.go` — `Index` gains `byRoot map[string]*rootIndex`; tables built in `NewIndex` right after the `pathIndex` loop (~lines 150-153)
- Modify: `internal/docs/links.go` — `func (idx *Index) ResolveLink(from *Doc, target string) (*Doc, Miss)`
- Test: `internal/docs/links_test.go`

**Interfaces:**
- Consumes: Task 2 types and `scanDoc`
- Produces: `ResolveLink(from *Doc, target string) (*Doc, Miss)`; contract: a non-nil `*Doc` with `MissNoTarget` means the file resolved and the anchor did not

**Dispatch:** Serial (after Task 2) · fresh Sonnet subagent

**Report:** `<reports-dir>/task-3.md`

**Steps:**
- [ ] Failing tests, one per row: vault basename hit; `MissNoTarget`; `MissAmbiguous` (two `Alpha.md`); `[[deep/Alpha]]` path-prefix disambiguation; `MissOutsideRoot` for `[[../repo/index]]` and `[../../etc/passwd](...)`; alias list; alias scalar; case fold; vault heading anchor hit and miss (doc + `MissNoTarget`); block id; nested heading `[[anchors#Some Heading#Sub]]` → doc + `MissNone` with fragment reconstructed as `Some Heading#Sub`; docs-root anchor `guide.md#getting-started`; within-root isolation both directions; fragment-only link resolves to `from`
- [ ] The slug-agreement test: render `## Getting Started` through `Render()`, extract `id=`, assert equal to the resolver's slug
- [ ] Implement the algorithm: reconstruct path and fragment per the Global Constraint; empty path → `from`; docs roots: relative path from `filepath.Dir(from.RelPath)`, `path.Clean`, leading `..` or `/` → `MissOutsideRoot`, look up `byRel`; vault roots: `byRel` (folded, ext optional) → `byName` → `byAlias`, a `/` in the target filters `byName` candidates by `RelPath` suffix; apply the Task 1 tiebreak decision; then the fragment check per root kind
- [ ] Run — expect GREEN
- [ ] Commit: `feat(docs): ResolveLink with Obsidian and GitHub anchor semantics`

---

### Task 4 — `Backlinks`, index options, and the root-kind override

**Files:**
- Modify: `internal/docs/index.go` — `backlinks map[docKey][]int` built in `NewIndex` after the tables by running `ResolveLink` over every `Doc.Links`; `func (idx *Index) Backlinks(*Doc) []*Doc` sorted by `RelPath`, deduped; **new** `type IndexOptions struct { RootKinds map[string]RootKind }` and `func NewIndexWithOptions(paths []string, opts IndexOptions) (*Index, error)`, with `NewIndex(paths)` becoming `NewIndexWithOptions(paths, IndexOptions{})`; `indexDirRoot` and `indexFileRoot` take the override map and prefer it over `detectRootKind`
- Modify: `internal/config/config.go` (~line 515, `DocsConfig`) — `RootKinds map[string]string` (`toml:"root_kinds"`), values `docs` | `vault`, keyed by the root path as configured
- Modify: `internal/cli/docs_roots.go` — a helper that converts `DocsConfig.RootKinds` to `IndexOptions`; `internal/cli/docs_serve.go:57` and `internal/cli/docs_list.go:28` switch from `NewIndex` to `NewIndexWithOptions` through that helper
- Test: `internal/docs/links_test.go` (backlinks, rebuild, override), `internal/config` test for the new key

**Interfaces:**
- Consumes: `ResolveLink` (Task 3)
- Produces: `Backlinks(*Doc) []*Doc`; `IndexOptions`; `NewIndexWithOptions`; config key `[docs] root_kinds`

**Dispatch:** Serial (after Task 3) · fresh Sonnet subagent

**Report:** `<reports-dir>/task-4.md`

**Steps:**
- [ ] Failing tests: `notes/Alpha.md` lists its linkers; `notes/orphan.md` returns empty and never panics; add a file, `Rebuild()`, assert tables and backlinks reflect it; `IndexOptions{RootKinds: {"repo": RootVault}}` flips the `repo/` fixture to vault semantics; a single-file root appears in `Backlinks` of the doc it links to
- [ ] Implement; run — expect GREEN; `watcher_test.go` green untouched; both cobra call sites compile against the new helper
- [ ] Commit: `feat(docs): Backlinks reverse index and root_kinds override`

---

### Task 5 — Remove the spike, finish, polish

**Files:**
- Delete: `internal/docs/linkspike/`
- Modify: `CHANGELOG.md` `[Unreleased]` — one entry (new resolver API and `root_kinds` key; no user-visible rendering change yet)
- Modify: this document — tick boxes, `## Learnings`

**Dispatch:** In-context

**Report:** —

**Steps:**
- [ ] `git rm -r internal/docs/linkspike`; `go build ./... && go vet ./... && go test ./...` green
- [ ] `golangci-lint run` clean on changed files
- [ ] Changelog entry
- [ ] Commit: `chore(docs): remove link resolution spike`
- [ ] run `cadence-forge:polish`; fold findings (wrong-tree pointer: run the reviewer arms against a diff built from the worktree)

---

## Deviations

*(empty at approval)*

## Learnings

### Task 1 read-out

Provenance: host digest `cf6e768835c7`, Go `go1.26.5`, vault md count 3679, field-reports md count 809.

| root# | kind | stopReason | docs | links_plain | links_alias | links_embed | links_heading | links_block | links_relpath | resolved | missNoTarget | missAmbiguous | missOutsideRoot | missPermille | indexMS | resolveMS | heapAllocDeltaBytes | heapAllocPeakBytes | unresolvedBasenameElsewhereInVault | ambiguousBasenames | filenamesWithHash |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | 1 | 0 | 809 | 1131 | 377 | 0 | 0 | 0 | 20 | 1455 | 66 | 0 | 7 | 47 | 968 | 78 | -1071960 | 1906792 | 20 | 1 | 0 |
| 2 | 0 | 1 | 47 | 0 | 0 | 0 | 0 | 0 | 50 | 26 | 4 | 0 | 20 | 480 | 35 | 0 | -124792 | 960296 | 0 | 1 | 0 |
| 3 | 1 | 0 | 3679 | 4262 | 773 | 324 | 99 | 0 | 348 | 3328 | 2324 | 151 | 3 | 426 | 2338 | 2 | -6066784 | 6916472 | 63 | 61 | 5 |

Seat verdict on the stop conditions: (a) field-reports miss rate 47‰ — root-only scope holds; (b) indexMS 968 field-reports / 2338 vault, both under the line, field-reports by 3%; (c) heap peak 6.9 MB — no interning; (d) ambiguous basenames 61 of 3679 vault docs (1.7%) — `MissAmbiguous` stays broad, no Obsidian tiebreak; (e) 5 vault filenames contain `#` — Task 3 documents the first-`#` split caveat on `ResolveLink`. The repo-docs root (root 2) misses 480‰, 20 of them outside-root: relative links from `docs/` into source files, expected and not a stop condition.
