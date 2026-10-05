# forgectl projects / review — whose repos get enumerated

> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).

```sh
forgectl projects list [query]           # list all projects: local clones + your GitHub repos + your Gitea repos
forgectl projects list --json            # machine-readable JSON (safe to pipe; degradation notes go to stderr)
forgectl projects list --json --strict   # same, but exit 1 when any host degraded (output still written)
forgectl projects list --host github.com   # filter to one hostname (or "local")
forgectl projects list --host git.example.com forge  # host filter + name substring
forgectl projects pick [query]           # picker with both descriptors TTY; otherwise sanitized candidates on stdout + exit 1 (aliases: p, open)
forgectl projects                        # shorthand for pick; same headless candidate/exit-1 contract
forgectl projects clone [query]          # picker with both descriptors TTY; otherwise candidates + exit 1 (use sshUrl from list --json)
forgectl projects worktree <query> [branch] # same ambiguity contract as clone; use sshUrl from list --json
forgectl projects pull-all --json          # [{"name","status"}]; exits 1 when any pull failed, as without --json
forgectl projects clone --dry-run <target>  # print where it would land and exit, touching nothing
forgectl projects clone --wing mcp <target> # override the wing table for this one clone

forgectl review                          # unified table (reviewed rows dimmed)
forgectl review --kind issue             # issues only (or: pr)
forgectl review mark owner/repo#42       # mark an item reviewed
```

When both stdin and stdout are terminals, these selectors keep their existing
pickers. With either descriptor non-TTY, `projects`/`projects pick` emit one
sanitized display identity per candidate on stdout and exit 1; narrow pick to a
unique project name when possible or inspect `projects list --json`. Ambiguous
`projects clone` and `projects worktree` use the same rows and exit 1; obtain a
candidate's `sshUrl` from `projects list --json` for an exact target, or rerun
interactively when it has none. Project display rows are not universal command
arguments.

`projects` builds a unified inventory across local clones, GitHub, and whichever Gitea instance `tea` is logged into. A project that isn't checked out locally shows as `[uncloned]`; picking it clones from the right host before opening the tmux session. `list --json` emits structured records to stdout — degradation notes (e.g. a host that's unreachable) go to stderr so the pipe stays clean. A degraded host still exits 0 by default, so a partial inventory reads the same as a small account to a script that only checks the exit code; pass `--strict` to exit 1 whenever any host produced a degradation note. The records that did load are written to stdout first either way. A machine with no `tea` binary has no Gitea source set up, which is not a degradation: it adds no note and does not trip `--strict`. A `tea` that runs and fails still does.

## On-disk layout

Three filing rules, and `projects clone` writes the first that applies:

```text
<projects>/<wing>/<repo>          # repos in a configured wing
<projects>/<host>/<owner>/<repo>  # every other repo with a resolvable remote
<projects>/<repo>                 # legacy flat clones (read-only affordance)
```

`<host>` is the **full hostname** — `github.com`, `git.example.com`,
`github.example.com` — never a short token. Every segment is lowercased, so the
tree mirrors a repo's dedup identity exactly.

A **wing** is a directory directly under the projects root holding repos that
belong together, one level shallower than the host tree. Placement and
discovery answer different questions and come from different places:

- **Placement is configured.** Where a *new* clone belongs is a judgment about
  how you group work, and disk state cannot answer it. List the repos in
  `[[projects.wings]]`; anything unlisted lands in the host tree.
- **Discovery is structural.** Any depth-1 directory holding at least one git
  repo is walked as a wing, table or no table — so a wing you have not
  configured is still listed, it just is not a clone target.

```toml
[[projects.wings]]
name  = "cadence-ecosystem"      # a directory directly under the projects root
repos = ["cameronsjo/cadence"]   # "owner/name", matched case-insensitively
```

A wing name is validated against the same charset a hostname path segment is,
and one that collides with the configured `[github]` host fails config load —
a wing and a host tree cannot be the same directory.

`clone` will not mint a duplicate: if the repo is already checked out under the
*other* rule, it prints that path and clones nothing. The check compares the
existing checkout's origin, so a same-named but different repo does not
suppress a legitimate clone. Use `--dry-run` to see where a target would land.

Both inventories are scoped to GitHub accounts you name, and both default to the
account `gh` is already authenticated as:

```toml
[projects]
# owners = ["your-login"] # gh repo list scope; unset or [] = authenticated GitHub.com login
[review]
# owners = ["your-login"] # gh search --owner scope; unset or [] = authenticated GitHub.com login
```

