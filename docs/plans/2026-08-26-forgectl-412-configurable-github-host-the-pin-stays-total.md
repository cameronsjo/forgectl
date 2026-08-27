---
status: "in-flight"
updated: "2026-08-26"
branch: "main"
body_sha256: "8f29a221a5507ed19d76dee7f36df965335861d36638623132b9abe82716eb1a"
session: "amber-fugue"
session_id: "144ff0e1-ddfd-4eab-9854-0d60d2e73caa"
machine: "cf6e768835c7"
approved_in: "marble-spindle"
approved_session_id: "b19f0da1-4eb5-4366-b727-cb670ba211cd"
---

# forgectl#412 — configurable GitHub host (the pin stays total)

## Context

[forgectl#412](https://github.com/cameronsjo/forgectl/issues/412): every gh subprocess on the projects/review path runs through `internal/githubauth.Runner`, which force-sets `GH_HOST=github.com` (`const Host`, `internal/githubauth/runner.go:21`). That pin is a deliberate integrity control — an ambient `GH_HOST` must not silently redirect queries while rows get stamped as github.com data — but it makes a GitHub Enterprise deployment (Cameron's work machine) silently query public github.com: `projects list` reports a plausible-looking wrong inventory (1 remote-only repo instead of 128), `review` fails per-owner with categorical notes. forgectl#191 already accepted "second deployment against a different forge host" as a use case; the host is its unfinished half.

**Scope (Cameron-approved):** one configurable GitHub host per deployment. The pin stays total on the projects/review path — every gh subprocess there still gets `GH_HOST` force-set, just to the configured, validated value; ambient `GH_HOST` still loses. Full multi-source (`[[projects.sources]]`) is out of scope.

## Alternatives declined

- **Inherit ambient `GH_HOST`** (issue option 1) — reverses the documented githubauth integrity control; rows would again be stamped as data from a host nobody chose.
- **Full `[[projects.sources]]` multi-source** — handles two forges at once but pulls in config-derived host maps, per-host clone layout, N-source fold, marks-prune semantics; ~3–4× the work for a case no deployment has. The `[github]` section leaves room for a later `[[github.hosts]]`.
- **`[projects] host` + `[review] host` twin keys** — divergent keys would stamp clones and review keys with different hosts, exactly the mislabeling the pin prevents. One deployment-wide `[github] host`.

## Panel

Panel: security-posture-reviewer, plan-reviewer ran — 26 findings, 26 folded in, 0 declined

## Panel review — findings declined

none declined — all findings folded in

## Threat model (security seat, folded)

Making the pin's value config-steerable creates a **credential-redirect primitive** that the constant made impossible: `gh` sends `GH_ENTERPRISE_TOKEN` to whatever `GH_HOST` names, and sends `GH_TOKEN`/`GITHUB_TOKEN` to github.com *and any `*.ghe.com` subdomain*. One hostile line in the (operator-writable, agent-reachable) config file would exfiltrate a PAT. **Bound (mandatory):** whenever the resolved host ≠ `DefaultHost`, `pinnedRunner` scrubs `GH_TOKEN`, `GITHUB_TOKEN`, `GH_ENTERPRISE_TOKEN`, `GITHUB_ENTERPRISE_TOKEN` from the gh subprocess env (set to empty alongside the pin), forcing gh to its `hosts.yml` stored credential for that host — which makes the README's `gh auth login --hostname <host>` prerequisite load-bearing. Env-assertion tests: an ambient enterprise token never reaches a non-default-host gh call.

## Design

### Config (`internal/config/config.go`)

- New struct: `GithubConfig` with one field `Host string` (tag `toml:"host"`); `IsZero()`; member `Github GithubConfig` (tag `toml:"github"`) on `Config` (`config.go:87-106`). Empty/absent = `github.com`.
- **Bijection pin (`internal/cli/modules_test.go:225 TestModules_ConfigClaimsBijection`):** `[github]` is deliberately deployment-wide (spans projects + review), owned by no single module — add it to the test as a documented shared-section exception citing #412 (ADR-0005 ruling recorded in the test comment).
- Schema doc-comment block (`config.go:65-73` region); `initSections` roster row `{"github", "github", githubScaffold}` (`init_cmd.go:235`) + the commented scaffold constant — the roster row is what satisfies `TestInitSections_CoversEveryStructSection` (`init_cmd_test.go:64`).
- Verify `forgectl config` renders the new section (`TestConfig_PrintsEverySection`, `config_cmd_test.go:116`); update the renderer roster if the reflect walk alone doesn't cover it.
- Field-set pinning test mirroring the `PrConfig` SECURITY BOUNDARY precedent (`config.go:394-401` comment; test lives in the config test file).
- **Fail-open guard (security seat):** tolerant `Load()` returning a partially-decoded config must not silently default a GHE deployment back to github.com. Surface decode degradation to the module seams (e.g. a decode-degraded marker on the loaded config / `meta.IsDefined` via `DecodeStrict` at the seam) and refuse loudly via the config-error command tree when the file failed to decode. Test with a deliberately malformed config.

### githubauth (`internal/githubauth/`)

- `const Host` → `const DefaultHost = "github.com"`; package doc updated (pin total on this path; host per-deployment).
- `ResolveHost(configured string) (string, error)`: trim, empty → `DefaultHost`, lowercase, `MaxHostBytes` cap (mirror `MaxOwnerBytes`, `owners.go:21`), anchored hostname regex `^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$` — **no port**. Doc comment carries the load-bearing reason: excluding `:` and `/` is store-key integrity (`Item.Key()` embeds Host verbatim; URL-prefix comparisons) — and names the deliberate asymmetry with `reGiteaHost` (which allows ports). User-visible consequence documented: a ported GHE host is unconfigurable. Category-only errors; the unvalidated value is never rendered.
- `Runner(run exec.Runner, host string)`: `pinnedRunner` gains `host` (named-field discipline, all four methods explicit); pin sets `GH_HOST = p.host` last; **non-default host additionally scrubs the four token env vars** (threat model above). Fail-closed: a host failing the regex yields a runner whose every gh path returns `ErrUnpinnableHost` (mirrors `ErrUnpinnableGhPath`).
- `ResolveOwners(ctx, run, configured, host)`; `discoverLogin` wraps with the host. Double-wrap idempotent.

### Projects (`internal/projects/`)

- `Client` gains `gitHubHost` (default `DefaultHost`); `WithGitHubHost` option beside `WithGitHubOwners` (`internal/cli/projects.go:30`). Thread to the four githubauth sites (`github.go:36/:87/:129/:145`).
- Rows keep token `"github"`; clone layout stays `Dir/github/<owner>/<name>` — no migration.
- `canonicalHost(hostname, gitHubHost)` (`project.go:264`): exact match, case-insensitive, one trailing `:port` stripped from the *remote* hostname, against the configured host → `"github"`; `git.sjo.lol` → `"gitea"`; else raw. Fixes the live `strings.Contains` substring stamping bug (`evil-github.com.attacker.net` → `"github"` today). Ported-remote case gets a named test. Under GHE config a leftover github.com clone maps to raw token → unmatched local dir; deliberate, documented.
- **`ParseCloneTarget` ripple (both seats):** it calls `parseRemoteURL` from two CLI sites with no `Client` (`cli/projects_clone.go:48`, `cli/projects_worktree.go:42`) and hardcodes `Host: "github"` for bare `owner/repo` (`project.go:245`). Ruling: `ParseCloneTarget` gains a `gitHubHost` param; the shorthand resolves against the *configured* host (deployment-scoped meaning, documented in help); both CLI sites pass the resolved host.
- `internal/cli/projects.go`: `ResolveHost` at construction; invalid host or decode-degraded config → `newProjectsConfigErrorCmd` (mirror `newReviewConfigErrorCmd`'s *structure*, not its `%q` formatting — category-only), **with `applyAliases` applied** so `forgectl proj ls` reports the config error, not "unknown command". Non-default host: one stderr note in `projects list` (`github host: <host>`, termsafe, post-validation) — tested: absent on default host, absent from `--json` stdout.
- `hostBadge` and `--host github|gitea` filter tokens unchanged.

### Review (`internal/review/`, `internal/cli/review.go`)

- `GitHubHost` const becomes the default; effective host is instance data. `NewGitHub(run, owners, host)` stores it, wraps the runner, stamps items with it (`github.go:188`, `:277`).
- **`activeHosts` keeps its prepend, parameterized (plan seat):** `activeHosts(effectiveHost, srcs)` prepends the effective GitHub host explicitly instead of the const — no duck-typed `Host()` requirement on the GitHub source, fake-source tests (`cli/review_mark_test.go:72` prune test) stay meaningful, no `extraHosts` collapse.
- `newReviewCmdForSources` (the test seam) gains the effective-host param, threading it to `mark`/`unmark` (`cli/review_mark.go:18,38`) and `sync`.
- Work-ref parsing: parameterize the github-shaped regex by the effective host, **keeping `/pull/`-only strictness for GitHub-family hosts** (gitea hosts keep `pulls?` via `reHostWorkURL`) — no parser widening; default-host behavior stays byte-for-byte and is asserted. `hostAllowed` loses the github.com special case; under GHE config a literal `https://github.com/...` ref is rejected (deliberate: no never-prunable marks). `ParseWorkRef` (`review.go:112`) — the second caller — is updated to pass `DefaultHost` explicitly with its contract re-worded.
- Marks-prune statement (PR body + `review sync` doc): flipping `[github].host` leaves old-host marks inert. No migration tooling; documented, not accidental.

### Docs

- Scope the claim honestly (both seats): the pin covers **the projects/review inventory path**; `branch`/`pr`/`doctor` gh calls remain unpinned and github.com-shaped. Flip "GitHub Enterprise is not supported" at `README.md:82`, `:422-423`, `:571-577`; help text `projects_list.go:27-36`, `review.go:138-160`; config schema comment; init scaffold; githubauth package doc. README: `gh auth login --hostname <host>` prerequisite (load-bearing post-scrub).
- **`CHANGELOG.md` `[Unreleased]` entry rides this PR** (released binary; changelog-before-PR rule).

### Follow-up issue (filed BEFORE the docs step, cited by it)

One issue on cameronsjo/forgectl naming: unpinned gh callers (`internal/branch/branch.go:341,364,508`, `internal/pr/{ref.go:219,session.go:195,launch.go:548,search.go:68}`, `internal/doctor/doctor.go:195`, plus `pr/ref.go:246-248`'s own github-only URL parse); `NewGitea`'s `%q` invalid-host render; the hardcoded `git.sjo.lol` arm in `canonicalHost`; `--host`→`--source` rename aside.

## Tests

- Mechanical: existing pin assertions (`githubauth/runner_test.go:83+`, `projects/github_test.go:29`, `projects/projects_test.go:223`, `worktree_test.go:129`, `review/github_owners_test.go:93`, **`cli/review_mark_test.go`, `cli/review_list_test.go`**) become default-host cases of parameterized tables.
- New: `ResolveHost` table (empty→default, GHE host, lowercase, port/scheme/whitespace/oversize rejected, error leak test); `Runner` invalid-host fail-closed; configured-host pin beats ambient AND caller env; **token-scrub Env assertions** (enterprise + github tokens absent on non-default host; untouched on default); `GithubConfig` decode/IsZero/field-set pin/render/bijection-exception/init-roster; malformed-config loud refusal; `canonicalHost` table (substring attack RED, github.com-under-GHE, ported remote); `originMatches` round-trip; clone dest unchanged; `ParseCloneTarget` shorthand under GHE; projects seam loud-failure incl. aliased verbs; stderr host note (present/absent/json-clean); review item stamping; `activeHosts(effectiveHost, srcs)` ordering; work-ref parsing GHE cases + byte-for-byte default assertion; sync prune leaves old-host marks inert while fake-source prune test still goes red; leak tests at both module seams.

## Execution

Branch-mode repo → worktree off `origin/main` in `forgectl/.claude/worktrees/`, one PR, squash-merge, plain-text `Closes cameronsjo/forgectl#412`. Producer tuple in commit trailers + PR body. Conventional Commits. `cadence-forge:polish` to the `record-polish` marker before PR-ready.

0. **Security review of the existing control file set** (`githubauth/{runner,owners}.go`, `exec/exec.go`, `config/config.go`, `projects/{project,github,projects}.go`, `review/{github,gitea,review}.go`, `cli/{projects,review}.go`) — Opus-tier (`cadence-forge:security-reviewer`; fallback a fresh Opus subagent handed the file list, never "install the plugin"). The package has never had a review (PR #292: zero review objects). Findings feed steps 1–7.
1. githubauth: `DefaultHost`, `ResolveHost` (+`MaxHostBytes`), `ErrUnpinnableHost`, `Runner(run, host)` + token scrub + tests
2. owners.go host threading + tests
3. config: `GithubConfig`, bijection exception, init roster + scaffold, renderer check, decode-degraded surfacing + tests
4. projects: `canonicalHost` tightening, `WithGitHubHost`, `ParseCloneTarget` param, threading + tests
5. cli/projects: seam validation (aliases included), error cmd, list stderr note + tests
6. review: `NewGitHub` host, stamping, work-ref parsing parameterization, `hostAllowed`, `ParseWorkRef` + tests
7. cli/review: `newReviewCmdForSources` host param, `activeHosts(effectiveHost, srcs)`, mark/unmark/sync wiring + tests
8. CHANGELOG `[Unreleased]`; file the follow-up issue; docs pass (README, help, scaffold, package doc)
9. `gofmt` + `golangci-lint run` + `go test ./...` (exit codes captured); security-reviewer diff pass; polish; PR

## Verification

- `go test ./...` green in the worktree, lint clean.
- Fake-runner Env assertions prove: default pins github.com with tokens untouched; configured host pins that host with all four tokens scrubbed; ambient GH_HOST loses in both.
- No-config behavior byte-identical (existing tests' outcomes unchanged).
- Live acceptance is Cameron's next work visit: set `[github] host`, `gh auth login --hostname <host>` done, `projects list` shows the 242-repo inventory, `review` notes gone — recorded on #412 at close.

## Deviations

*(empty at approval)*

## Learnings

*(empty at approval)*

<!-- Execution log (amber-fugue, 2026-08-26) -->

**Execution state:** steps 0–9 done — step-0 security review ran (6 findings: 2 Critical folded in-branch, 4 residuals filed to forgectl#413), githubauth/config/projects/review/cli threaded and tested, CHANGELOG + docs landed, forgectl#413 filed. Remaining: review passes fold-in, PR.

## Deviations

- **`pulls`-form URLs stay accepted on the effective host.** The plan's "keep `/pull/`-only strictness for GitHub-family hosts" turned out to conflict with its own byte-for-byte default-host assertion: the generic any-host URL branch (`reHostWorkURL`, which allows `pulls?`) always accepted `https://github.com/o/r/pulls/N` via hostAllowed's github.com arm. Byte-for-byte won: the github-shaped regex itself stays `/pull/`-only, the generic branch keeps admitting the effective host, and the pre-change acceptance is pinned by test as deliberate.
- **Step-0 findings reshaped the runner beyond the plan text:** `Runner` collapses a same-host double wrap and fails closed on a host-conflicting one (finding 1 — nested pins invert precedence, decoupling the scrub from the host); `pinnedRunner` implements `RunStreaming`, refusing gh (finding 6 — the separate `StreamingRunner` interface was an open side).
- **`allowedHostSpelling` replaces the planned boolean `hostAllowed`:** case-folded matching had to return the configured spelling, or a mixed-case typed ref would mint a key disagreeing with the source's stamped items.
- **`canonicalHost` treats an empty gitHubHost as the default** — existing tests construct `Client` as a struct literal (no `New`), and a zero-value client must mean github.com, not "nothing matches github". The gitea arm also moved to the same exact-match (behavior-preserving under the port-strip).

## Learnings

- `TestRunDocsServe_*` shutdown assertions flake under parallel load on clean `main` too (`-count=2` reproduces; each passes in isolation) — noted on forgectl#413 so a red CI run has a name.
- The repo's CHANGELOG is release-please-generated (no standing `[Unreleased]` section); the entry was added manually per the changelog-before-PR rule and will fold into the next release heading.
- A raw-string cobra `Long` cannot carry backticks — the gh-auth prerequisite line cost one build break.

**Review passes (step 9):** code-review PASS (0 Critical/Important; 1 of 3 nits folded — stray import group; regex-per-call and stale test names declined). Security diff pass (Opus) clean — no Critical/Important; 3 of 4 nits folded (flag-proof config-error trees, decode-degraded marker on unlocatable ConfigPath, refusal of [github] host == [review.gitea] host), the port-strip-over-match nit accepted as the documented ported-remote tradeoff. Polish marker recorded (scope=code; security=ran on Opus). PR forgectl#414.
