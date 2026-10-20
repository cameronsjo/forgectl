# docs-reader Specification

## Purpose
The `docs` command group is forgectl's local markdown reader: it indexes doc roots, renders and serves them over loopback HTTP, checks their links, lists and searches them, and opens one in the terminal or browser, the same way at the machine or over SSH.

## Requirements

### Requirement: Docs verbs
forgectl SHALL provide `docs serve [dir|file ...]`, `docs open [path]` (with `--print-url`), `docs read <file>`, `docs list [dir|file ...]`, `docs check [dir|file ...]`, and `docs search <query>`. `list`, `check`, and `search` MUST accept `--json`.

#### Scenario: Listing as JSON
- **WHEN** the user runs `forgectl docs list --json`
- **THEN** stdout is an array of `{"root","path","title","modTime"}` objects, and `[]` when nothing is indexed

### Requirement: Default doc set
With no path arguments, `serve`, `list`, `check`, `read`, and `search` SHALL index the current directory, `./docs` when present, `$CADENCE_FIELD_REPORTS_DIR` when set, and every root in `[docs].roots` of `config.toml` (with leading `~` expanded). Naming paths on the command line MUST replace that default set entirely.

#### Scenario: Explicit roots replace defaults
- **WHEN** the user runs `forgectl docs list ./notes`
- **THEN** only docs under `./notes` are listed

### Requirement: Root kinds
Each root SHALL be classified as `vault` when it or an ancestor (stopping at home, a filesystem boundary, or `/`) contains `.obsidian/`, else `docs`; `[docs.root_kinds]` MUST override detection, and any value other than `docs` or `vault` is a config error. A `vault` root resolves `[[wikilinks]]` and Obsidian syntax; a `docs` root stays GitHub-flavoured markdown with `[[w]]` as literal text. Links never resolve across roots.

#### Scenario: Wikilink in a docs root
- **WHEN** a doc in a `docs` root contains `[[Other]]`
- **THEN** it renders as literal text and `docs check` does not count it as a link

### Requirement: Loopback serving with Host and cross-site rejection
`docs serve` SHALL bind `127.0.0.1` on a random port unless `--addr` or `[docs].addr` names another address. It MUST answer 403 to a request whose Host is not `127.0.0.1`, `localhost`, `::1`, or the bound non-loopback address, and to a request carrying a foreign `Origin` or `Sec-Fetch-Site: cross-site`. Every response MUST carry a Content-Security-Policy with `img-src 'self' data:`.

#### Scenario: DNS rebinding attempt
- **WHEN** a request reaches the loopback server with `Host: attacker.example`
- **THEN** the server responds 403

#### Scenario: Cross-site page fetch
- **WHEN** a browser request arrives with `Sec-Fetch-Site: cross-site`
- **THEN** the server responds 403

### Requirement: Bearer token off loopback
`docs serve` MUST require a bearer token whenever it binds a non-loopback address, generating one when `--token-file` is not given. A `--token-file` MUST be an absolute path to a regular, owner-only file holding one RFC 6750 token plus an optional final newline, or startup exits 2. The removed `--token` flag MUST be refused with exit 2 and a message pointing to `--token-file`; no token is ever accepted on argv.

#### Scenario: Network bind without token file
- **WHEN** the user runs `forgectl docs serve --addr 100.64.0.5:8080` without `--token-file`
- **THEN** the server generates a token and rejects requests lacking it with 401

#### Scenario: Token on argv
- **WHEN** the user passes `--token abc`
- **THEN** the command exits 2 naming `--token-file` as the replacement

### Requirement: Open steers but never starts a server
`docs open` SHALL point the system browser at an already-running `docs serve` reader found through discovery, and MUST NOT start a server. With no reader running it MUST exit 2 naming `forgectl docs serve`. For a token-protected reader it MUST print the URL and a `curl -H 'Authorization: Bearer ...'` command instead of opening a browser; a protected legacy reader gets the URL and restart advice, never the token.

#### Scenario: Nothing running
- **WHEN** the user runs `forgectl docs open` with no reader serving
- **THEN** no server starts and the command exits 2 naming `forgectl docs serve`

#### Scenario: Print only
- **WHEN** the user runs `forgectl docs open --print-url guide.md` against a running reader
- **THEN** the doc's URL is printed and no browser opens

### Requirement: Root containment
Every path served, read, searched, or existence-checked SHALL resolve one component at a time inside its root. A symlink MUST be followed only while it stays inside the root; leaving the root refuses the whole path, and a chain of more than 40 symlinks is refused. `docs serve` MUST serve only the regular file the resolution approved, so a swap after the check cannot redirect the read. Files outside the index, excluded directories, and non-markdown files are refused by `docs read`.

