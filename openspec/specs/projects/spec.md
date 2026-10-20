# projects Specification

## Purpose
`forgectl projects` builds one inventory of local clones, the configured GitHub host's repos, and the logged-in Gitea instance's repos, and lets the operator list, pick, clone, worktree, and pull them under a predictable on-disk layout.

## Requirements

### Requirement: Unified project inventory
`projects list [query]` SHALL list projects from three sources: local clones under the projects root (`PROJECTS_DIR`, read-only), the configured GitHub host, and the Gitea instance `tea` is logged into. Each row SHALL carry its full hostname and whether it is cloned; a project not checked out SHALL be marked uncloned. Aliases `l`, `ls`, `find` and the group alias `proj` SHALL work.

#### Scenario: Name filter
- **WHEN** the operator runs `forgectl projects list forge`
- **THEN** only projects whose name matches `forge` are listed

#### Scenario: Unknown host filter
- **WHEN** the operator passes `--host` with a hostname the inventory does not carry and that is not the configured GitHub host or `local`
- **THEN** the command fails naming the hosts the inventory does have, rather than printing an empty list

### Requirement: JSON output keeps stdout clean
`projects list --json` SHALL write rows (`host`, `owner`, `name`, `sshUrl`, `mirror`, `private`, `cloned`, `localPath`, `status`) to stdout as a bare array, `[]` when empty, and MUST send degradation notes to stderr only.

#### Scenario: A degraded host under --json
- **WHEN** one host is unreachable and `--json` is set
- **THEN** stdout holds only the JSON rows that loaded and the note goes to stderr

### Requirement: Degradation and strict mode
A degraded host SHALL still exit 0 by default. `--strict` MUST exit 1 whenever any host produced a degradation note, after writing the rows that did load. When no source can be read at all, `list`, `pick`, `clone` and `worktree` MUST exit 1 with `no project source could be read` after printing the notes, never reporting an empty account. A machine with no `tea` binary SHALL add no note.

#### Scenario: Strict with a degraded host
- **WHEN** GitHub fails and the operator runs `projects list --json --strict`
- **THEN** the loaded rows are written to stdout and the command exits 1

#### Scenario: Every source fails
- **WHEN** the projects root is missing, GitHub fails, and Gitea is absent
- **THEN** the command exits 1 with `no project source could be read`

### Requirement: Categorical error notes
GitHub degradation notes SHALL name a cause and fix where gh's exit status or output shows one (for example `gh is not signed in to github.com; run gh auth login`) and otherwise say `run forgectl doctor for the cause`. gh's own output text MUST NOT be printed.

#### Scenario: gh not signed in
- **WHEN** gh is not authenticated for the configured host
- **THEN** the note names the cause and `gh auth login` as the fix, with no gh stderr text

### Requirement: Bounded list output
`--limit N` SHALL keep the first N rows; `--limit 0` keeps every row. The human table MUST default to 100 rows and print a `showing N of M` hint on stderr when cut. With `--json`, no `--limit` keeps the bare array; any `--limit` SHALL emit `{truncated,total,shown,limit,hint,notes,items}` in that key order. `--fields` (with `--json`) SHALL keep only named row keys in order; an unknown name MUST exit 1 listing valid names.

#### Scenario: Bounded JSON
- **WHEN** the inventory has 120 rows and the operator runs `projects list --json --limit 50`
- **THEN** stdout is one object with `truncated: true`, `total: 120`, `shown: 50`, a `hint`, and 50 `items`

#### Scenario: Unknown field
- **WHEN** the operator passes `--fields host,bogus`
- **THEN** the command exits 1 and lists the valid field names

### Requirement: Headless selector contract
When stdin and stdout are both terminals, `projects`, `projects pick`, `clone` and `worktree` SHALL show an interactive picker. With either descriptor not a TTY, an ambiguous selection MUST print one sanitized display identity per candidate on stdout and exit 1 rather than guessing.

#### Scenario: Ambiguous pick in a pipe
- **WHEN** `forgectl projects pick app` matches several projects and stdout is not a terminal
- **THEN** each candidate is printed on stdout and the command exits 1

### Requirement: Clone placement layout
`projects clone` SHALL file a repo at `<projects>/<wing>/<repo>` when `[[projects.wings]]` lists it (or `--wing` overrides), otherwise at `<projects>/<host>/<owner>/<repo>` with the full hostname. Every segment MUST be lowercased, and a leading-dot repo name SHALL be filed under a `dot-` leaf. `--dry-run` SHALL print the target and touch nothing.

#### Scenario: Unlisted GitHub repo
- **WHEN** the operator clones `Acme/Widget` from github.com with no wing configured for it
- **THEN** it lands at `<projects>/github.com/acme/widget`

#### Scenario: Dry run
- **WHEN** the operator runs `projects clone --dry-run acme/widget`
- **THEN** the destination path is printed and nothing is cloned or created

### Requirement: No duplicate clones
`clone` MUST NOT create a second checkout of a repo already checked out under another placement rule: it SHALL print the existing path and clone nothing. The check SHALL compare the existing checkout's origin, so a same-named different repo does not block a clone. If a leading-dot repo and a real `dot-` repo would share a leaf, the second clone MUST be refused naming both.

#### Scenario: Already cloned in the flat layout
- **WHEN** the target repo already exists at `<projects>/<repo>` with a matching origin
- **THEN** clone prints that path and clones nothing

### Requirement: Wing discovery is structural
Any directory directly under the projects root that holds at least one git repo SHALL be walked as a wing for discovery, configured or not. A wing name MUST be validated as a hostname path segment, and a wing colliding with the configured GitHub host MUST fail config load.

#### Scenario: Unconfigured wing
- **WHEN** `<projects>/tools/cli` is a git repo and `tools` is not in the wings table
- **THEN** `projects list` includes it

### Requirement: Owner scope and GitHub host pin
GitHub inventory SHALL be scoped to `[projects] owners`, or the authenticated login when unset or empty. Owner values MUST be validated and deduplicated before becoming gh arguments; one malformed entry means zero queries. Every gh call MUST be pinned to `[github] host` (default `github.com`), overriding an ambient `GH_HOST`, and on a non-default host MUST scrub `GH_TOKEN`, `GITHUB_TOKEN`, `GH_ENTERPRISE_TOKEN`, `GITHUB_ENTERPRISE_TOKEN`.

#### Scenario: Ambient GH_HOST
- **WHEN** the shell sets `GH_HOST=ghe.example.com` and `[github] host` is unset
- **THEN** the inventory queries github.com

#### Scenario: Invalid owner entry
- **WHEN** `[projects] owners` holds one malformed value
- **THEN** no GitHub query runs for any owner

### Requirement: Gitea rows take their host from the clone URL
Gitea rows SHALL take their hostname from each repo's clone URL as reported by `tea`, with no configured Gitea host. A row whose URL has no hostname, or whose hostname equals the configured GitHub host, MUST be dropped.

#### Scenario: Gitea repo on its own host
- **WHEN** `tea` reports a repo cloned from `https://git.example.com/me/app.git`
- **THEN** the row's host is `git.example.com`

### Requirement: Pull every project
`projects pull-all` SHALL pull every local project, skip any checkout with a dirty working tree, and exit 1 when any pull failed. `--json` SHALL emit `[{"name","status"}]` with the same exit code.

#### Scenario: A dirty checkout
- **WHEN** one project has uncommitted changes
- **THEN** it is skipped rather than pulled
