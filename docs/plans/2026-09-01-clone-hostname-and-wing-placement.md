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
- Passing a host to `tea`. `giteaList` (`gitea.go:22`) runs `tea repo ls` with no host argument and lets tea resolve its own login; `[gitea].host` is used for **attribution and path only** and never reaches an argv.
- Unifying `[gitea].host` with the existing `[review.gitea].host`. Follow-up issue.

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

`Repo.Host` becomes the normalized hostname. `canonicalHost(hostname, gitHubHost, giteaHost)` returns the configured GitHub host on an exact match, the configured Gitea host on an exact match, and otherwise the **normalized** `bare` value it already computes — lowercased, one trailing `:port` stripped.

Returning `bare` rather than today's raw `hostname` (`project.go:302`) is a deliberate behavior change with two effects worth claiming: it preserves the `Key()`-mirrors-the-tree invariant that `canonicalDest`'s comment (`projects.go:556-558`) names, which a raw `GitHub.Example.com` would falsify; and it closes a latent bug where a ported unknown remote mints a `~/Projects/gh.example.com:8443/` directory, since `validPathSegment` does not reject `:`.

The exact-match anti-spoofing property documented at `project.go:271-280` — the reason `evil-github.com.attacker.net` is not stamped as trusted inventory — is preserved unchanged. It is why this work is security-gated.

Touched sites:

| Site | Change |
| --- | --- |
| `project.go:281-304` `canonicalHost` | take `giteaHost`; return normalized hostnames |
| `project.go:187` `parseRemoteURL` | take `giteaHost` and thread it; its three callers — `localRepos` (`projects.go:376`), `originMatches` (`projects.go:585`), `ParseCloneTarget` (`project.go:245`) — all change signature or call |
| `project.go:244-256` `ParseCloneTarget` | bare `owner/repo` shorthand stamps the effective GitHub host; the SSHURL branch compares against it |
| `projects.go:539-551` `Clone` dispatch | `case "github"` → compare against the effective GitHub host |
| **`worktree.go:49-61` second dispatch** | the same switch, in the bare-worktree path — **both** panel seats caught this independently; left alone, every GitHub worktree falls to `cloneBareFromURL` with a server-supplied URL, dropping the `GH_HOST` pin and the non-default-host token scrub that `github.go:118-126` puts *inside* `cloneBareRepo` "where no future caller can forget it" — and a shorthand target (`SSHURL == ""`) dies outright |
| `github.go:107`, `gitea.go:44` | remote-list rows stamp the configured hostname |
| `project.go:127-139` `hostBadge` | **retire the mapping** — display the hostname itself, `local` for empty. It is a `Repo` method with no access to configured hosts, so any mapping must hardcode `github.com` and silently drops a GHE host's badge. Widen `DisplayLine`'s `%-12s` to fit a real hostname |
| `projects.go:422-424,448` `Inventory` | host labels and fold order still say `github`/`gitea`; `projects_test.go:172` asserts on it |
| `internal/cli/projects_list.go:38,44-45,86,92-101` | `--host` stays a **closed allowlist** — the two resolved hosts plus `local`, error otherwise — and the help example and flag usage string both move off `github | gitea` |
| `project.go:30` | `Repo.Host`'s documented JSON value is the `projects list --json` wire contract; update it, and call the break out in the changelog |

**Zero-value defense.** `canonicalHost:287-289` already defaults an empty `gitHubHost` to `githubauth.DefaultHost`, because a `&Client{}` built by struct literal carries no host. Converting the dispatch from the constant `case "github"` to a field comparison would inherit the hole rather than the defense: a zero-value client would fail the trusted branch and fall through to a raw-URL clone. Add one accessor, `(*Client) effectiveGitHubHost() string`, returning the default on empty, and route `canonicalHost`'s callers, **both** dispatch switches, and `ParseCloneTarget` through it.

`Repo.Key()` (`project.go:170-175`) needs no edit — it composes `Host`, and both sides of the dedup route through `canonicalHost`, so they stay in step. The GHE collision closes as a side effect.

### 2. Gitea host into config

```toml
[gitea]
host = "git.sjo.lol"
```

New top-level section, defaulting to `git.sjo.lol` so nothing changes without a config file. This also removes a personal hostname currently hardcoded in a **public** Go file.

The type must **not** be named `GiteaConfig` — that name is taken by the `[review.gitea]` section (`config.go:461-475`). Name it `GiteaHostConfig`.

Validate with `githubauth.ResolveHost` (`internal/githubauth/runner.go`) — lowercase DNS labels, no port, no scheme, length-bounded, and errors that never render the rejected value. **Do not** substitute `internal/review/gitea.go:23`'s `reGiteaHost`, which permits uppercase and a trailing `:port`; either would fork the `Key()` space and land in a path segment. Its error text says "github host", so wrap it with the section name.

**Fail closed on a namespace collision.** Config load errors when the resolved GitHub host equals the resolved Gitea host, and when any wing name equals either. Today `"github"` and `"gitea"` are disjoint literals and no configuration can make a Gitea row take the GitHub dispatch branch; once both are config values, one line (`[gitea].host = "github.com"`) would stamp every `tea repo ls` row with the GitHub host and clone it via `gh` from server-supplied owner/name strings. The collision surface moves from impossible to one config line, so the check is what keeps it closed.

### 3. Wing placement from config; wing discovery from disk

These are two different jobs and the panel was right that conflating them re-arms the defect being fixed.

**Placement is config.** Where a *new* clone belongs is a judgment call and cannot be inferred:

```toml
[[projects.wings]]
name = "cadence-ecosystem"
repos = ["cameronsjo/cadence", "cameronsjo/forgectl"]
```

