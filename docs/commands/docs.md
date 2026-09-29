# forgectl docs — local markdown reader: render + serve an indexed doc set over loopback HTTP

> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).

`docs` is forgectl's local markdown reader ([forgectl#93](https://github.com/cameronsjo/forgectl/issues/93)): pure-Go server-side rendering (goldmark+GFM, class-based chroma highlighting, bluemonday sanitization), Artificer-themed, served over loopback HTTP so it behaves the same whether you're at the machine or SSH'd in from the headless workbench — no terminal-specific rendering, no popping between windows.

```sh
forgectl docs serve [dir|file ...]       # render + serve, loopback-only (DNS-rebinding-safe)
forgectl docs serve --open               # also open the system browser
forgectl docs open [path]                # point the browser at a doc on the already-running reader
forgectl docs open --print-url [path]    # print the resolved URL instead of opening a browser
forgectl docs read <file>                # read one doc in the terminal with mdroll, else in the HTML reader
forgectl docs list [dir|file ...]        # list the indexed docs, no server (--json for scripting)
forgectl docs check [dir|file ...]       # broken links, broken anchors, orphan pages (--json for scripting)
forgectl docs search <query> [--json]    # full-text search the indexed docs (ripgrep backend)
```

Documents in a `vault` root (see [Root kinds](#root-kinds)) also render Obsidian's inline syntax: `==text==` is highlighted, `#tags` show as tag chips, and `%%comments%%` are hidden. Comment text is kept out of the page body, the page title and sidenav entry, heading anchors and the "On this page" outline, and the index that links resolve against. Two forms are hidden:

- An inline comment: two `%%` markers in the same paragraph, heading, table cell or link text, and everything between them, spaces and line breaks included (`%% like this %%`). Markdown decides first, so a `%%` inside inline code, an autolink or a `[[wikilink]]` is not a marker (it neither opens nor closes a comment), and `\%` is a literal percent sign. A `%%` with no partner stays visible as text, and a paragraph that holds only a comment disappears.
- A block comment: a line starting with `%%` and holding no other `%%`, at the top level of a note (not inside a blockquote or list), through the next line containing `%%`, when that closing `%%` ends its line.

A block comment is not recognized, and its lines are read as ordinary text under the inline rule, when its closing line has text after the `%%`, or when a line before the closer opens a code fence (```` ``` ```` or `~~~`) or an HTML block (a line starting with `<`). A stray marker therefore never hides the rest of a note. Docs roots render plain GitHub-flavoured markdown, where all of these stay literal.

Vault `[[wikilinks]]` render as links to the note they resolve to, by the rules in [Root kinds](#root-kinds): a heading link jumps to the heading, and a block link jumps to the block. A link that does not resolve, is ambiguous, or leaves the root shows dashed and red, with the reason on hover. A link whose note exists but whose heading or block does not still opens the note, marked the same way. `[[note\|alias]]`, the escaped form a table cell needs, links like `[[note|alias]]`. `![[embeds]]`, a wikilink inside a markdown link, and one after a raw-HTML `<a>` opened earlier in the same paragraph show as their source text.

A block id (`^blk-1`) at the end of a paragraph or list item, after a space or at the start of the paragraph's last line, is hidden and becomes that block's anchor. It renders as `id="^blk-1"`, so it never collides with a heading's anchor. Anywhere else a `^id` stays visible, and a link to it still resolves but opens the note at the top. That covers a `^id` in a paragraph of its own, in the middle of a paragraph, on a heading, in a callout's first paragraph, and inside inline code or `$$…$$` math that spans lines within a paragraph. A `^id` inside a code block, a `$$` block or a `%%` comment is not a block id, so a link to it is marked as a missing block.

A page served in the moment between a file changing and the index rebuilding shows every wikilink as unresolved. This fails closed by design, and the live reload that follows the rebuild shows the resolved links.

Diagrams render in the page: a fenced code block tagged `mermaid` becomes a live diagram themed from the same Artificer tokens as the rest of the reader, and both those and inline SVG pan and zoom (drag to pan, modifier-scroll or click-then-scroll to zoom, double-click, `0`, or the diagram card's reset button to reset). Mermaid 11.12.3 (MIT) is embedded in the binary, and its license ships in the release archives' `font-licenses/`.

Math renders in the page too, client-side with a vendored KaTeX: `$…$` inline, and `$$…$$` or a fenced code block tagged `math` for display math. A dollar sign stays literal text unless it clearly opens math, so `It costs $5 and $10` and `echo $HOME` are left alone. A `<` directly followed by a letter also keeps math literal, because it reads as an HTML tag: `$a<b$` shows as typed, while `$a < b$` renders. A formula KaTeX cannot parse shows its TeX in red, with the error on hover. Commands that emit links or HTML, such as `\href` and `\htmlClass`, are disabled. KaTeX 0.18.9 (MIT) is embedded in the binary with its woff2 fonts; its version and a sha256 per file are recorded in `internal/docs/assets/provenance-katex.json`, and its license ships in the release archives' `font-licenses/`.

The landing page at `/` lists the most recently changed docs and each root's doc count. The reading surface: the sidenav renders each indexed root as a collapsible directory tree (per-directory counts, current path pre-expanded, filter box that hides empty branches), and a document's YAML/TOML frontmatter renders as an always-visible properties block above the body instead of leaking into it. Longer documents get an "On this page" outline of their h2 and h3 sections — a third column on wide viewports, an inline disclosure on narrow ones — GFM alert blockquotes (`> [!NOTE]` and kin) render as tiered callouts (in a vault root, Obsidian's callout types and aliases too, in any case, such as `> [!info]` or `> [!summary]-`), and a status bar carries the serving address, document path, and reading time. Below 900px the sidenav becomes an off-canvas drawer behind the appbar toggle. When a watched file changes, the open page updates in place: the reading position, opened folders and collapsibles, and a live filter all survive the update.

## Doc discovery

With no arguments, `serve`, `list`, `check`, `read` and `search` all index cwd, `./docs` (if present), and `$CADENCE_FIELD_REPORTS_DIR` (if set), plus any extra roots configured in the `[docs]` section of `config.toml`:

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

### Exit codes and errors

Every docs verb shares one "could not run" contract ([#604](https://github.com/cameronsjo/forgectl/issues/604)):

- **Exit 2** when the verb fails before it starts its real work: an unreadable or missing root, a bad `[docs]` config, a bad flag or a wrong number of arguments, a `--timeout` deadline, no search backend, no reader to open, a bind failure, an unusable `--token-file`, or the retired `--token`.
- **Under `--json`**, on the verbs that declare it (`list`, `check`, `search`), that failure leaves stdout empty and writes exactly one `{"error","code","root"}` object to stderr. This includes flag and argument errors, even a `--json` written after the bad flag. `root` is always present and is empty unless a deadline stopped on a specific root. Verbs without `--json` (`serve`, `open`, `read`) print the human error and exit 2.
- The object's shape is additive-only ([ADR-0008](../adr/0008-agent-contract.md) rule 2): `root` was already documented for `docs list`, so it stays a fixed key rather than being omitted when empty, and `docs search` gained it.

Two things sit outside the contract, on purpose:

- **A partial result exits 1.** `docs check` with findings, and `docs search` when rg could not fully search a root, did run; their output is complete on stdout. Under `--json` a partial `docs search` also writes one `{"error","code","root"}` object (code 1, `root` empty) to stderr.
- **`docs read` passes mdroll's exit status through** (see [`docs read`](#docs-read-in-the-terminal)), so a 2 from `read` is ambiguous once mdroll has started. The contract applies only before the child does; a failure to start mdroll is still a 2.

`docs serve` exits 0 after a clean Ctrl-C. A failure once the server is listening (the serve loop itself failing) is the server's own and exits 1.

### Root kinds

Every root is classified as `docs` or `vault` when it is indexed. A root is a `vault` when it, or a directory above it (stopping at your home directory, a filesystem boundary, or `/`), contains an Obsidian `.obsidian/` folder; anything else is `docs`. The kind decides how links inside that root resolve:

| Kind | Link target | Anchor |
|---|---|---|
| `docs` | relative markdown path from the linking file (`[x](../guide.md)`) | GitHub-style heading slug (`#getting-started`) |
| `vault` | wikilink by vault-relative path, bare note name, or frontmatter alias (`[[Note]]`, `[[folder/Note]]`); a `./` or `../` markdown path resolves from the linking file | heading text or slug (`#Some Heading`; text matches ignoring case, spacing and markdown punctuation such as `==`, `*` or backticks; a heading whose text contains `#` can only be linked by its slug, because `#` separates nested headings), or a block id (`#^blk-1`) |

The kind also picks the markdown dialect: a `vault` root renders the Obsidian highlights, comments, tags, callout types and wikilinks described above, resolving and rendering each wikilink by these rules, and a `docs` root stays plain GitHub-flavoured markdown. Links never resolve across roots. `[docs.root_kinds]` forces a kind when detection gets it wrong, keyed by the root path as you wrote it in `roots` or on the command line — relative spellings such as `.` match the same directory the CLI derives. A value other than `docs` or `vault` is a config error, and `forgectl launch doctor` reports it.

## Checking links

`docs check` walks the same roots as `list` (no arguments: cwd, `./docs`, `$CADENCE_FIELD_REPORTS_DIR`, and any `[docs].roots`; naming paths replaces that set) and reports links that are broken on disk (as GitHub would render them), without binding a server.

| Finding kind | Meaning |
|---|---|
| `broken_link` | the target file does not exist |
| `ambiguous_link` | the target matches more than one doc (for example `notes.md` and `notes.markdown`); candidates are not listed |
| `broken_anchor` | the file exists but the `#heading` or `#^block` fragment does not, including a fragment-only `#x` |
| `orphan` | a doc in a directory root that no other doc links to |
| `deprecated` | the doc's YAML frontmatter says `status: deprecated` (exact, lowercase) |
| `stale` | the doc's YAML frontmatter `stale_after` is an RFC 3339 instant with an explicit offset (`2026-09-23T00:00:00Z`) and now is at or past it; a date-only (`2026-09-23`) or offset-less value is ignored; a TOML `stale_after` is ignored too |

- **Existence fallback.** A link to a directory (`commands/`) or a non-markdown file (`LICENSE`, an image) resolves to no indexed doc. It is reported as broken only when nothing exists at that path inside the root. The check reads nothing and refuses a symlink that escapes the root. It checks existence only: a `#fragment` on such a link is not checked, whether the target is a non-indexed file that exists on disk (`LICENSE#x`, `diagram.png#page=2`) or a directory (`commands/#x`), so those links never produce a `broken_anchor`.
- **Out-of-root links are counted, not reported.** A link such as `../../README.md` that leaves its root works on GitHub, so it is not a finding. It is counted in `summary.outside_root_links`.
- **Orphans.** A root-level `README` or `index` page is never an orphan, and a single-file root has no orphans. A `README.md` in a subdirectory is an ordinary doc, but a directory link from another doc (`[plans](plans/)`, or root-relative `/plans`) counts as a link to that directory's `README` or `index` page (any case, any indexed extension), the page GitHub shows for it. A target ending in `/`, or in a `.` or `..` segment, always names the directory, even when a `plans.md` sits beside it. Only the orphan check counts it this way: the link still resolves to no doc in the reader. Other docs in that directory still need a link of their own.
- **Vault roots are skipped.** A root detected or configured as a `vault` is not checked yet: a note on stderr says so, and if no docs-kind root remains the command exits 2.
- **Trust signals.** `deprecated` and `stale` follow the Open Knowledge Format v0.2 §5.4/§5.5 (SPEC at `ad30107`). Both are findings, so they exit 1 like any other. The reader also badges them, in the properties block and in the status bar. OKF changed `stale_after` from a date to a datetime inside v0.2 without a version bump; date-only values written against the older text are ignored, per the current spec and its reference implementation. Coverage gaps: vault roots are not checked (see above), though the reader still badges their docs, and a doc over 1 MiB is indexed by title only, so it gets no finding and no status-bar badge, though its properties block still badges.
- **Exit codes.** 0 clean; 1 findings (the complete report is on stdout); 2 the check could not run (unreadable root, `--timeout` deadline, no docs-kind root, bad flag), under the shared contract above.

Human output is one line per finding, `<root>/<path>: <kind> <target>`; a link finding (`broken_link`, `ambiguous_link`, `broken_anchor`) names its source line as `<root>/<path>:<line>: <kind> <target>`, and a `stale` line ends with its `stale_after` value instead of a target. `line` is 1-based and counts every line of the file as written, frontmatter included, so it can feed a CI annotation directly. Findings are grouped by root, in configured order, then by path. Within a file, link findings come first, sorted by line, and the lineless findings follow, sorted by kind. `--json` prints one object:

```json
{
  "schema_version": 1,
  "roots": [{"label": "docs", "kind": "docs", "checked": true, "docs": 42}],
  "findings": [
    {"kind": "broken_link", "root": "docs", "path": "plans/x.md", "target": "gone.md", "line": 12},
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

Exit codes: no match exits 0 with an empty `results` (human output says `no matches` on stderr). When rg could not fully search a root, the other roots are still searched and every hit found is printed, then the command exits 1 with the reason on stderr: one line per failed root, or under `--json` one `{"error","code","root"}` object on stderr alongside the full response, `errors` included, on stdout. A missing `rg`, an empty or invalid query, a bad `--limit`, a root or config error, or an expired `--timeout` exits 2; under `--json` that leaves stdout empty and writes exactly one `{"error","code","root"}` object to stderr (the shared contract above). The partial-result object is the same shape with code 1.

## `docs open` steers, never starts

`docs open` points the system browser at an already-running `docs serve` reader; it never starts one. That is a deliberate boundary: `docs serve` is a foreground process the operator owns — it prints its address, holds the terminal, and stops on Ctrl-C. If `open` could spawn one, it would either fork a server nobody can see or block the terminal it was called from, and either way the operator would no longer know how many readers exist or which one their browser is pointed at. When nothing is running, `open` says so and names the command to run.

It uses the system browser, never a terminal's own browser command — the reader's entire premise is being terminal-agnostic (reachable from the machine, from an SSH session, from a phone), so coupling `open` to one terminal emulator would undo that.

A legacy server (predating generation-owned discovery) has no freshness endpoint, so `open` cannot verify the listener at its recorded address is still the same server before handing it a token — it prints the URL and tells you to restart with `forgectl docs serve` instead.

## `docs read` in the terminal

`docs read <file>` opens one document from the default doc set (the same roots `docs serve` and `docs list` index with no arguments). `<file>` is a path on disk or a root-relative `<root>/<path>` name as `docs list` prints it, and either way it resolves through the index: a file outside the indexed roots, under an excluded directory, or not markdown is refused.

When [mdroll](https://github.com/tokuhirom/mdroll) is on `PATH`, `read` runs it as `mdroll --watch --no-remote-images -- <absolute path>`, with no shell and with forgectl's stdin, stdout, and stderr handed straight through, so mdroll's own keys (search, TOC, link picker) work. `--watch` stands in for the HTML reader's live reload, and `--` keeps a document named like a flag from being parsed as one (`forgectl docs read -- -odd.md` gets such a name past forgectl's own parser). mdroll's exit status becomes forgectl's (the one carve-out from the exit-2 contract above); if a signal kills mdroll, forgectl exits 128 plus the signal number, as a shell would. An mdroll reachable only through a relative `PATH` entry is refused and treated as absent.

`--no-remote-images` keeps mdroll from fetching `http(s)` images, which it does by default: a remote image in a document is a tracking beacon, and the HTML reader blocks it with `img-src 'self' data:`. Beyond that one flag, `docs read` follows mdroll's own content policy, not the HTML reader's — mdroll does its own rendering, and forgectl's sanitizer and CSP do not apply to it.

mdroll is optional. Without it, what `read` does depends on whether stdin and stdout are both terminals:

- **Both terminals:** `read` serves the doc set as `docs serve --open` would, with the browser pointed at that document rather than the index. It holds the terminal until Ctrl-C, like `docs serve`.
- **Otherwise** (an agent, a script, a pipe, stdin from `/dev/null`): `read` starts nothing. It prints the document's resolved absolute path to stdout, a note on stderr naming `forgectl docs serve --open`, and exits 0. A server would block the caller with nothing to interrupt it ([ADR-0008](../adr/0008-agent-contract.md)).

`forgectl doctor` reports mdroll as skipped, not failed, when it is absent or found only through a relative `PATH` entry.

How discovery records are written, where they live on disk, and how to clear them by hand after a crash: [docs server discovery — operations](../operations/docs-discovery.md).
