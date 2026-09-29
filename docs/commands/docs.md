# forgectl docs — local markdown reader: render + serve an indexed doc set over loopback HTTP

> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).

`docs` is forgectl's local markdown reader ([forgectl#93](https://github.com/cameronsjo/forgectl/issues/93)): pure-Go server-side rendering (goldmark+GFM, class-based chroma highlighting, bluemonday sanitization), Artificer-themed, served over loopback HTTP so it behaves the same whether you're at the machine or SSH'd in from the headless workbench — no terminal-specific rendering, no popping between windows.

```sh
forgectl docs serve [dir|file ...]       # render + serve, loopback-only (DNS-rebinding-safe)
forgectl docs serve --open               # also open the system browser
forgectl docs open [path]                # point the browser at a doc on the already-running reader
forgectl docs open --print-url [path]    # print the resolved URL instead of opening a browser
forgectl docs list [dir|file ...]        # list the indexed docs, no server (--json for scripting)
forgectl docs check [dir|file ...]       # broken links, broken anchors, orphan pages (--json for scripting)
forgectl docs search <query> [--json]    # full-text search the indexed docs (ripgrep backend)
```

Diagrams render in the page: a fenced code block tagged `mermaid` becomes a live diagram themed from the same Artificer tokens as the rest of the reader, and both those and inline SVG pan and zoom (drag to pan, modifier-scroll or click-then-scroll to zoom, double-click, `0`, or the diagram card's reset button to reset).

The landing page at `/` lists the most recently changed docs and each root's doc count. The reading surface: the sidenav renders each indexed root as a collapsible directory tree (per-directory counts, current path pre-expanded, filter box that hides empty branches), and a document's YAML/TOML frontmatter renders as an always-visible properties block above the body instead of leaking into it. Longer documents get an "On this page" outline of their h2 and h3 sections — a third column on wide viewports, an inline disclosure on narrow ones — GFM alert blockquotes (`> [!NOTE]` and kin) render as tiered callouts, and a status bar carries the serving address, document path, and reading time. Below 900px the sidenav becomes an off-canvas drawer behind the appbar toggle. When a watched file changes, the open page updates in place: the reading position, opened folders and collapsibles, and a live filter all survive the update.

## Doc discovery

With no arguments, both `serve` and `list` index cwd, `./docs` (if present), and `$CADENCE_FIELD_REPORTS_DIR` (if set), plus any extra roots configured in the `[docs]` section of `config.toml`:

```toml
[docs]
roots = ["~/Projects/forgectl/docs"]   # extra indexed roots, beyond the defaults
addr  = ""                              # default bind address for `docs serve`

[docs.root_kinds]                       # override per-root link semantics (see below)
"/absolute/path/to/notes" = "vault"
"." = "docs"                            # relative to where forgectl runs
```

A leading `~` or `~/` in `roots` and in the `root_kinds` keys expands to your home directory. Paths named on the command line are not expanded; your shell does that.

Naming directories or files on the command line replaces that default set entirely.

### Root kinds

Every root is classified as `docs` or `vault` when it is indexed. A root is a `vault` when it, or a directory above it (stopping at your home directory, a filesystem boundary, or `/`), contains an Obsidian `.obsidian/` folder; anything else is `docs`. The kind decides how links inside that root resolve:

| Kind | Link target | Anchor |
|---|---|---|
| `docs` | relative markdown path from the linking file (`[x](../guide.md)`) | GitHub-style heading slug (`#getting-started`) |
| `vault` | wikilink by vault-relative path, bare note name, or frontmatter alias (`[[Note]]`, `[[folder/Note]]`); a `./` or `../` markdown path resolves from the linking file | heading text or slug (`#Some Heading`), or a block id (`#^blk-1`) |

Links never resolve across roots. `[docs.root_kinds]` forces a kind when detection gets it wrong, keyed by the root path as you wrote it in `roots` or on the command line — relative spellings such as `.` match the same directory the CLI derives. A value other than `docs` or `vault` is a config error, and `forgectl launch doctor` reports it.

## Checking links

`docs check` walks the same roots as `list` (no arguments: cwd, `./docs`, `$CADENCE_FIELD_REPORTS_DIR`, and any `[docs].roots`; naming paths replaces that set) and reports what would 404 in the reader, without binding a server.

| Finding kind | Meaning |
|---|---|
| `broken_link` | the target file does not exist |
| `ambiguous_link` | the target matches more than one doc (for example `notes.md` and `notes.markdown`); candidates are not listed |
| `broken_anchor` | the file exists but the `#heading` or `#^block` fragment does not, including a fragment-only `#x` |
| `orphan` | a doc in a directory root that no other doc links to |
| `deprecated` | the doc's YAML frontmatter says `status: deprecated` (exact, lowercase) |
| `stale` | the doc's frontmatter `stale_after` is an RFC 3339 instant with an explicit offset (`2026-09-23T00:00:00Z`) and now is at or past it; a date-only (`2026-09-23`) or offset-less value is ignored |

- **Existence fallback.** A link to a directory (`commands/`) or a non-markdown file (`LICENSE`, an image) resolves to no indexed doc. It is reported as broken only when nothing exists at that path inside the root. The check reads nothing and refuses a symlink that escapes the root.
- **Out-of-root links are counted, not reported.** A link such as `../../README.md` that leaves its root works on GitHub, so it is not a finding. It is counted in `summary.outside_root_links`.
- **Orphans.** A root-level `README` or `index` page is never an orphan, and a single-file root has no orphans. A `README.md` in a subdirectory is an ordinary doc.
- **Vault roots are skipped.** A root detected or configured as a `vault` is not checked yet: a note on stderr says so, and if no docs-kind root remains the command exits 2.
- **Trust signals.** `deprecated` and `stale` follow the Open Knowledge Format v0.2 §5.4/§5.5 (SPEC at `ad30107`). Both are findings, so they exit 1 like any other. The reader also badges them, in the properties block and in the status bar. OKF changed `stale_after` from a date to a datetime inside v0.2 without a version bump; date-only values written against the older text are ignored, per the current spec and its reference implementation. Coverage gaps: vault roots are not checked (see above), though the reader still badges their docs, and a doc over 1 MiB is indexed by title only, so it gets no finding and no status-bar badge, though its properties block still badges.
- **Exit codes.** 0 clean; 1 findings (the complete report is on stdout); 2 the check could not run (unreadable root, `--timeout` deadline, no docs-kind root, bad flag).

Human output is one line per finding, `<root>/<path>: <kind> <target>`; a `stale` line ends with its `stale_after` value instead of a target. `--json` prints one object:

```json
{
  "schema_version": 1,
  "roots": [{"label": "docs", "kind": "docs", "checked": true, "docs": 42}],
  "findings": [
    {"kind": "broken_link", "root": "docs", "path": "plans/x.md", "target": "gone.md"},
    {"kind": "orphan", "root": "docs", "path": "notes.md"},
    {"kind": "stale", "root": "docs", "path": "runbook.md", "stale_after": "2026-09-01T00:00:00Z"}
  ],
  "summary": {"broken_links": 1, "ambiguous_links": 0, "broken_anchors": 0, "orphans": 1, "outside_root_links": 20, "deprecated": 0, "stale": 1}
}
```

The shape is additive-only ([ADR-0008](../adr/0008-agent-contract.md) rule 2): new keys and finding kinds may appear, existing ones are never renamed or removed. A skipped vault root also carries a `skipped` reason, and roots never carry an absolute path.

## Bearer-token surface

The server binds loopback-only by default and rejects any request whose `Host` header isn't `127.0.0.1`/`localhost`/`::1` — DNS-rebinding defense, not just a bind-address restriction.

Binding `--addr` to a non-loopback address adds that address to the allowlist and **requires** a bearer token, generating one if `--token-file` is not passed: exposing the reader to the network and authenticating it are one decision, never two.

A token file must be:

- an **absolute** path
- a **regular file**
- **owner-only** permissions
- containing one RFC 6750 bearer token, plus an optional final LF or CRLF

`--token` (a command-line value) was removed — command-line values are visible to other processes on the same host — so `--token-file` is the only way to supply one explicitly.

**Protected servers cannot be `--open`ed directly**, because browser navigation cannot attach an `Authorization` header. `forgectl docs open` on a token-protected server prints the URL and a `curl -H 'Authorization: Bearer <token>' <url>` command instead of opening a browser.

## Search

`forgectl docs search <query>` runs a case-insensitive, fixed-string full-text query over the same roots `docs list` indexes with no arguments, and prints one line per hit: root, `path:line`, and a snippet of up to 240 characters around the match. `--limit N` (default 50) caps the results and `--timeout` (default 10s) bounds indexing plus search. A query that starts with `-` goes after `--`: `forgectl docs search -- --flag-name`.

The backend is [ripgrep](https://github.com/BurntSushi/ripgrep) (`rg`), which must be on `PATH`. Support for qmd as a ranked backend is planned.

- **Only indexed docs are returned.** Every hit rg reports is checked against the docs index, the same membership gate the reader serves through, so a file outside a root, under an excluded directory (`.git`, `node_modules`, `vendor`, any dot-directory), or reached through a symlink never appears. Hits dropped this way are counted in `skipped`.
- **rg's own config file is ignored.** rg runs with `--no-config`, so `RIPGREP_CONFIG_PATH` cannot turn on `--follow` or otherwise change what is searched.
- Files over 1 MB are not searched, and at most 5 hits are taken from one file.
- Docs whose paths are not valid UTF-8 are not searchable; rg can only report such a path as raw bytes, and those hits are counted in `skipped`.
- **Results are ordered and stable.** Roots are searched in their configured order, and rg walks each root in path order (`--sort=path`, which also keeps rg to a single worker), so the same query over the same tree returns the same results, and `--limit` always keeps the same prefix. A doc reachable through two overlapping roots (cwd and `./docs`, say) is returned once, under the first root.

`--json` prints one object to stdout. `results` is always an array, and each result carries `root`, `path`, `title`, `line`, and `snippet`. `truncated` is true when more hits existed past `--limit`. `errors` is always an array of `{root, message}`, one per root rg could not fully search (an unreadable file, say, or output that could not be parsed).

```json
{"backend":"ripgrep","query":"needle","results":[{"root":"docs","path":"guide.md","title":"Guide","line":12,"snippet":"the needle in the guide"}],"truncated":false,"skipped":0,"errors":[]}
```

Exit codes: no match exits 0 with an empty `results` (human output says `no matches` on stderr). When rg could not fully search a root, the other roots are still searched and every hit found is printed, then the command exits 1 with the reason on stderr: one line per failed root, or under `--json` one `{"error","code"}` object on stderr alongside the full response, `errors` included, on stdout. A missing `rg`, an empty or invalid query, or an expired `--timeout` exits 2; under `--json` that leaves stdout empty and writes exactly one `{"error","code"}` object to stderr.

## `docs open` steers, never starts

`docs open` points the system browser at an already-running `docs serve` reader; it never starts one. That is a deliberate boundary: `docs serve` is a foreground process the operator owns — it prints its address, holds the terminal, and stops on Ctrl-C. If `open` could spawn one, it would either fork a server nobody can see or block the terminal it was called from, and either way the operator would no longer know how many readers exist or which one their browser is pointed at. When nothing is running, `open` says so and names the command to run.

It uses the system browser, never a terminal's own browser command — the reader's entire premise is being terminal-agnostic (reachable from the machine, from an SSH session, from a phone), so coupling `open` to one terminal emulator would undo that.

A legacy server (predating generation-owned discovery) has no freshness endpoint, so `open` cannot verify the listener at its recorded address is still the same server before handing it a token — it prints the URL and tells you to restart with `forgectl docs serve` instead.

How discovery records are written, where they live on disk, and how to clear them by hand after a crash: [docs server discovery — operations](../operations/docs-discovery.md).