`repos` entries match `owner/name` case-insensitively. A wing `name` must pass the host-segment guard and the collision check above. `ProjectsConfig.IsZero()` (`config.go:384`) becomes `len(pc.Owners) == 0 && len(pc.Wings) == 0` — a `[projects]` section carrying only wings must not report absent (`config_test.go:710` asserts on this).

forgectl's config is the steady-state home for this mapping. The `cadence:creating-wings` skill defines a `~/Projects/.wings/wings.tsv` keyed on bare repo name; neither it nor `~/.wings.conf` exists on disk, the 2026-08-04 reorg discarded them, and the key shapes deliberately differ. T8 files an issue to retire the tsv from the skill so the next session reading both does not see drift.

**Discovery is structural.** Readers classify a depth-1 directory as a wing when it directly contains at least one git repo — no config, so a wing missing from the table is still *listed*, it just is not a clone target. Verified against disk: the seven wings score 4–28 child repos; `github.com` and `git.sjo.lol` score **0** (their repos sit a level deeper); every residue directory scores 0; `pi-extensions` scores 0 and is correctly not a wing. `cadence-ecosystem` is both a repo and a wing and must be listed as both.

### 4. `Placement`, the single path formula

New pure function in `internal/projects/`:

```go
func Placement(root string, r Repo, wing string) string
```

`wing != ""` → `<root>/<wing>/<name>`; otherwise `<root>/<host>/<owner>/<name>`. It keeps `canonicalDest`'s `strings.ToLower` on every segment — dropping it would break the `Key()`-mirrors-the-tree invariant that Alternatives-declined leans on. It **promises a path, not a policy**: three callers with incompatible leaf semantics use it (`Clone` writes a checkout, `Worktree` writes `.bare` + per-branch and refuses an existing leaf, the probe reads), and none of that belongs in the formula.

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

- [ ] **T1 — Worktree entry.** forgectl is branch-mode: `git -C forgectl worktree add .claude/worktrees/<slug> -b feat/hostname-and-wing-placement origin/main`, `push -u`, draft PR. Persist this plan to `forgectl/docs/plans/2026-09-01-clone-hostname-and-wing-placement.md` as the first commit.
- [ ] **T2 — Config.** `GiteaHostConfig` for `[gitea].host` and `WingConfig` on `ProjectsConfig`. Validate hosts via `githubauth.ResolveHost` (wrapped for the section name), wing names via the host-segment guard. Fail closed on GitHub-host == Gitea-host and on wing-name == either host. Update `IsZero()`. Tests in `internal/config/config_test.go`.
- [ ] **T3 — Security review, pre-implementation.** Dispatch `cadence-forge:security-reviewer` (Opus-tier; if the type does not resolve, a direct Opus reviewer — never Sonnet, never contingent on a plugin) over the control's file set: `internal/projects/{project,projects,worktree,gitea,github,resolve}.go`, `internal/githubauth/runner.go`, `internal/cli/{projects,projects_list,projects_clone}.go`, `internal/config/config.go`. Fold findings before T4.
- [ ] **T4 — Host vocabulary.** `effectiveGitHubHost` accessor; `canonicalHost` takes `giteaHost` and returns `bare`; `WithGiteaHost` option threaded from `internal/cli/projects.go`; `parseRemoteURL` + its three callers; `ParseCloneTarget`; **both** dispatch switches; row stamps; `hostBadge`; `Inventory` labels; the `--host` closed allowlist and its two help strings; the `Repo.Host` doc comment. Tests: `TestCanonicalHost` (including that `evil-github.com.attacker.net` still maps to its own hostname), `TestParseCloneTarget*`, `TestParseRemoteURL_GHEHost`, a zero-value `Client` still dispatching GitHub through `gh`, and a GitHub `Repo` reaching `cloneBareRepo` in `Worktree`.
- [ ] **T5 — `Placement`.** Introduce the function with its ToLower and its two-tier segment guards, retire `canonicalDest`, move `Clone` and `Worktree` onto it, add `WithWings` and the table lookup, add `--wing`. Update `TestCanonicalDest_*` → `TestPlacement_*`, the `TestClone_*` dest assertions, and `worktree_test.go`.
- [ ] **T6 — Duplicate-checkout guard.** The cross-tree probe via `originMatches`, its no-op return, and the `cloneOnly` stdout contract. Split from T5 deliberately: T5 is a mechanical refactor, this is the only new filesystem behavior and the likeliest thing to be wrong.
- [ ] **T7 — Readers and `--dry-run`.** Structural wing detection ahead of the `isGitRepo(top)` shortcut in `discoverDir`; the wing pass in `discoverCanonicalHostCandidates` and `searchRoot` with the `.git` match rule; the both-paths ambiguity message in `resolveByName`. Add `--dry-run` to `projects clone` (resolve, print the destination, exit). Update the discovery tests, `resolve_test.go` including a depth-contract case, and `projects_clone_test.go`.
- [ ] **T8 — Terminal safety and docs.** `termsafe.SafeLine` on the HOST column and the badge pass-through. `docs/commands/projects-and-review.md` (layout section and the flip-host note at `:79-80`), `docs/configuration.md:41`, and an `[Unreleased]` CHANGELOG entry flagging the `--json` `host` field as a breaking wire change. File two issues on `cameronsjo/forgectl`: the `[gitea]`/`[review.gitea]` unification, and retiring `wings.tsv` from the `cadence:creating-wings` skill.
- [ ] **T9 — Polish and ship.** `cadence-forge:polish`, a second security pass over the same file set, then ready-flip the PR.

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
