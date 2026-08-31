# forgectl projects / review — whose repos get enumerated

> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).

```sh
forgectl projects list [query]           # list all projects: local clones + your GitHub.com repos + git.sjo.lol/cameron
forgectl projects list --json            # machine-readable JSON (safe to pipe; degradation notes go to stderr)
forgectl projects list --host github     # filter to one host: github | gitea
forgectl projects list --host gitea forge  # host filter + name substring
forgectl projects pick [query]           # picker with both descriptors TTY; otherwise sanitized candidates on stdout + exit 1 (aliases: p, open)
forgectl projects                        # shorthand for pick; same headless candidate/exit-1 contract
forgectl projects clone [query]          # picker with both descriptors TTY; otherwise candidates + exit 1 (use sshUrl from list --json)
forgectl projects worktree <query> [branch] # same ambiguity contract as clone; use sshUrl from list --json

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

`projects` builds a unified inventory across local clones, GitHub, and the self-hosted Gitea. A project that isn't checked out locally shows as `[uncloned]`; picking it clones from the right host before opening the tmux session. `list --json` emits structured records to stdout — degradation notes (e.g. a host that's unreachable) go to stderr so the pipe stays clean.

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
  no migration tooling.

The pin covers the projects/review inventory path only — `branch`, `pr`, and
`doctor` gh calls remain unpinned and github.com-shaped
([forgectl#413](https://github.com/cameronsjo/forgectl/issues/413)). The Gitea
source is unchanged and keeps its existing `git.sjo.lol` assumptions — forgectl
does not claim arbitrary-Gitea portability either (same follow-up).

If `forgectl init` already wrote an active `owners = ["…"]` into your
config.toml, it stays exactly as written: init never rewrites a section that is
already present. Delete or comment the line to move to the authenticated default.
