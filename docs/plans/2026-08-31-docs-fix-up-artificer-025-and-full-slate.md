---
status: "in-flight"
updated: "2026-08-31"
branch: "plan/docs-fix-up"
body_sha256: "5efbf4e1edbf15f844140131a2a6f6205eae0cfe519221ad4ec457030a4bc6b5"
session: "deft-sonata"
session_id: "7fc5913c-2346-479f-a249-9d871812e47d"
machine: "cf6e768835c7"
approved_in: "swift-refrain"
approved_session_id: "fd2d9a79-e2cb-416d-ad09-aee42f83c2b3"
---

# forgectl docs fix-up — Artificer 0.25 upgrade + full docs slate

## Context

forgectl's docs are tracking code well (the 28-group roster is test-pinned and current) but carry five real gaps, surfaced by a two-scout survey and a four-seat panel on 2026-08-31. The headline is the Artificer design system: `forgectl docs serve` renders markdown through vendored Artificer assets pinned at **0.19.0** (vendored 2026-07-16) while upstream is **0.25.0** — six minors behind, with no script or CI running the re-vendor command that `internal/docs/assets.go` names in prose. Alongside that: `docs/RELEASING.md` still teaches manual tagging on a repo that runs release-please; `forgectl docs open` and the docs server's bearer-token surface are undocumented in the README; the README is an 885-line manual; ADRs lack Status/Date headers and an index. Cameron chose the full slate.

