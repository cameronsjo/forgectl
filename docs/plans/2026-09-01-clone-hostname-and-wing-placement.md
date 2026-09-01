---
status: "in-flight"
updated: "2026-09-01"
branch: "feat/hostname-and-wing-placement"
body_sha256: "2415100a4c6a59aa3e6533ab414550ae1efd7a13af7a86e7608057274938e18a"
session: "stone-anthem"
session_id: "d33eb60f-e303-473c-8ac8-46ffc7941d8e"
machine: "cf6e768835c7"
approved_in: "keen-chord"
approved_session_id: "27d82593-a9ed-411d-a4fc-856c51a7bf4e"
---

# forgectl: full-hostname clone paths and wing-aware placement

## Context

`forgectl projects clone` files a repo at `~/Projects/<token>/<owner>/<repo>` where `<token>` is the literal string `github` or `gitea` — `canonicalDest`, `internal/projects/projects.go:559`. The estate convention (`cadence-ecosystem/docs/lore/the-estate.md:20-24`), already live on disk, uses the **full hostname**:

```text
~/Projects/<wing>/<repo>          # repos that belong to a wing
~/Projects/<host>/<org>/<repo>    # every other repo with a resolvable remote
~/Projects/<name>                 # no wing, no remote
```

On disk: `~/Projects/github.com/{cameronsjo,ggml-org,seerr-team}/`, `~/Projects/git.sjo.lol/cameron/`, seven wings. Neither `~/Projects/github/` nor `~/Projects/gitea/` exists — **nothing to migrate.** forgectl has never been what built these trees; the first real `clone` would have started a parallel one.

Three defects follow from the token:

1. **The hostname is shortened.** `github` instead of `github.com`, `gitea` instead of `git.sjo.lol`.
2. **A work GHE host collides with github.com.** `canonicalHost` (`project.go:296-303`) maps *any* configured GitHub host to the single token `github`, so `github.work-domain.com/org/repo` shares both a directory tree and a `Repo.Key()` dedup bucket with a github.com repo of the same owner/name. `docs/commands/projects-and-review.md:79-80` records the symptom without naming the cause.
3. **Wings are invisible.** forgectl has zero wing awareness (`grep -ri wing internal --include='*.go'` → nothing). Discovery walks `<root>/<host>/<owner>/<repo>` (3 levels) plus flat `<root>/<repo>` (1 level); wing members sit at 2. So `projects list`, `pick`, `pull-all`, and `surface launch` under-report by every wing member — 69 repos across seven wings on this machine.

Intended outcome: forgectl's on-disk model *is* the estate's three filing rules. A clone lands where the estate says it lands, `projects list` sees everything, and no code path can shorten a hostname.

## Goal

One host vocabulary — the full, normalized hostname — used for the path segment, the dedup key, both dispatch switches, and the `--host` filter; plus wing placement from config and wing *discovery* from disk.

## Non-goals

- Migrating anything on disk. The legacy `github/` and `gitea/` directories do not exist; the flat-legacy read affordance is untouched.
- Passing a host to `tea`. `giteaList` (`gitea.go:22`) runs `tea repo ls` with no host argument and lets tea resolve its own login. **Amended 2026-09-01:** no Gitea host reaches an argv because there is no configured Gitea host at all — see Deviation D1.
- Unifying `[gitea].host` with the existing `[review.gitea].host`. **Moot under D1** — `[gitea]` is never introduced, so there is nothing to unify and no issue to file.

## Global Constraints

- All work lands in the `feat/hostname-and-wing-placement` worktree; the forgectl primary checkout is never written.
- `go vet ./... && go test ./...` MUST pass before every commit — no red checkpoints.
- No network in tests: path-formula and dispatch cases are unit tests, never live clones.
- The exact-match anti-spoofing property of `canonicalHost` (`project.go:271-280`) MUST survive unchanged; any change to it is out of scope and blocks.
- Security review (T3) runs at Opus tier before T4, and again at T9 — never Sonnet, never Haiku (Usage Principle U9).
- No on-disk migration: legacy `github/` and `gitea/` trees are not created, read, or moved.
- Server-supplied text (hostnames, owners, repo names) MUST NOT reach a terminal unwrapped or a path segment unguarded.

## Approach

### 1. Full hostname replaces the token, everywhere

