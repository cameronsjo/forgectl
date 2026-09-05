# forgectl docs — local markdown reader: render + serve an indexed doc set over loopback HTTP

> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).

`docs` is forgectl's local markdown reader ([forgectl#93](https://github.com/cameronsjo/forgectl/issues/93)): pure-Go server-side rendering (goldmark+GFM, class-based chroma highlighting, bluemonday sanitization), Artificer-themed, served over loopback HTTP so it behaves the same whether you're at the machine or SSH'd in from the headless workbench — no terminal-specific rendering, no popping between windows.

```sh
forgectl docs serve [dir|file ...]       # render + serve, loopback-only (DNS-rebinding-safe)
forgectl docs serve --open               # also open the system browser
forgectl docs open [path]                # point the browser at a doc on the already-running reader
forgectl docs open --print-url [path]    # print the resolved URL instead of opening a browser
forgectl docs list [dir|file ...]        # list the indexed docs, no server (--json for scripting)
```

Diagrams render in the page: a fenced code block tagged `mermaid` becomes a live diagram themed from the same Artificer tokens as the rest of the reader, and both those and inline SVG pan and zoom (drag to pan, modifier-scroll or click-then-scroll to zoom, double-click, `0`, or the diagram card's reset button to reset).

The reading surface: the sidenav renders each indexed root as a collapsible directory tree (per-directory counts, current path pre-expanded, filter box that hides empty branches), and a document's YAML/TOML frontmatter renders as an always-visible properties block above the body instead of leaking into it. Longer documents get an "On this page" outline — a third column on wide viewports, an inline disclosure on narrow ones — GFM alert blockquotes (`> [!NOTE]` and kin) render as tiered callouts, and a status bar carries the serving address, document path, and reading time. Below 900px the sidenav becomes an off-canvas drawer behind the appbar toggle.

## Doc discovery

With no arguments, both `serve` and `list` index cwd, `./docs` (if present), and `$CADENCE_FIELD_REPORTS_DIR` (if set), plus any extra roots configured in the `[docs]` section of `config.toml`:

```toml
[docs]
roots = ["~/Projects/forgectl/docs"]   # extra indexed roots, beyond the defaults
addr  = ""                              # default bind address for `docs serve`

[docs.root_kinds]                       # override per-root link semantics (see below)
"~/Projects/forgectl/docs" = "docs"
"." = "vault"
```

Naming directories or files on the command line replaces that default set entirely.

### Root kinds

Every root is classified as `docs` or `vault` when it is indexed. A root is a `vault` when it, or a directory above it (stopping at your home directory, a filesystem boundary, or `/`), contains an Obsidian `.obsidian/` folder; anything else is `docs`. The kind decides how links inside that root resolve:

| Kind | Link target | Anchor |
|---|---|---|
| `docs` | relative markdown path from the linking file (`[x](../guide.md)`) | GitHub-style heading slug (`#getting-started`) |
| `vault` | wikilink by vault-relative path, bare note name, or frontmatter alias (`[[Note]]`, `[[folder/Note]]`); a `./` or `../` markdown path resolves from the linking file | heading text or slug (`#Some Heading`), or a block id (`#^blk-1`) |

Links never resolve across roots. `[docs.root_kinds]` forces a kind when detection gets it wrong, keyed by the root path as you wrote it in `roots` or on the command line — relative spellings such as `.` match the same directory the CLI derives. A value other than `docs` or `vault` is a config error, and `forgectl launch doctor` reports it.

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

## `docs open` steers, never starts

`docs open` points the system browser at an already-running `docs serve` reader; it never starts one. That is a deliberate boundary: `docs serve` is a foreground process the operator owns — it prints its address, holds the terminal, and stops on Ctrl-C. If `open` could spawn one, it would either fork a server nobody can see or block the terminal it was called from, and either way the operator would no longer know how many readers exist or which one their browser is pointed at. When nothing is running, `open` says so and names the command to run.

It uses the system browser, never a terminal's own browser command — the reader's entire premise is being terminal-agnostic (reachable from the machine, from an SSH session, from a phone), so coupling `open` to one terminal emulator would undo that.

A legacy server (predating generation-owned discovery) has no freshness endpoint, so `open` cannot verify the listener at its recorded address is still the same server before handing it a token — it prints the URL and tells you to restart with `forgectl docs serve` instead.

How discovery records are written, where they live on disk, and how to clear them by hand after a crash: [docs server discovery — operations](../operations/docs-discovery.md).