#### Scenario: Symlink escapes the root
- **WHEN** a doc path passes through a symlink pointing outside the root
- **THEN** the reader treats it as not found and serves nothing

### Requirement: Render limits
`docs serve` MUST NOT render a doc over 1 MiB, serving a notice that suggests `forgectl docs read` instead. A doc over 512 KiB, or one whose markup would make the parser superlinear, SHALL be shown as source text under a notice. Each page render MUST have a 5 second deadline, after which its source text is shown under a "too slow to render" notice.

#### Scenario: Oversized doc
- **WHEN** a browser requests a 2 MiB markdown doc
- **THEN** the page is a notice naming the doc rather than a render

### Requirement: Link check findings
`docs check` SHALL report `broken_link`, `ambiguous_link`, `broken_anchor`, `orphan`, `stale`, `deprecated`, and `anchor_unchecked` findings, each with `severity` `error` or `info` (`deprecated` and `anchor_unchecked` are `info`). Out-of-root links MUST be counted in `summary.outside_root_links`, not reported. A root-level README or index page, or a page with frontmatter `orphan_ok: true`, is never an orphan. Vault roots get only link findings.

#### Scenario: Orphan opt-out
- **WHEN** an unlinked page has `orphan_ok: true` in YAML frontmatter
- **THEN** no `orphan` finding is reported and `summary.ignored_orphans` counts it

### Requirement: Check JSON and exit codes
`docs check --json` SHALL print one object with `schema_version`, `roots`, `findings`, `summary`, and an always-present `skipped` array of `{root, path, reason}`; the shape is additive-only. Exit MUST be 0 when clean or only `info` findings exist, 1 when any `error` finding exists, and 2 when the check could not run or the walk skipped an unreadable path. Exit 2 outranks exit 1.

#### Scenario: Info-only findings
- **WHEN** the only finding is `deprecated`
- **THEN** the command exits 0

#### Scenario: Unreadable subdirectory
- **WHEN** the index walk skips an unreadable directory
- **THEN** the full report is printed, the path appears in `skipped`, and the command exits 2

### Requirement: Search gated by the index
`docs search` SHALL run a case-insensitive fixed-string query with ripgrep by default (`rg --no-config`), or qmd's BM25 search only when `--backend qmd` or `search_backend = "qmd"` asks for it. Every hit MUST be checked against the docs index and dropped (counted in `skipped`) when outside it, and snippets MUST be re-read from the indexed doc, never taken from the backend. `--limit` defaults to 50 and `--timeout` to 10s.

#### Scenario: Hit in an excluded directory
- **WHEN** rg reports a match under `node_modules/`
- **THEN** the result is omitted and `skipped` increases

#### Scenario: qmd not requested
- **WHEN** qmd is installed but neither the flag nor config selects it
- **THEN** the search runs with ripgrep and `backend` is `ripgrep`

### Requirement: Search JSON and exit codes
`docs search --json` SHALL print `{"backend","query","results","truncated","skipped","errors","skipped_paths"}` with `results`, `errors`, and `skipped_paths` always arrays. No match MUST exit 0 with empty results; a root rg could not fully search exits 1 with every other hit printed; a missing backend, invalid query, bad `--limit`, root or config error, or expired `--timeout` exits 2.

#### Scenario: Missing ripgrep
- **WHEN** `rg` is not on `PATH`
- **THEN** the command exits 2, stdout is empty under `--json`, and stderr holds one `{"error","code","root"}` object

### Requirement: Could-not-run error contract
Every docs verb SHALL exit 2 when it fails before its real work (unreadable root, bad `[docs]` config, bad flag or argument count, deadline, bind failure, unusable token file). Under `--json` on `list`, `check`, and `search`, such a failure MUST leave stdout empty and write exactly one `{"error","code","root"}` object to stderr, with `root` always present.

#### Scenario: Bad flag under JSON
- **WHEN** the user runs `forgectl docs list --bogus --json`
- **THEN** stdout is empty, stderr holds one error object with code 2, and the exit is 2

### Requirement: Terminal read without blocking agents
`docs read <file>` SHALL run `mdroll --watch --no-remote-images -- <absolute path>` without a shell when mdroll resolves through an absolute `PATH` entry, passing mdroll's exit status through. Without mdroll, when stdin and stdout are both terminals it serves the set with the browser opened at that doc; otherwise it MUST start nothing, print the resolved absolute path to stdout, and exit 0.

#### Scenario: Agent without mdroll
- **WHEN** an agent with no TTY runs `forgectl docs read guide.md` and mdroll is absent
- **THEN** stdout is the doc's absolute path, no server starts, and the exit is 0