`Repo.Host` becomes the normalized hostname. `canonicalHost(hostname, gitHubHost)` returns the configured GitHub host on an exact match, and otherwise the **normalized** `bare` value it already computes — lowercased, one trailing `:port` stripped, and (amended per S-P3) one trailing `.` stripped. There is no Gitea arm: after §2 the Gitea host is just another hostname arriving from a URL, which is the whole point of the change.

**What returning `bare` actually fixes (amended 2026-09-01, S-C1/S-P1/S-P2).** The approved plan claimed two effects; the pre-implementation security review refuted one and under-stated the other. Corrected:

- **The real defect is a forgeable token, and it is a live Critical on `main`.** `canonicalHost`'s default arm returns the raw hostname into the *same value space* as the tokens, so a remote whose bare hostname is literally `github` mints the trusted token. Probed: `forgectl projects clone https://github/evil/repo` → `ParseCloneTarget` sees `host == "github"` and leaves `SSHURL` empty → `Clone` takes `case "github"` → **`gh repo clone evil/repo` against public github.com**. A URL naming a different server clones an attacker-chosen repo. The same input reaches `cloneBareRepo` through `worktree.go:49`. Separately, a planted checkout with `origin = git@github:cameronsjo/forgectl` keys identically to the genuine repo and **suppresses the real row** in `Inventory`. Full hostnames close this because `"github"` stops being anything's canonical value.
- **The `Key()`-mirrors-the-tree claim was wrong as written.** Both `Key()` (`project.go:174`) and `canonicalDest` (`projects.go:560`) already `ToLower`, so a raw `GitHub.Example.com` does *not* falsify the invariant. What it does produce is a **split identity**: one directory and one `Key()`, but two different `r.Host` values, so the dispatch switch and `filterRepos` (`projects_list.go:99`, case-sensitive) disagree with the tree. That is the property to test.
- **The `:port` rationale was false.** `url.Hostname()` strips the port, and the scp-like branch puts the port in the *path*. The only way `:` reaches `Repo.Host` is an IPv6 literal (`ssh://git@[::1]:22/o/r` → `host="::1"`), which `validPathSegment` accepts. Test that, not `gh.example.com:8443`.

The exact-match anti-spoofing property documented at `project.go:271-280` — the reason `evil-github.com.attacker.net` is not stamped as trusted inventory — is preserved unchanged. It holds today; the token space it returns *into* is what did not.

Touched sites:

| Site | Change |
| --- | --- |
| `project.go:281-304` `canonicalHost` | drop the Gitea arm; return normalized hostnames; strip one trailing `.` alongside the port (S-P3 — `git@github.com.:o/r` otherwise forks the key, and under the new segment guard would stop cloning entirely) |
| `project.go:187` `parseRemoteURL` | signature unchanged (no `giteaHost` to thread) |
| `project.go:244-256` `ParseCloneTarget` | bare `owner/repo` shorthand stamps the effective GitHub host; the SSHURL branch compares against it |
| `projects.go:539-551` `Clone` dispatch | `case "github"` → compare against the effective GitHub host |
| **`worktree.go:49-61` second dispatch** | the same switch, in the bare-worktree path — **both** panel seats caught this independently; left alone, every GitHub worktree falls to `cloneBareFromURL` with a server-supplied URL, dropping the `GH_HOST` pin and the non-default-host token scrub that `github.go:118-126` puts *inside* `cloneBareRepo` "where no future caller can forget it" — and a shorthand target (`SSHURL == ""`) dies outright |
| `github.go:107` | GitHub rows stamp the configured hostname |
| `gitea.go:44` | Gitea rows derive their hostname from the row's own URL (§2); a row with no derivable host, or one claiming the GitHub host, is skipped |
| `github.go:109`, `gitea.go:36-49` | **new (S-I2)** — validate row-supplied `Owner`/`Name` at the stamp boundary, not only in `Clone`: the owner/repo charset plus a leading-dot rejection and a length bound. A repo literally named `.git` otherwise makes `isGitRepo` report the *wing directory itself* as a repo, hiding every member — the exact defect this plan fixes, re-armed by a repo name. `.bare` collides with the worktree layout |
| `project.go:127-139` `hostBadge` | **retire the mapping** — display the hostname itself, `local` for empty. It is a `Repo` method with no access to configured hosts, so any mapping must hardcode `github.com` and silently drops a GHE host's badge. Widen `DisplayLine`'s `%-12s` to fit a real hostname |
| `projects.go:422-424,448` `Inventory` | host labels and fold order still say `github`/`gitea`; `projects_test.go:172` asserts on it |
| `internal/cli/projects_list.go:38,44-45,86,92-101` | `--host` stays a **closed allowlist** — the two resolved hosts plus `local`, error otherwise — and the help example and flag usage string both move off `github | gitea` |
| `project.go:30` | `Repo.Host`'s documented JSON value is the `projects list --json` wire contract; update it, and call the break out in the changelog |

