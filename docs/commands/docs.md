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
forgectl docs search <query> [--json]    # full-text search the indexed docs (ripgrep, or opt-in qmd)
```

Documents in a `vault` root (see [Root kinds](#root-kinds)) also render Obsidian's inline syntax: `==text==` is highlighted, `#tags` show as tag chips, and `%%comments%%` are hidden. Comment text is kept out of the page body, the page title and sidenav entry, heading anchors and the "On this page" outline, and the index that links resolve against. Two forms are hidden:

- An inline comment: two `%%` markers in the same paragraph, heading, table cell or link text, and everything between them, spaces and line breaks included (`%% like this %%`). Markdown decides first, so a `%%` inside inline code, an autolink or a `[[wikilink]]` is not a marker (it neither opens nor closes a comment), and `\%` is a literal percent sign. A `%%` with no partner stays visible as text, and a paragraph that holds only a comment disappears.
- A block comment: a line starting with `%%` and holding no other `%%`, at the top level of a note (not inside a blockquote or list), through the next line containing `%%`, when that closing `%%` ends its line.

A block comment is not recognized, and its lines are read as ordinary text under the inline rule, when its closing line has text after the `%%`, or when a line before the closer opens a code fence (```` ``` ```` or `~~~`) or an HTML block (a line starting with `<`). A stray marker therefore never hides the rest of a note. A block comment also cannot interrupt a paragraph: a `%%` line directly under a paragraph line joins that paragraph, so the comment is hidden only under the inline rule, and one that spans a blank line shows as text. Obsidian may hide that case; this reader shows it rather than risk hiding prose. Docs roots render plain GitHub-flavoured markdown, where all of these stay literal.

Vault `[[wikilinks]]` render as links to the note they resolve to, by the rules in [Root kinds](#root-kinds): a heading link jumps to the heading, and a block link jumps to the block. A link that does not resolve, is ambiguous, or leaves the root shows dashed and red, with the reason on hover. A link whose note exists but whose heading or block does not still opens the note, marked the same way. `[[note\|alias]]`, the escaped form a table cell needs, links like `[[note|alias]]`. `![[embeds]]`, a wikilink inside a markdown link, and one after a raw-HTML `<a>` opened earlier in the same paragraph show as their source text.

A block id (`^blk-1`) at the end of a paragraph or list item, after a space or at the start of the paragraph's last line, is hidden and becomes that block's anchor. It renders as `id="^blk-1"`, so it never collides with a heading's anchor. A `^id` alone in a paragraph right after a list, table or blockquote (a callout included) is hidden too, and becomes the anchor of that list, table or quote, as Obsidian attaches it. Anywhere else a `^id` stays visible, and a link to it still resolves but opens the note at the top. That covers a `^id` in a paragraph of its own after anything else, such as another paragraph, in the middle of a paragraph, on a heading, in a callout's first paragraph, and inside inline code or `$$…$$` math that spans lines within a paragraph. A `^id` inside a code block, a `$$` block or a `%%` comment is not a block id, so a link to it is marked as a missing block.

A page served in the moment between a file changing and the index rebuilding shows every wikilink as unresolved. This fails closed by design, and the live reload that follows the rebuild shows the resolved links.

Diagrams render in the page: a fenced code block tagged `mermaid` becomes a live diagram themed from the same Artificer tokens as the rest of the reader, and both those and inline SVG pan and zoom (drag to pan, modifier-scroll or click-then-scroll to zoom, double-click, `0`, or the diagram card's reset button to reset). Mermaid 11.12.3 (MIT) is embedded in the binary, and its license ships in the release archives' `font-licenses/`.

Math renders in the page too, client-side with a vendored KaTeX: `$$…$$` or a fenced code block tagged `math` for display math, in every root, and `$…$` inline in `vault` roots only. A `docs` root leaves a single `$…$` as text, because a dollar sign there is far more often shell (`export PATH=$HOME/bin:$PATH`) than math, so a repo doc that relies on GitHub's `$x$` shows the raw TeX; write `$$x$$` for inline math there. In a vault root a dollar sign stays literal text unless it clearly opens math, so `It costs $5 and $10` and `echo $HOME` are left alone. A `<` directly followed by a letter also keeps math literal, because it reads as an HTML tag: `$a<b$` shows as typed, while `$a < b$` renders. A formula KaTeX cannot parse shows its TeX in red, with the error on hover. A formula that defines a macro (`\def`, `\newcommand`, `\let` and the rest of that family) is not rendered, and neither is one too long, too large or too deeply nested to render safely: each stays as its TeX source, with the reason on hover. Macros can multiply KaTeX's work enough to freeze the tab. Commands that emit links or HTML, such as `\href` and `\htmlClass`, are disabled. KaTeX 0.18.9 (MIT) is embedded in the binary with its woff2 fonts; its version and a sha256 per file are recorded in `internal/docs/assets/provenance-katex.json`, and its license ships in the release archives' `font-licenses/`.

Copying a selection from the document copies clean HTML and plain text, without the reader's theme fonts and colours, and a formula pastes as its TeX source (`$…$` or `$$…$$`). Selections outside the document body, such as the sidenav, copy natively.

The landing page at `/` lists the most recently changed docs and each root's doc count. The reading surface: the sidenav renders each indexed root as a collapsible directory tree (per-directory counts, current path pre-expanded, filter box that hides empty branches), and a document's YAML/TOML frontmatter renders as an always-visible properties block above the body instead of leaking into it. Longer documents get an "On this page" outline of their h2 and h3 sections — a third column on wide viewports, an inline disclosure on narrow ones — GFM alert blockquotes (`> [!NOTE]` and kin) render as tiered callouts (in a vault root, Obsidian's callout types and aliases too, in any case, such as `> [!info]` or `> [!summary]-`; a fold sign is accepted, but the callout always shows open). Plain text after the marker, as in `> [!tip] Before you start`, becomes the callout's title in place of its label; a title holding markup, such as emphasis or a link, keeps the label and stays in the body. A status bar carries the serving address, document path, and reading time. Below 900px the sidenav becomes an off-canvas drawer behind the appbar toggle. When a watched file changes, the open page updates in place: the reading position, opened folders and collapsibles, and a live filter all survive the update.

## Doc discovery

With no arguments, `serve`, `list`, `check`, `read` and `search` all index cwd, `./docs` (if present), and `$CADENCE_FIELD_REPORTS_DIR` (if set), plus any extra roots configured in the `[docs]` section of `config.toml`:

```toml
[docs]
roots = ["~/Projects/forgectl/docs"]   # extra indexed roots, beyond the defaults
addr  = ""                              # default bind address for `docs serve`
search_backend = "ripgrep"              # `docs search` backend: "ripgrep" (default) or "qmd"

[docs.root_kinds]                       # override per-root link semantics (see below)
"/absolute/path/to/notes" = "vault"
"." = "docs"                            # relative to where forgectl runs
```

A leading `~` or `~/` in `roots` and in the `root_kinds` keys expands to your home directory. Paths named on the command line are not expanded; your shell does that.

Naming directories or files on the command line replaces that default set entirely.

### Exit codes and errors

Every docs verb shares one "could not run" contract ([#604](https://github.com/cameronsjo/forgectl/issues/604)):

- **Exit 2** when the verb fails before it starts its real work: an unreadable or missing root, a bad `[docs]` config, a bad flag or a wrong number of arguments, a `--timeout` deadline, no search backend, no reader to open, a bind failure, an unusable `--token-file`, the retired `--token`, or a `docs serve` startup in which the server cannot confirm it is serving its own discovery generation.
- **Under `--json`**, on the verbs that declare it (`list`, `check`, `search`), that failure leaves stdout empty and writes exactly one `{"error","code","root"}` object to stderr. This includes flag and argument errors, even a `--json` written after the bad flag. `root` is always present and is empty unless a deadline stopped on a specific root. Verbs without `--json` (`serve`, `open`, `read`) print the human error and exit 2.
- The object's shape is additive-only ([ADR-0008](../adr/0008-agent-contract.md) rule 2): `root` was already documented for `docs list`, so it stays a fixed key rather than being omitted when empty, and `docs search` gained it.

Two things sit outside the contract, on purpose:

- **Exit 1 means the verb ran.** `docs check` with at least one `error`-severity finding (an info-only run exits 0), and `docs search` when rg could not fully search a root, did run; their output is complete on stdout. Under `--json` a partial `docs search` also writes one `{"error","code","root"}` object (code 1, `root` empty) to stderr.
- **A partial tree is a finding-level exit 2, not a pre-work failure.** When the index walk skipped an unreadable path, `docs check` did run and its report is complete for what it could read, so the report goes to stdout as usual (human lines, or the JSON object with its `skipped` array) and the exit code is 2. Under `--json` it writes no stderr error object, since that shape is reserved for verbs that produced no result; without `--json` a one-line reason goes to stderr.
- **`docs read` passes mdroll's exit status through** (see [`docs read`](#docs-read-in-the-terminal)), so a 2 from `read` is ambiguous once mdroll has started. The contract applies only before the child does; a failure to start mdroll is still a 2.

In short: 2 = could not run; 1 = error findings or a partial search; 0 = clean, or `docs check` with only `info` findings.

`docs serve` exits 0 after a clean Ctrl-C. Discovery has two failure modes. When the server cannot confirm it is serving its own discovery generation (its startup self-probe gets no answer, discovery publication reports success without a lease, or picking its initial discovery generation fails before the server starts), startup is aborted before the banner and it is "could not run" (exit 2). A failed discovery-record write, an ordinary publish error, failing to pick a fresh generation after a collision, or running out of collision retries only prints a warning: the server keeps serving without being discoverable by `docs open`, and exits 0 on a clean Ctrl-C. A failure after the server is up (the serve loop itself failing) is the server's own and exits 1.

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
| `deprecated` | the doc's YAML frontmatter says `status: deprecated` (exact, lowercase). Severity `info` |
| `stale` | the doc's YAML frontmatter `stale_after` is an RFC 3339 instant with an explicit offset (`2026-09-23T00:00:00Z`) and now is at or past it; a date-only (`2026-09-23`) or offset-less value is ignored; a TOML `stale_after` is ignored too |

- **Existence fallback.** A link to a directory (`commands/`) or a non-markdown file (`LICENSE`, an image) resolves to no indexed doc. It is reported as broken only when nothing exists at that path inside the root. The check reads nothing and refuses a symlink that escapes the root. It checks existence only: a `#fragment` on such a link is not checked, whether the target is a non-indexed file that exists on disk (`LICENSE#x`, `diagram.png#page=2`) or a directory (`commands/#x`), so those links never produce a `broken_anchor`.
- **Out-of-root links are counted, not reported.** A link such as `../../README.md` that leaves its root works on GitHub, so it is not a finding. It is counted in `summary.outside_root_links`.
- **Orphans.** A root-level `README` or `index` page is never an orphan, and a single-file root has no orphans. A `README.md` in a subdirectory is an ordinary doc, but a directory link from another doc (`[plans](plans/)`, or root-relative `/plans`) counts as a link to that directory's `README` or `index` page (any case, any indexed extension), the page GitHub shows for it. A target ending in `/`, or in a `.` or `..` segment, always names the directory, even when a `plans.md` sits beside it. Only the orphan check counts it this way: the link still resolves to no doc in the reader. Other docs in that directory still need a link of their own.
- **Opting a page out of the orphan check.** A page nothing is expected to link to (a point-in-time snapshot, a scratch note) can say so with `orphan_ok: true` in its YAML frontmatter. Only the boolean `true` counts: `false`, `"true"` and `yes` do not, so a typo leaves the orphan reported. The page's broken links, trust signals and everything else are still checked. Each suppressed orphan is counted in `summary.ignored_orphans` instead of appearing as a finding. The reason belongs in the page or its commit; there is no config-file or flag form, so the opt-out travels with the page into CI and other checkouts.
- **Vault roots are checked for links.** A `vault` root gets `broken_link`, `ambiguous_link` and `broken_anchor`, resolved by the same vault rules the reader uses (wikilink by name, path or alias; heading text or slug; `^block` id), with no `obsidian` subprocess. A wikilink to an attachment that exists (`[[assets/logo.png]]`) is not broken. It never reports `orphan` (daily and inbox notes are orphans by nature), `deprecated` or `stale` there, and `orphan_ok` has nothing to suppress. A vault-only invocation is checked like any other.
- **Size cap.** `docs serve` does not render a doc over 1 MiB (the same limit the index scan uses); it serves a notice naming the doc and suggesting `forgectl docs read <root>/<path>`. The cap bounds memory and the per-request read, not render CPU. An unreadable subdirectory, a file that vanishes during the index walk, or an entry whose symlinks cannot be resolved is skipped instead of failing the build, and an unreadable root still fails. Every read verb (`serve`, `list`, `read`, `search`) prints `skipped N unreadable path(s) under <root> (see docs check)` on stderr whatever the log level, except under `--json`, where stderr is reserved for the one error object (see [Exit codes and errors](#exit-codes-and-errors)): there `docs search` lists the skipped paths in its response's `skipped_paths` array instead, and `docs list`, whose output is a bare array with no room for them, says nothing. `docs check` refuses to vouch for the partial tree (below).
- **Trust signals.** `deprecated` and `stale` follow the Open Knowledge Format v0.2 §5.4/§5.5 (SPEC at `ad30107`). Both are findings, but they weigh differently: `stale` has severity `error` and exits 1 like every other finding, while `deprecated` has severity `info` and does not fail the check, because a deprecated page is kept on purpose and its own `status: deprecated` already states the author's intent. The reader also badges them, in the properties block and in the status bar. OKF changed `stale_after` from a date to a datetime inside v0.2 without a version bump; date-only values written against the older text are ignored, per the current spec and its reference implementation. Coverage gaps: vault roots get no trust findings (see above), though the reader still badges their docs, and a doc over 1 MiB is indexed by title only, so it gets no finding and no status-bar badge, though its properties block still badges.
- **Severity.** Every finding carries `severity`: `error` (all kinds except `deprecated`) or `info` (`deprecated`). Only an `error` finding sets exit 1. When findings exist the error text reads `docs check: N finding(s), M informational`; an info-only run exits 0 and prints `docs check: M informational finding(s), no errors` to stderr. The `verified`/`generated` trust tiers (OKF §5.2/§5.3) are not read, checked or badged: that is deferred until a corpus sets them.
- **Exit codes.** 0 clean, or only `info` findings; 1 at least one `error` finding (the complete report is on stdout); 2 the check could not run (unreadable root, `--timeout` deadline, bad flag), under the shared contract above, or could not vouch for the tree: the walk skipped an unreadable path, so docs inside it went unchecked and links into it would read as broken (see [Exit codes and errors](#exit-codes-and-errors)). The skipped paths are listed as `<root>/<path>: skipped (<reason>)` after the findings, or under `--json` in the additive top-level `skipped` array of `{root, path, reason}` (always present, empty when nothing was skipped; `reason` is the bare cause, such as `permission denied`, never a path). Without `--json` the stderr summary also counts any error findings. Exit 2 outranks exit 1. Skips under a vault root are reported too.

Human output is one line per finding, `<root>/<path>: <kind> <target>`; a link finding (`broken_link`, `ambiguous_link`, `broken_anchor`) names its source line as `<root>/<path>:<line>: <kind> <target>`, and a `stale` line ends with its `stale_after` value instead of a target. `line` is 1-based and counts every line of the file as written, frontmatter included, so it can feed a CI annotation directly. Findings are grouped by root, in configured order, then by path. Within a file, link findings come first, sorted by line, and the lineless findings follow, sorted by kind. `--json` prints one object:

```json
{
  "schema_version": 1,
  "roots": [{"label": "docs", "kind": "docs", "checked": true, "docs": 42}],
  "findings": [
    {"kind": "broken_link", "severity": "error", "root": "docs", "path": "plans/x.md", "target": "gone.md", "line": 12},
    {"kind": "orphan", "severity": "error", "root": "docs", "path": "notes.md"},
    {"kind": "stale", "severity": "error", "root": "docs", "path": "runbook.md", "stale_after": "2026-09-01T00:00:00Z"}
  ],
  "summary": {"broken_links": 1, "ambiguous_links": 0, "broken_anchors": 0, "orphans": 1, "ignored_orphans": 0, "outside_root_links": 20, "deprecated": 0, "stale": 1}
}
```

The shape is additive-only ([ADR-0008](../adr/0008-agent-contract.md) rule 2): new keys and finding kinds may appear, existing ones are never renamed or removed. A vault root is `checked: true` like any other (before vault roots were checked it carried `checked: false` and a `skipped` reason; `skipped` is still omitted when empty), and roots never carry an absolute path.

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

The default backend is [ripgrep](https://github.com/BurntSushi/ripgrep) (`rg`), which must be on `PATH`. [qmd](https://www.npmjs.com/package/@tobilu/qmd) is an opt-in ranked backend, described under [The qmd backend](#the-qmd-backend) below.

- **Only indexed docs are returned.** Every hit rg reports is checked against the docs index, the same membership gate the reader serves through, so a file outside a root, under an excluded directory (`.git`, `node_modules`, `vendor`, any dot-directory), or reached through a symlink never appears. Hits dropped this way are counted in `skipped`.
- **rg's own config file is ignored.** rg runs with `--no-config`, so `RIPGREP_CONFIG_PATH` cannot turn on `--follow` or otherwise change what is searched.
- Files over 1 MB are not searched, and at most 5 hits are taken from one file.
- Docs whose paths are not valid UTF-8 are not searchable; rg can only report such a path as raw bytes, and those hits are counted in `skipped`.
- **Results are ordered and stable.** Roots are searched in their configured order, and rg walks each root in path order (`--sort=path`, which also keeps rg to a single worker), so the same query over the same tree returns the same results, and `--limit` always keeps the same prefix. A doc reachable through two overlapping roots (cwd and `./docs`, say) is returned once, under the first root.

`--json` prints one object to stdout. `backend` names the backend that ran, either `ripgrep` or `qmd`. `results` is always an array, and each result carries `root`, `path`, `title`, `line`, and `snippet`. `truncated` is true when more hits existed past `--limit`, or, under qmd, when qmd's result window came back full, so under qmd it can be set with fewer than `--limit` results. Human output then ends with a note on stderr that more matches may exist. `errors` is always an array of `{root, message}`, one per root rg could not fully search (an unreadable file, say, or output that could not be parsed). `skipped_paths` is always an array of `{root, path, reason}`, under either backend: the same `{root, path, reason}` entries `docs check` reports in `skipped`. They are paths the index walk could not read, so docs under them were never searched. It is a separate key from `skipped`, which counts dropped hits. `reason` is the bare cause (`permission denied`), never a path.

```json
{"backend":"ripgrep","query":"needle","results":[{"root":"docs","path":"guide.md","title":"Guide","line":12,"snippet":"the needle in the guide"}],"truncated":false,"skipped":0,"errors":[],"skipped_paths":[]}
```

Exit codes: no match exits 0 with an empty `results` (human output says `no matches` on stderr). When rg could not fully search a root, the other roots are still searched and every hit found is printed, then the command exits 1 with the reason on stderr: one line per failed root, or under `--json` one `{"error","code","root"}` object on stderr alongside the full response, `errors` included, on stdout. A missing `rg`, an empty or invalid query, a bad `--limit`, a root or config error, or an expired `--timeout` exits 2; under `--json` that leaves stdout empty and writes exactly one `{"error","code","root"}` object to stderr (the shared contract above). The partial-result object is the same shape with code 1.

### The qmd backend

`--backend qmd`, or `search_backend = "qmd"` in the `[docs]` section, runs the query through qmd's BM25 search instead of rg. The flag overrides the config key. forgectl never picks qmd because it is installed. It runs qmd only when you ask for it, and `backend` in the `--json` response says which one ran. A `search_backend` value other than `ripgrep` or `qmd` is a config error, and `forgectl launch doctor` reports it.

forgectl runs `qmd search --json --full-path -n <N> -- <query>` with no shell, using the absolute path `PATH` resolves to. A qmd reachable only through a relative `PATH` entry is refused and treated as absent. forgectl never runs `qmd query`, which loads local language models and can download them.

- **qmd searches its own collections, not the roots.** qmd has no machine-readable way to say which directory a collection covers, so forgectl skips that mapping. It searches qmd's default collections, then keeps only the hits that name a doc in the docs index, at the exact path under a root, through the same gate rg hits pass. A hit outside every root, a file under a root that the index does not hold (qmd's index can be stale), an excluded directory, a `../` escape, or a file qmd could only report as a `qmd://` URI is dropped and counted in `skipped`. So is a hit under a path the index walk could not read, and that path is listed in `skipped_paths` as under rg. The title comes from the docs index, not from qmd. A doc reachable through overlapping roots is returned under the first root.
- **qmd ranks before forgectl filters.** qmd applies `-n` across all its collections, so forgectl asks for 10 times `--limit`, capped at 1000 (and never less than one more than `--limit`, up to 100000). When qmd returns that whole window, `truncated` is set even if fewer than `--limit` hits survive the gate, because qmd ranked hits it never printed.
- **Results follow qmd's ranking**, not path order, and a query is a set of BM25 terms rather than rg's fixed string. The snippet is qmd's, with its `@@ … @@` header removed and its lines joined.
- **qmd's output must be exactly one JSON array.** Text before or after the array, a second JSON value, or output over 8 MB is refused rather than guessed at. That failure, a non-zero qmd exit, and a missing qmd all exit 2 under the shared contract above. qmd has no per-root partial result, so it never exits 1.

The pure-JSON premise was verified against qmd 2.8.3: `search --json --full-path` prints only a JSON array (or `[]`) to stdout and sends its warnings to stderr, including for queries that look like flags or carry quotes or parentheses. If another qmd version prints anything else to stdout, `docs search --backend qmd` fails closed with exit 2 and returns nothing.

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