**Panel-verified live-state facts this plan stands on** (red-team probes, 2026-08-31): local `main` is 4 commits behind `origin/main`; `origin/main` carries `scripts/check-changelog-owner.sh` (#426) making release-please the **sole** `CHANGELOG.md` writer (hand edits hard-fail CI; `AGENTS.md` says release prose goes in a `BEGIN_COMMIT_OVERRIDE`/`END_COMMIT_OVERRIDE` PR-body block); the coverage gate `TestModules_DocumentedInREADME` is satisfied **only** by lines inside the `## Usage` fence (`README.md:62-301`) — the `## Command groups` table matches nothing; the only breaking Artificer event in (0.19.0, 0.25.0] is 0.24.0's React `useTheme` hook removal, N/A to a pure-Go consumer; and draft PR forgectl#427 (worktree `.claude/worktrees/docs-tui-reader`) holds `README.md`, `internal/docs/assets.go`, `internal/cli/docs.go`, `internal/cli/docs_serve.go`, `internal/docs/templates/shell.html.tmpl`, `internal/docs/server.go`.

## Goal

forgectl's docs current on Artificer 0.25.0 with a committed re-vendor script, an accurate release doc, a complete command-surface README roughly halved with deep-dives split into `docs/commands/` and `docs/configuration.md`, indexed ADRs — all landing as one PR from a worktree, with release notes riding the PR body's commit-override block.

## Alternatives declined

- **Everything but the README split** — smaller session, but leaves the 885-line manual that the `docs serve` Artificer surface exists to replace.
- **Artificer track only** — deferred the actively-wrong `RELEASING.md` and the undocumented auth surface; prose gaps compound faster than asset drift.
- **Prune the unreferenced vendored files** — `assets.go` documents full-set vendoring + selective `go:embed` as deliberate (keeps `tokens.json`/`provenance.json` unservable over HTTP), and `artificer vendor` restores the standard set every run. One carve-out from the panel: the retired `ARTIFICER-CHEATSHEET.md` (vendor never deletes; drops out of provenance tracking on re-vendor) is deleted in Task 2.
- **28 docs/commands pages for uniformity** — only 8 groups have deep-dive prose; 19 stub pages is minted emptiness. Pages are created where content exists (plus one earned new page for `docs` auth).

## Panel

Panel: plan-reviewer ×2 + red-team + cameron-review ran — 27 findings, 24 folded in, 3 declined (see Panel review — findings declined)

## Panel review — findings declined

- **[Task 1] Plan belongs in the meta-repo, not `forgectl/docs/plans/`** — declined: this plan plans code-coupled work riding forgectl's own PR (Lane A); forgectl's `docs/plans/` is active, 16 plans, latest 2026-08-28.
- **[Task 2] Add a blocking CI job comparing the vendored pin against the npm registry** — declined in the blocking form: it goes red on every upstream release, failing unrelated PRs on an external event. Folded instead as `scripts/vendor-artificer.sh --check` (advisory drift detection), plus a follow-up issue on artificer-design-system proposing an upstream `artificer vendor --check`.
- **[Task 4] Move the `## Usage` fence out of the README into `docs/commands/README.md`** — declined: the fence's line-leading `# <name> — …` and `forgectl <name>` lines are the **only** text satisfying the coverage gate for all 28 modules (red-team probe); moving it turns the gate red or forces a gate rewrite this PR doesn't want.

## Architecture

Two independent tracks share one branch. **Track A (assets):** `internal/docs/assets/artificer/` is re-vendored from the pinned registry package (`npx @cameronsjo/artificer@0.25.0 vendor --dest internal/docs/assets/artificer --strict` — reproducible; never the sibling checkout, whose working tree carries unreleased post-0.25.0 work) via a new committed `scripts/vendor-artificer.sh`. `assets.go` embeds nine files, three from the vendor dir (`artificer.css`, `artificer-theme.js`, `artificer-tree.js`); `mermaid-init.js` and `diagram.css` read Artificer tokens, so they're in the verification scope. Template class list to verify against `shell.html.tmpl`: `surface-tool`, `page-shell`, `appbar` (+`__brand`/`__spacer`/`__actions`), `wordmark`, `split-pane`, `sidenav` (+`__group`), `field`/`field__label`/`input`, `surface-document`, `container container--lg`, `empty-state`, `theme-toggle` — red-team probed all as present-and-grown at 0.25.0, zero removals. **Track B (prose):** the 8 existing group deep-dives plus a new `docs` page move to `docs/commands/<group>.md`; `## Configuration` (`README.md:479-878`, ~400 lines) moves to `docs/configuration.md`; the `## Usage` fence and roster table stay in the README (gate-load-bearing); the roster's `Docs` column links are rewritten to the new pages. `forgectl docs serve` already renders `docs/`, so the split is self-hosting.

## Tech Stack

- Go (module `forgectl`); module registry `internal/cli/modules.go`, 28 manifests, test-pinned
- release-please + auto-tag + goreleaser workflows; conventional commits required; version 0.14.1; `CHANGELOG.md` is release-please-owned (CI-enforced, #426)
- Artificer: registry package `@cameronsjo/artificer@0.25.0` (`vendor` subcommand: `--dest`/`--fonts`/`--strict`, exit 7 = strict drift, refuses dest outside cwd)
- `artificer-design-system:artificer-upgrade` skill drives the ledger walk

## Global Constraints

- Conventional commit subjects (release-please parses them into the changelog); **never edit `CHANGELOG.md`** (CI gate `scripts/check-changelog-owner.sh`); release prose = one `BEGIN_COMMIT_OVERRIDE`/`END_COMMIT_OVERRIDE` block in the PR body per `AGENTS.md`
- **Only the orchestrator commits.** Dispatched subagents edit files and report; the orchestrator stages and commits with explicit pathspecs (`git add <paths> && git commit -- <paths>`) and the producer-tuple trailer block on every commit (values computed by the driving session; `warn-commit-provenance` nudge block is copyable). Tuple also goes in the PR body (squash-merge repo)
- Feature work in a worktree: forgectl is branch-mode. Branch `plan/docs-fix-up` cut from **freshly fetched** `origin/main`; entry posture push -u + draft PR (stub body; full body composed and redaction-scanned at Task 6, before the ready flip)
- No Go behavior changes; comment-only edits to `assets.go` allowed; `go build ./... && go test ./...` green before the ready flip
- Vendored Artificer keeps the full standard set except the retired `ARTIFICER-CHEATSHEET.md` (deleted, Task 2); `primitives.json` arrives new by design
- **Sequencing hazard, named:** draft PR #427 overlaps Tasks 2/4 files. Assumption: this PR lands first and #427 rebases (it is a draft). If #427 is actively being worked, sequence with Cameron before Task 2.
- `cadence:redaction` over the final PR body before it posts

## Orchestrator

**Driver:** sonnet — no security triggers; every task specced against files read or panel-probed this session; flat waves, orchestrator-only commits

## Tasks

### Task 1 — Fetch, worktree entry, plan persist

**Files:**
- Create: `docs/plans/2026-08-31-docs-fix-up-artificer-025-and-full-slate.md` (this plan — Lane A)

**Interfaces:**
- Consumes: *(none)*
- Produces: worktree at `.claude/worktrees/docs-fix-up`, branch `plan/docs-fix-up` from fresh `origin/main`, pushed, draft PR (stub body)

**Dispatch:** In-context — session-entry posture, not delegable

**Report:** —

**Steps:**
- [x] `git -C forgectl fetch origin` (local main is 4 commits behind; FETCH_HEAD dated Aug 29)
- [x] Re-read at `origin/main`: `AGENTS.md`, `CHANGELOG.md` head, `.github/workflows/`, `scripts/check-changelog-owner.sh` — confirm the changelog-ownership facts above still hold; deviations amend this plan loudly
- [x] `git worktree add .claude/worktrees/docs-fix-up -b plan/docs-fix-up origin/main` (no name collision — probed)
- [x] Persist this plan into the worktree's `docs/plans/`, commit (tuple trailers), `push -u`, draft PR with stub body
- [x] Re-`Read` every file at the worktree path before its first edit (absolute-path file-state trap)

### Task 2 — Artificer 0.19.0 → 0.25.0 re-vendor + committed script

**Files:**
- Modify: `internal/docs/assets/artificer/*` (re-vendored); delete `internal/docs/assets/artificer/ARTIFICER-CHEATSHEET.md`
- Create: `scripts/vendor-artificer.sh`
- Modify: `internal/docs/assets.go` (comment-only: "two files" → three; point at the script)

**Interfaces:**
- Consumes: worktree (Task 1)
- Produces: vendored assets at 0.25.0, fresh `provenance.json`, runnable re-vendor/check script

**Dispatch:** In-context — the upgrade-skill run and verification need judgment against live output

**Report:** —

**Steps:**
- [x] Invoke `artificer-design-system:artificer-upgrade`; collect `versions[X].breaking[]` for X in (0.19.0, 0.25.0] from upstream `primitives.json`. **Expected answer, stated up front: exactly one event — 0.24.0 removes the React `useTheme` hook — N/A to a Go consumer. Any different result is the red flag** (the 0.10.1 trio — theme-key dot rename, `.tok-keyword`, px→rem — predates the 0.19 pin and proves nothing here)
- [ ] Write `scripts/vendor-artificer.sh`: `vendor` mode wraps `npx @cameronsjo/artificer@<pin> vendor --dest internal/docs/assets/artificer --strict`; `--check` mode compares `provenance.json` `.version` against `npm view @cameronsjo/artificer version` (advisory drift detection). One verdict line, meaningful exit codes, TTY-aware color
- [ ] Run it; delete `ARTIFICER-CHEATSHEET.md` in the same change; diff `artificer.css`/`artificer-theme.js`/`artificer-tree.js` against the template class list (Architecture) and eyeball `mermaid-init.js`/`diagram.css` token names against the new `tokens.json`
- [ ] Verification (no server needed — measured output recorded here at execution): `grep -c 'v0.25.0' internal/docs/assets/artificer/artificer.css` ≥ 1, `jq -r .version internal/docs/assets/artificer/provenance.json` = `0.25.0`, `go build ./... && go test ./internal/docs/` green. Optional human check: `go run . docs serve` and look at a page
- [ ] File the follow-up issue upstream: `artificer vendor --check` as a first-class flag (cameron-review finding)
- [ ] Commit: `fix(docs): re-vendor Artificer 0.19.0 -> 0.25.0; add vendor script` (+ tuple trailers)

### Task 3 — RELEASING.md rewrite for release-please

**Files:**
- Modify: `docs/RELEASING.md` (27 lines today)

**Interfaces:**
- Consumes: worktree (Task 1)
- Produces: release doc describing the real flow

**Dispatch:** Parallel (wave 2) · fresh Sonnet subagent — **edits only, no commits**; sees this block + Global Constraints

**Report:** resolved at dispatch time to `<mktemp -d>/task-3.md` by the orchestrator

**Steps:**
- [x] Read `.github/workflows/{release-please,auto-tag,release}.yml` + `release-please-config.json` at the worktree; document the actual flow: conventional commits → release PR (`chore(main): release …`) → merge → auto-tag → goreleaser
- [x] Delete the manual `git tag` recipe. **State manual tagging as forbidden, not break-glass**: a hand-pushed tag does build (release.yml fires on tag push) but desynchronizes `.release-please-manifest.json` and wedges the release PR (`auto-tag.yml:12-17` documents the wedge)
- [x] Keep the `releasing-to-homebrew-tap` pointer and the `## Verify before tagging` goreleaser block (retitled to fit the new flow) — acceptance: doc mentions no hand-run `git tag`, names the release-PR flow, survives `forgectl docs serve` rendering
- [x] Orchestrator commits: `docs(releasing): rewrite for release-please; manual tagging is a wedge, not a path`

### Task 4 — README: document gaps, then split deep-dives out

**Files:**
- Modify: `README.md`
- Create: `docs/commands/{env,resume,k8s,proxy,projects-and-review,launch,pr,bench}.md` (verbatim-first extraction) and `docs/commands/docs.md` (new page: `docs open` + bearer-token surface)
- Create: `docs/configuration.md` (the `## Configuration` section, `README.md:479-878`, moved whole)

**Interfaces:**
- Consumes: worktree (Task 1)
- Produces: README ≈ 400-450 lines (overview, install, roster table with rewritten `Docs` links, intact `## Usage` fence, one-paragraph config pointer to `docs/configuration.md`); nine command pages + one configuration page

**Dispatch:** Serial within itself; parallel to Task 3 · fresh Sonnet subagent — **edits only, no commits**

**Report:** resolved at dispatch time to `<mktemp -d>/task-4.md` by the orchestrator

**Steps:**
- [ ] Page pattern (uniform, all ten files): H1 `# forgectl <group> — <one-liner from the roster>`, then `> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).`, then the section content verbatim; no front matter
- [ ] Write `docs/commands/docs.md` from code, not invention: `docs open` registered at `internal/cli/docs.go:62`; token rules from the command's `Long` text (`docs.go:26-58`): token file absolute + owner-only, RFC 6750 bearer, protected servers can't be `--open`ed directly. Add one roster-table row link and a line in the README docs group's usage block naming `open`
- [ ] Extract the 8 deep-dive sections (`README.md:339,372,459,502,546,616,809,862`) verbatim into their pages; move `## Configuration` to `docs/configuration.md`; **do not touch the `## Usage` fence (lines 62-301) — it alone keeps `TestModules_DocumentedInREADME` green**; `## How it fits together` and `### External commands`/`### Logging` stay in the README
- [ ] Rewrite the roster table's `Docs` column: moved sections → `docs/commands/<group>.md` links (kills the 8 now-dead anchors); unmoved stay "Usage below"
- [ ] Gate check: `go test ./internal/cli/ -run TestModules_DocumentedInREADME` green; link check: grep the README for `](#` anchors pointing at removed headings — zero hits
- [ ] Orchestrator commits (two): `docs(readme): document docs open and the bearer-token surface`, `docs: split deep-dives into docs/commands/ and docs/configuration.md`

### Task 5 — ADR headers + index

**Files:**
- Modify: `docs/adr/00*.md` (8 files)
- Create: `docs/adr/README.md`

**Interfaces:**
- Consumes: worktree (Task 1)
- Produces: each ADR carries Status/Date; index table exists

**Dispatch:** Parallel (wave 2) · fresh Haiku subagent — **edits only, no commits**; mechanical, exact spec below

**Report:** resolved at dispatch time to `<mktemp -d>/task-5.md` by the orchestrator

**Steps:**
- [x] Per ADR, insert directly under the `# NNNN — <title>` line: a blank line, then `Status: Accepted` and `Date: <YYYY-MM-DD>` on their own lines, then a blank line. Date = first-commit date: `git log --follow --format=%as -- <file> | tail -1`. Status vocabulary: `Accepted` unless the ADR body itself says superseded/deprecated — then match the body and note it in the report
- [x] `docs/adr/README.md`: table of number · title (linked) · status · date
- [x] Orchestrator commits: `docs(adr): add status and date headers plus an index`

### Task 6 — Integrate, review, ready flip

**Files:**
- Modify: `docs/README.md` (single writer for the index: add `commands/`, `configuration.md`, point at `adr/README.md`); others as findings dictate

**Interfaces:**
- Consumes: Tasks 2-5

**Dispatch:** In-context

**Report:** —

**Steps:**
- [ ] Update `docs/README.md` index; spot-check three pages (one command page, `configuration.md`, the ADR index) render in `forgectl docs serve`
- [ ] `go build ./... && go test ./...` green in the worktree
- [ ] Run diff-based reviewers against a diff built from the worktree (`cadence:code-reviewer`; polish's built-in arms no-op on sub-repo worktrees); fold findings; run `cadence-forge:polish` for the transcript gate
- [ ] Compose the final PR body: summary, `BEGIN_COMMIT_OVERRIDE`/`END_COMMIT_OVERRIDE` release-notes block (this is the changelog channel), producer tuple; `cadence:redaction` scan; update the body
- [ ] Flip draft → ready

## Deviations

- **Task 1:** the persist-plan hook homed the plan in the meta-repo; redirected to forgectl `docs/plans/` per this plan's own Lane-A ruling (declined finding #1).
- **Task 5:** all 8 ADRs already carry `- **Status:** Accepted (YYYY-MM-DD)` as their first bullet — the header-insertion step was moot (survey premise stale); only the index was created, and the commit message drops "status and date headers".
- **Task 2 (note):** the 0.25.0 ledger's `versions{}` carries no 0.23.0/0.25.0 entries (releases without ledger events); the breaking[] walk over the keys present confirms the expected single 0.24.0 event.

## Learnings

*(empty at approval)*