The two lists are **independent**. Neither inherits from the other — which repos
you jump between and which repos you triage work in are different questions, and
a shared list would force one answer on both. Set one, the other, both, or
neither.

Leaving a list unset (or writing `owners = []`) resolves the authenticated login
once per run, so the same binary follows whichever account you are logged in as
on that machine. A configured list is authoritative and makes no discovery call
at all. Owner values are validated against GitHub's owner charset, capped in
count and length, and deduplicated case-insensitively before any of them becomes
a `gh` argument; a malformed entry anywhere in the list means zero queries rather
than a quietly narrowed inventory.

**Every GitHub call on the projects/review inventory path is pinned**, whatever
`GH_HOST` says in the surrounding shell — an ambient host is overridden rather
than queried, because listing another instance's repos and labeling them
`github` would put wrong data in the inventory. The pinned value is
per-deployment: it defaults to `github.com`, and one config line points the
whole deployment (projects **and** review — one host, so clones and review keys
can never disagree) at a GitHub Enterprise instance instead:

```toml
[github]
host = "github.example.com" # lowercase hostname; no port, no scheme
```

Two prerequisites and one consequence:

- **`gh auth login --hostname <host>` must have been run first.** On any
  non-default host, forgectl scrubs `GH_TOKEN`, `GITHUB_TOKEN`,
  `GH_ENTERPRISE_TOKEN`, and `GITHUB_ENTERPRISE_TOKEN` from every gh
  subprocess — a config line must not be able to redirect an ambient
  credential to a host of its choosing — so gh's stored credential for that
  hostname is the only one that works.
- The host is validated (lowercase DNS name, no port, no scheme, no path); an
  invalid value or an unreadable config file fails both command trees loudly
  instead of silently falling back to github.com. A GHE host served on a
  nonstandard port is not configurable.
- Flipping the host later leaves the old host's reviewed marks inert (never
  pruned, never re-verified) and its clones as unmatched local dirs; there is
  no migration tooling. The clones themselves stay put and stay distinct — a
  GitHub Enterprise host files under its own hostname, so it no longer shares
  a directory tree or a dedup identity with a github.com repo of the same
  owner and name.