**Zero-value defense.** `canonicalHost:287-289` already defaults an empty `gitHubHost` to `githubauth.DefaultHost`, because a `&Client{}` built by struct literal carries no host. Converting the dispatch from the constant `case "github"` to a field comparison would inherit the hole rather than the defense: a zero-value client would fail the trusted branch and fall through to a raw-URL clone. Add one accessor, `(*Client) effectiveGitHubHost() string`, returning the default on empty, and route `canonicalHost`'s callers, **both** dispatch switches, and `ParseCloneTarget` through it. There is no Gitea counterpart to owe (S-P4 is moot under D1 — Gitea hosts are per-row, never a `Client` field).

**Non-constant switch cases (S-I2 sibling).** Both dispatches become `case c.effectiveGitHubHost():`. Go permits duplicate non-constant cases and silently takes the first in source order — no compile error. With the Gitea arm gone there is only one case, so the hazard is closed by construction rather than by a check; keep it that way, and do not reintroduce a second host-valued case without a construction-time invariant.

`Repo.Key()` (`project.go:170-175`) needs no edit — it composes `Host`, and both sides of the dedup route through `canonicalHost`, so they stay in step. The GHE collision closes as a side effect.

### 2. Gitea host derived from each row's URL — no config key

**Amended 2026-09-01 (D1, Cameron's ruling).** The plan as approved added a `[gitea].host` config section. It is not needed: every `tea repo ls --output tsv` row already carries the repo's full clone URL in its fourth column, and `parseRemoteURL` already extracts a hostname from exactly that form. Verified against the live server:

```text
cameron	agent-pool	source	ssh://git@git.sjo.lol:222/cameron/agent-pool.git
```

So `giteaList` runs each row's URL through `parseRemoteURL` and takes the hostname from there — ssh or https, whichever the server reports. Dropped as a consequence: the `[gitea]` section, `GiteaHostConfig`, `DefaultGiteaHost`, `githubauth.ResolveHostFor`, and `git.sjo.lol` as a constant in a public Go file (it moves out of the source entirely rather than to a different public file).

Two fail-closed rules replace the config-time collision check, both per-row and both in `giteaList`:

- A row whose URL yields no hostname is **skipped**, not stamped with a fallback. There is no configured default to fall back to, and a hostless row would key as `local:` with an empty path and collide with every other such row.
- A row whose derived hostname equals the configured GitHub host is **skipped**. This is the surviving half of the approved collision check: it is what stops a Gitea row from taking the `gh` dispatch branch and being cloned from github.com by owner/name.

The wing-name check narrows to the GitHub host alone, since Gitea hosts are now discovered per-row rather than configured.

Where the approved reasoning still holds: `internal/review/gitea.go:23`'s `reGiteaHost` remains the wrong guard (it permits uppercase and a trailing `:port`, either of which forks the `Key()` space and lands in a path segment). `canonicalHost`'s normalization is what host values pass through instead.

### 3. Wing placement from config; wing discovery from disk

These are two different jobs and the panel was right that conflating them re-arms the defect being fixed.

**Placement is config.** Where a *new* clone belongs is a judgment call and cannot be inferred:

```toml
[[projects.wings]]
name = "cadence-ecosystem"
repos = ["cameronsjo/cadence", "cameronsjo/forgectl"]
```

`repos` entries match `owner/name` case-insensitively. A wing `name` must pass the path-segment guard and the collision check (amended: against the GitHub host only, per D1). `ProjectsConfig.IsZero()` (`config.go:384`) becomes `len(pc.Owners) == 0 && len(pc.Wings) == 0` — a `[projects]` section carrying only wings must not report absent (`config_test.go:710` asserts on this).

forgectl's config is the steady-state home for this mapping. The `cadence:creating-wings` skill defines a `~/Projects/.wings/wings.tsv` keyed on bare repo name; neither it nor `~/.wings.conf` exists on disk, the 2026-08-04 reorg discarded them, and the key shapes deliberately differ. T8 files an issue to retire the tsv from the skill so the next session reading both does not see drift.

**Discovery is structural.** Readers classify a depth-1 directory as a wing when it directly contains at least one git repo — no config, so a wing missing from the table is still *listed*, it just is not a clone target. Verified against disk: the seven wings score 4–28 child repos; `github.com` and `git.sjo.lol` score **0** (their repos sit a level deeper); every residue directory scores 0; `pi-extensions` scores 0 and is correctly not a wing. `cadence-ecosystem` is both a repo and a wing and must be listed as both.

### 4. `Placement`, the single path formula

New pure function in `internal/projects/`:

```go
func Placement(root string, r Repo, wing string) (string, error)
```

`wing != ""` → `<root>/<wing>/<name>`; otherwise `<root>/<host>/<owner>/<name>`. It keeps `canonicalDest`'s `strings.ToLower` on every segment — dropping it would break the `Key()`-mirrors-the-tree invariant that Alternatives-declined leans on. It returns an **error** (amended per S-I3): the approved signature returned a bare `string`, which left the host-segment guard no way to "reject rather than fall back" except by panicking. `Worktree` must call it and fail **before** its `os.Mkdir` at `worktree.go:42`. It **promises a path, not a policy**: three callers with incompatible leaf semantics use it (`Clone` writes a checkout, `Worktree` writes `.bare` + per-branch and refuses an existing leaf, the probe reads), and none of that belongs in the formula.

**Segment guards, in order:** the host segment goes through a dedicated DNS-shape guard (`githubauth`'s `reHost` form) that rejects rather than falls back; owner and name keep `validPathSegment` (`projects.go:571-575`). Traversal is already covered by the latter — probed: `"."`, a hostname containing `/`, and empty all reject. What it does not cover, and what a hostname can now carry, is `:` (legal in an APFS filename, rendered as `/` in Finder), a leading dot (a host tree invisible to `ls`), an over-long name, and punycode homoglyphs. The DNS guard closes all four.

`canonicalDest` retires into it. Both current callers move over — `projects.go:523` (`Clone`) and `worktree.go:30` (`Worktree`).

**Who supplies `wing`.** A new `WithWings(table)` client option, mirroring `WithGitHubHost`, threaded at the CLI seam `internal/cli/projects.go:41-49`. Both `Clone` and `Worktree` look the repo up in the table on the `Client` — `Worktree(ctx, r, branch)` (`worktree.go:25`) has no flag and needs the table, or the claim that a wing member's worktree lands in the wing has no mechanism. `--wing <name>` on `projects clone` overrides the lookup. No match and no flag → host tree, which is estate rule 2 verbatim.

### 5. Duplicate-checkout guard

Before creating a destination, `Clone` probes the *other* candidate path (wing ↔ host tree) and reuses the existing `originMatches` (`projects.go:580-587`) rather than a bare `os.Stat` — a stat alone lets any same-named *different* repo suppress a legitimate clone, which is the exact case `originMatches` was written for. On a match: no-op, and `cloneOnly` prints the **existing** path to stdout, preserving the scriptable contract documented at `projects_clone.go:111-128`. On a same-path-different-origin hit the existing error path stands.

The 2026-08-04 reorg (`docs/lore/estate-ledger.md:9-24`) spent real effort resolving duplicate checkouts; this is the cheap way not to mint new ones.

### 6. Readers see wings

- `projects.go:252-283` `discoverDir` — the wing pass must run **before** the `isGitRepo(top)` shortcut at `:266-273`. `~/Projects/cadence-ecosystem` is itself a checkout, so it takes the flat branch today and its 28 members stay invisible — which is precisely the acceptance test. When the wing pass yields candidates, the wing directory is still appended as a flat project if it is itself a repo, and not otherwise.
- `projects.go:285-318` `discoverCanonicalHostCandidates` — one level under each detected wing. **Match rule, stated:** a child counts as a wing member when it carries a `.git` marker, matching this function's existing requirement.
- `resolve.go:186-234` `searchRoot` — the independent second layout implementation behind `forgectl surface launch` (`internal/cli/surface.go:140-141`) gets the same pass, with the **same `.git` requirement**, not its usual `isRealDir`. Its "do not walk below the known layouts" contract (`resolve.go:171-177`) holds: wings add one known layout, not unbounded depth.
- A repo checked out in *both* a wing and the host tree yields two matches, and `resolveByName` (`resolve.go:166-168`) turns that into `ErrTargetAmbiguous`. That is correct — a duplicate checkout is a real estate defect and should surface — but the error message must name both paths so it is actionable.

Wing members keep their normal `Key()` from origin, so they dedup against remote-list rows exactly as host-tree clones do.

### 7. Terminal safety

`renderRepoTable` (`internal/cli/projects_list.go`) writes `r.Host` with bare `fmt.Fprintf`, and `hostBadge` passes it into the picker's `DisplayLine`. Today a raw remote-derived host reaches those only for unrecognized hosts; after this change it is every row. The asymmetry is clearly unintended — the same file already wraps the *configured* host in `termsafe.SafeLine`, and `Inventory` (`projects.go:452-459`) refuses to interpolate server text into notes for this reason. Wrap the HOST column and the badge pass-through in the same PR.

## Alternatives declined

- **Keep the tokens, map token → hostname only inside the path formula.** Smaller diff, but leaves two host vocabularies in one package, makes the `Key()`-mirrors-the-tree invariant false, and leaves the GHE dedup collision open since `Key()` would still say `github`.
- **`--wing` flag with no config table.** Zero new schema, but every clone of a wing member needs the flag remembered; the mapping is stable and belongs in config.
- **Infer wing membership from disk for placement.** A wing tells you what already lives there, not where a *new* repo belongs; it would guess silently and wrongly. (Structural detection is adopted for *discovery*, where disk state genuinely is the authority.)
- **Gate discovery on the wing table too.** One mechanism, more explicit — but a wing missing from config would then be invisible to `projects list`, converting the defect being fixed into a config-drift defect with the identical symptom. Declined on the panel's finding.
- **forgectl reads `~/Projects/.wings/wings.tsv`.** One manifest for the estate, but the file does not exist, is keyed on bare repo name with no owner, and would need hand-maintenance. Cameron ruled for the forgectl table.
- **Reuse `[review.gitea].host`.** Couples projects placement to review configuration; same server today, need not be forever. Independent keys now, unification issue filed.

## Panel

Panel: cadence:plan-reviewer, cadence:security-posture-reviewer, artificer-voice:cameron-review ran — 24 findings, 22 folded in, 2 declined.

### Panel findings declined

- **"`Panel: pending` and a missing findings-declined stanza"** (plan-reviewer) — an artifact of reviewing the pre-panel draft, not a defect in the plan. Both stanzas are settled here.
- **"Split `[gitea]` vs `[review.gitea]` unification into this PR"** (cameron-review, raised as a question) — declined for scope. The two keys are independently valid, the collision check makes a bad pairing fail closed, and T8 files the unification issue.

## Orchestrator

**Driver:** opus

Security rationale (Usage Principle U9): `canonicalHost` is the control that stops a hostname like `evil-github.com.attacker.net` being stamped as trusted inventory, and its output now becomes a filesystem path segment, a dedup identity, and the predicate on two clone-dispatch switches. The anti-spoofing property survives, but every *consumer* of it changes — which is where both HIGH findings lived. Not Sonnet-drivable.

The control's file set has never had a structured security signal: the exact-match fix landed in PR #414 with one `COMMENTED` review by its own author and generic CI only.

Execution is serial — every task lands in `internal/projects/`, so there are no disjoint slices to fan out.

## Tasks

- [x] **T1 — Worktree entry.** forgectl is branch-mode: `git -C forgectl worktree add .claude/worktrees/<slug> -b feat/hostname-and-wing-placement origin/main`, `push -u`, draft PR. Persist this plan to `forgectl/docs/plans/2026-09-01-clone-hostname-and-wing-placement.md` as the first commit.
- [x] **T2 — Config.** ~~`GiteaHostConfig` for `[gitea].host`~~ (dropped, D1) and `WingConfig` on `ProjectsConfig`. `githubauth.ValidHostSegment` is the reusable path-segment guard, bounded at 253 not 256 (S-I6). `projects.ResolveWings` fails closed on an unsafe name, a name equal to the GitHub host, a repeated name, and a doubly-claimed repo. `IsZero()` counts wings. Tests split across `internal/config/config_test.go` (decode + presence) and `internal/projects/wings_test.go` (resolution + collisions) — the resolver cannot live in `internal/config`, which cannot import `githubauth` (config ← pr ← githubauth is a cycle).
- [x] **T3 — Security review, pre-implementation.** Dispatch `cadence-forge:security-reviewer` (Opus-tier; if the type does not resolve, a direct Opus reviewer — never Sonnet, never contingent on a plugin) over the control's file set: `internal/projects/{project,projects,worktree,gitea,github,resolve}.go`, `internal/githubauth/runner.go`, `internal/cli/{projects,projects_list,projects_clone}.go`, `internal/config/config.go`. Fold findings before T4.
- [x] **T4 — Host vocabulary.** `effectiveGitHubHost` accessor; `canonicalHost` drops the Gitea arm, returns `bare`, and strips a trailing `.` (S-P3); `ParseCloneTarget`; **both** dispatch switches; `github.go` row stamp; `gitea.go` per-row host derivation with its two skip rules (D1); row-level owner/name validation (S-I2); `hostBadge`; `Inventory` labels; the `--host` closed allowlist and its two help strings; the `Repo.Host` doc comment. Tests: `TestCanonicalHost` pinning that `evil-github.com.attacker.net`, **`github`, `GITHUB`, `gitea`, and `git.sjo.lol.`** all map to their own hostname (S-C1 — the bare-token rows are the Critical, and the approved test list would not have caught it); a dispatch test proving `Repo{Host:"github"}` no longer reaches `gh`; `TestParseCloneTarget*`; `TestParseRemoteURL_GHEHost`; the IPv6-literal case (S-P1); a zero-value `Client` still dispatching GitHub through `gh`; and a GitHub `Repo` reaching `cloneBareRepo` in `Worktree`.
- [x] **T5 — `Placement`.** Introduce the function returning `(string, error)` (S-I3), with its ToLower and its two-tier segment guards, plus a `defer`ed cleanup of `base` on every `Worktree` error path after `os.Mkdir` (S-I4 — today one transient failure permanently bricks that repo's worktree path), retire `canonicalDest`, move `Clone` and `Worktree` onto it, add `WithWings` and the table lookup, add `--wing`. Update `TestCanonicalDest_*` → `TestPlacement_*`, the `TestClone_*` dest assertions, and `worktree_test.go`.
- [x] **T6 — Duplicate-checkout guard.** The cross-tree probe via `originMatches`, its no-op return, and the `cloneOnly` stdout contract. Split from T5 deliberately: T5 is a mechanical refactor, this is the only new filesystem behavior and the likeliest thing to be wrong.
- [x] **T7 — Readers and `--dry-run`.** Structural wing detection ahead of the `isGitRepo(top)` shortcut in `discoverDir`; the wing pass in `discoverCanonicalHostCandidates` and `searchRoot` with the `.git` match rule; the both-paths ambiguity message in `resolveByName`. Add `--dry-run` to `projects clone` (resolve, print the destination, exit). Update the discovery tests, `resolve_test.go` including a depth-contract case, and `projects_clone_test.go`.
- [x] **T8 — Terminal safety and docs.** `termsafe.SafeLine` on the HOST column, the badge pass-through, **and the five further unwrapped sites S-I5 names** (`projects_list.go:155` REPO column, `projects_clone.go:101,117,121`, `projects_pick.go:177`, `projects_worktree.go:78` — owner and name are as server-supplied as host, and are unwrapped today). Also stop interpolating `r.Host/Owner/Name` into the error strings at `projects.go:521,533`, `worktree.go:28,44` — at two of those the values are the ones that just failed validation. `docs/commands/projects-and-review.md` (layout section and the flip-host note at `:79-80`), `docs/configuration.md:41`, and the breaking `--json` `host` wire change recorded in the PR body's `BEGIN_COMMIT_OVERRIDE` block — **amended (D3):** this repo generates `CHANGELOG.md` with Release Please and ships `TestChangelogHasNoUnreleasedLedger` forbidding an `[Unreleased]` section (`AGENTS.md`), so the approved plan's changelog instruction was wrong for this repo. File one issue on `cameronsjo/forgectl`: retiring `wings.tsv` from the `cadence:creating-wings` skill. (The `[gitea]`/`[review.gitea]` unification issue is moot under D1 — `[gitea]` never exists.)
- [x] **T9 — Polish and ship.** `cadence-forge:polish`, a second security pass over the same file set, then ready-flip the PR.

## Verification

Run from the worktree.

1. `go vet ./... && go test ./...` — the whole suite; `internal/projects`, `internal/config`, and `internal/cli` carry the new cases.
2. **Path formula, no network:** `PROJECTS_DIR=$(mktemp -d) forgectl projects clone --dry-run cameronsjo/quickmd` → `<tmp>/github.com/cameronsjo/quickmd`. Full hostname, no shortening.
3. **Wing routing:** the same command with `--wing testwing` → `<tmp>/testwing/quickmd`. Table-driven routing is asserted in `TestPlacement_*` rather than the CLI, since `ConfigPath()` (`config.go:877`) is derived and not flag-overridable — config-file cases use `redirectConfigDir`, the existing test helper.
4. **GHE separation (unit):** with the GitHub host set to `github.work-domain.com`, the destination is `<tmp>/github.work-domain.com/...`, and that repo's `Key()` differs from the github.com repo of the same owner/name.
5. **Spoof guard still closed:** `TestCanonicalHost` maps `evil-github.com.attacker.net` to its own hostname, never to the configured host. Necessary but not sufficient — the consumer tests in T4 are what cover where the risk actually moved.
6. **Namespace collision fails closed:** a config with `[gitea].host = "github.com"` errors at load, and so does a wing named `github.com`.
7. **Real inventory — the acceptance test:** `forgectl projects list` on this machine includes wing members. Today it misses all 69 across `artificer`, `mcp`, `obsidian`, `smart-home`, `lord-huron`, `github.io`, and `cadence-ecosystem`; after, it lists them, with **no `[[projects.wings]]` configured**, since discovery is structural. This is also what `cameronsjo/forgectl#412` (already merged and closed) asked for on the live-accept desk item — record the result there. Write it as "satisfies the check recorded on #412" in any PR body or commit: GitHub's closing parser has no negation handling and reads commit messages too.
8. **No duplicate checkout:** with `~/Projects/github.com/cameronsjo/quickmd` present, a clone routing to a wing no-ops and prints the existing path; a *different* repo of the same name at that path does **not** suppress the clone.

## Deviations


- **D1 — `[gitea].host` dropped; the Gitea host is derived per-row from the URL.** (2026-09-01, Cameron's ruling, mid-T2.) Prompted by the question "why does gitea need to get treated separately? it speaks git too." It does not: `cloneFromGitea` is already a plain `git clone`, and after §1 the dispatch collapses to "configured GitHub host → `gh` (credentials + `GH_HOST` pin); everything else → clone the URL". The only thing the config key bought was a hostname to stamp on `tea repo ls` rows — and every row already carries its clone URL, verified against the live server. Cameron's ruling covers both URL forms: ssh or https, whichever the server reports. **Net:** one config section, one type, one constant, one `githubauth` function, and one public-source hostname all removed; the collision check narrows from a config-time host-vs-host comparison to two per-row skip rules in `giteaList`, which is strictly closer to the data.
- **D3 — no `[Unreleased]` CHANGELOG section.** The plan (and the standing changelog rule) called for one. forgectl generates `CHANGELOG.md` with Release Please and ships a test forbidding the section; `AGENTS.md` routes consumer prose to a `BEGIN_COMMIT_OVERRIDE` block in the PR body. Caught by the suite going red on a docs-only commit. The entry's content moved to the PR body.
- **D2 — T2's tests split across two packages.** The plan put them all in `internal/config/config_test.go`. `internal/config` cannot import `internal/githubauth` (config ← pr ← githubauth is an import cycle), so the wing resolver lives in `internal/projects` and its tests with it; `config_test.go` keeps decode and presence.

## Learnings

- **The pre-implementation security review paid for itself before a line of §1 shipped.** It found a live Critical on `main` unrelated to any planned change (S-C1: `canonicalHost`'s default arm returns into the same value space as the tokens, so a remote hostname of literally `github` mints the trusted token and clones an attacker-chosen repo from github.com). The approved plan's own test list — `evil-github.com.attacker.net` only — would have shipped the fix without pinning it, leaving it revertible by a later refactor with a green suite.
- **Three of the plan's stated rationales did not survive contact with the code** (S-P1 the `:port` claim, S-P2 the `Key()`-mirrors-the-tree claim, S-P3 the trailing-dot FQDN). All three were *arguments for a change that is still correct* — but two of them would have produced tests that could not go red. A rationale is a claim about the code, and reviewing the plan is not the same as reviewing the code the plan describes.