Every `gh` call names its host on purpose
([forgectl#413](https://github.com/cameronsjo/forgectl/issues/413)):

- **Host-scoped calls are pinned to `[github] host`.** A call with no
  repository behind it has only configuration to name its host: the
  projects/review inventory, the `@me` searches behind `pr prs` and `pr dash`,
  and `doctor`'s `gh auth status --hostname <host>`.
- **PR-scoped calls name the PR's own host.** Viewing and posting a review
  pass `--repo HOST/OWNER/REPO` and run pinned to that same host, token
  removal included; the clone is a plain `git clone` of `https://HOST/…`, so
  git's credential setup for that host applies to it. gh resolves a
  two-part `--repo` against `GH_HOST` or its default host, never the
  checkout, so the host is never left to it. The PR's host is the configured
  `[github] host` for a typed `owner/repo#N` and for a row from `pr prs`,
  `pr dash`, or `pr pick`; the URL's host for a pasted PR URL; and the
  checkout remote's host for a bare `N`. A session record stores that host,
  so changing `[github] host` later does not move an existing session; a
  record written before records carried a host means the configured
  `[github] host`.
- **Checkout-resolved calls stay ambient.** `gh repo view` (resolving a bare
  `N`) and `branch`'s `gh pr list` take their repository from the checkout's
  git remotes, and gh filters those remotes by `GH_HOST` with no fallback, so
  pinning them would break every checkout whose remote is on another host.
  `branch`'s post-delete verification (`gh api`, which never infers a host)
  passes `--hostname` for the host in `git remote get-url <remote>`.

The Gitea source no longer assumes a hostname. `tea repo ls` reports each repo's
own clone URL, so every row's host is read from that URL rather than configured
— which means any Gitea instance `tea` is logged into files correctly, with no
config key. Two rows are dropped rather than filed: one whose URL yields no
hostname, and one whose hostname equals the configured GitHub host (which would
otherwise be fetched from GitHub by the server's own owner/repo strings).

If `forgectl init` already wrote an active `owners = ["…"]` into your
config.toml, it stays exactly as written: init never rewrites a section that is
already present. Delete or comment the line to move to the authenticated default.

## Release radar: `forgectl review releases`

`review releases` reads the release-rhythm registry (`docs/release-rhythm.yaml`
in cadence-ecosystem, ADR-0044 there) and asks GitHub, read-only, where each
`release-pr`, `testflight`, and `manual-cut` repo stands. `continuous` and
`dormant` repos are listed in a one-line footer.

```sh
forgectl review releases                         # table
forgectl review releases --json                  # machine-readable report
forgectl review releases --json --fail-on-stall  # exit 1 on any stalled or unknown repo
```

| Column | Source |
|---|---|
| last release | newest non-draft, non-prerelease release whose tag has the registry's `tag_pattern`; for `testflight`, the newest `testflight-upload` record (or `tf-*` tag) |
| unreleased | commits on the branch since that release (compare API) |
| release PR | open same-repo PR whose title passes the ship gate's title check |
| last ship | newest entrypoint run: age, conclusion, and the gate's `reason` from its `ship-gate <reason>:` annotation (testflight: `paused`, `uploaded`, `no-change`, `failed`) |
| human gates | count, and for `manual-cut` how long a cut has been due: the oldest uncut commit or upstream commit, never earlier than the last cut. App Store release has no age visible from GitHub |
| endpoints | the `version` in each tap formula or cask, or Scoop manifest, against the last release |
| gate copy | sha256 of `.github/scripts/ship-gate.sh` on the branch against the canonical copy |

A repo is **stalled** when:

- its toggle (`SHIP_NIGHTLY` for `release-pr`, `TESTFLIGHT_NIGHTLY` for
  `testflight`) is `on` and the entrypoint has not run in 26h;
- its toggle is `on` and its last 2 scheduled runs report the same reason other
  than `go`, `no-pr`, or `paused` (`uploaded` and `no-change` for testflight);
- its last run reports `half-shipped`;
- an endpoint still trails a release more than 24h old;
- its gate copy is missing or its hash differs from the canonical one;
- `no-release-pr` (`release-pr` repos, toggle on or off): the unreleased commits
  include a releasable one, no release PR is open, and the oldest releasable
  commit is more than 24h old; or
- `release-workflow-stuck` (`release-pr` repos, toggle on or off): a run of the
  release-PR workflow has been `waiting`, `queued`, or `pending` for more than 1h.

Releasable means a commit whose subject type is `feat`, `fix`, or `perf`, whose
type or scope carries a `!` (`feat!:`, `refactor(api)!:`), or whose body has a
`BREAKING CHANGE:` footer. A set of only `chore`, `ci`, `docs`, `test`,
`refactor`, `style`, or `build` commits never stalls. The release-PR workflow is
the registry entry's optional `release_workflow` (a path under
`.github/workflows/`), read-only; without it the radar reads `release-please.yml`,
or `prepare-release.yml` for `cadence-hooks`. A default workflow the repo lacks
is skipped; one the registry names must exist. When the compare read lists fewer
commits than the branch is ahead by (100 per read) and none of them is
releasable, the newer ones are unseen and the row is `unknown`.

A repo whose toggle is not `on` is `paused`: shown, not judged on its beat. A
repo with any failed read is `unknown`, and the failed read is named (for
example `unknown forgectl: runs: HTTP 403`). Missing data counts as a failed
read: no release matching the registry's `tag_pattern` (or, for `testflight`,
no unexpired upload record or `tf-*` tag), or a finished ship run whose `Gate`
job left no reason. `--fail-on-stall` exits 1 on any `stalled` or `unknown`
row, so a read failure never passes as healthy.

Inputs:

- Registry: `--registry`, else `$FORGECTL_RELEASE_REGISTRY`, else
  `~/Projects/cadence-ecosystem/docs/release-rhythm.yaml`. A registry over
  256 KiB, with a repeated or merge (`<<`) key, or with a mapping of more
  than 64 keys is refused.
- Canonical gate hash: `--gate-sha256`, else `$FORGECTL_GATE_SHA256`, else the
  sha256 of `../scripts/release/ship-gate.sh` beside the registry. With none of
  those, every `release-pr` row reads `unknown`.
- Auth: gh's own (`GH_TOKEN`, `GITHUB_TOKEN`, or its stored login), pinned to
  `[github] host`. The token needs read access to contents, pull requests,
  actions, and checks (the gate's annotations) on every tracked repo, and to
  upstream repos. Actions variables read is optional: when the token cannot
  list variables (a GitHub App token minted by `create-github-app-token`
  cannot be granted it), the toggle is inferred from the newest scheduled
  run, where the gate reports `paused` and a paused `testflight.yml` skips its
  job; the JSON report's `toggle.source` says which (`variable` or `runs`).
  With no scheduled run to read, the row is `unknown`.
