---
updated: "2026-09-04"
body_sha256: "95d133d55290c50f0160881e8b493ce97756a88df79627be619b299d692ff67a"
session: "swift-spindle"
session_id: "16d577be-0ac9-4639-8497-907d22a67043"
model: "claude-fable-5-1"
harness: "claude-code 2.1.261"
machine: "cf6e768835c7"
approved_in: "woven-lyre"
approved_session_id: "1488f160-f360-414b-952e-1f3846a08602"
status: proposed
tier: substantial
kind: roadmap
repo: forgectl
target_path: docs/plans/2026-09-04-docs-reader-roadmap.md
branch: plan/docs-reader-roadmap
---

# forgectl docs reader — Roadmap (2026-09-04)

## Context

`forgectl docs` is a read-only, loopback HTML markdown reader (goldmark + chroma + mermaid, Artificer-styled, SSE live reload) viewed in a browser or embedded in a cmux pane. It renders GFM, callouts, frontmatter properties, and an outline, and indexes multiple roots including `$CADENCE_FIELD_REPORTS_DIR`, which is an Obsidian vault. It has no search, no wikilink resolution, no link or orphan checking, and reload loses reading position.

**Frame.** Open any doc set, a repo's `docs/` or the Compendium vault, and read it as the author intended, find anything in it, and trust its links, in a browser, a cmux pane, or the terminal, without an editor.

Ideas were borrowed from JetBrains Markdown Pro (site intelligence, Obsidian vault support, preview fidelity) and tokuhirom/mdroll (text-first terminal rendering, search keys, link picker, TOC pane). Editing, format conversion, and style inspections were declined (see Alternatives declined).

**Constraint.** The reader stays read-only for the roadmap's horizon. Wikilink *completion*, table *editing*, or checkbox *toggling* are out of scope; the first of those to enter is a new product decision, not a roadmap item.

**Adopt before build.** Parsing is library work: `go.abhg.dev/goldmark/wikilink` handles `[[link]]`, `[[note|alias]]`, `![[embed]]`, and `#fragment` with a pluggable `Resolver`, and forgectl already depends on the same author's `goldmark/frontmatter`. Obsidian 1.12 ships a CLI (`obsidian unresolved|orphans|backlinks|links|search`) that covers the vault half of link checking. What has no library is Obsidian's *resolution* semantics in Go, and that is what #1 builds.

## Critical path (the spine)

#1 → #3

The spine is the pair that makes the vault render as its author sees it: nothing else in the reader can stand in for link resolution, and the vault is the reader's primary target. The scariest piece is **#1**: Obsidian's shortest-unique-path resolution, alias tables, and case rules, plus reverse-index cost on a large vault. It gets a spike before any UI work.

## Now

| # | Item | Scope | Depends on | ~ |
|---|------|-------|-----------|---|
| #1 | Link resolution substrate | Methods on the existing `Index` in `internal/docs/index.go`, reusing `Doc`, `Root`, and `walkRoot`: `ResolveLink(from *Doc, target string) (*Doc, Miss)` and `Backlinks(*Doc) []*Doc`, built at index time and refreshed by the existing fsnotify watcher. The name avoids the existing security-load-bearing `Index.Resolve` (`index.go:431`). Link forms come from `goldmark/wikilink`'s parser plus goldmark's own relative-link and `#anchor` nodes (GitHub slug rules); this item writes only the `Resolver`. Obsidian rules for vault roots: shortest-unique-path, case-insensitive, frontmatter `aliases`, `#heading` and `#^block` targets. **Root kinds:** a root whose directory contains `.obsidian/` is `kind: vault`, overridable in config; every other root is `kind: docs`. **Resolution is within-root only**: no cross-root fallback in any kind, so a repo's `docs check` can never surface a vault's note titles. A `Miss` is typed (`NoTarget`, `Ambiguous`, `OutsideRoot`) so the renderer can mark broken links distinctly; that visible marking is what "trust its links" means in the reader, and batch reporting is #2. **Spike first, resolution only:** plug a resolver into `wikilink`, resolve every link in the live vault and in `forgectl/docs`, report miss rate by `Miss` kind and index time and memory, and commit the read-out to #1's plan doc before any visible wiring. | — | ~ |

## Next

| # | Item | Depends on | ~ |
|---|------|-----------|---|
| #3 | Obsidian rendering: `goldmark/wikilink` wired to #1's resolver, transclusion, and the flavor no library covers (`==highlight==`, `%%comment%%`, tag chips, callout aliases); evaluate `powerman/goldmark-obsidian` at attune | #1 | ~ |
| #2 | `docs check` for `kind: docs` roots: broken links and orphans, `--json`, non-zero exit; vault roots delegate to `obsidian unresolved`/`orphans` when that CLI is on PATH | #1 |  |
| #4 | Full-text search: reader UI and `docs search --json`; shell out to `qmd query --json` when `qmd` is on PATH and the root is a registered qmd collection (detect, never register), else `rg`; no in-process index | — |  |
| #5 | `docs read <file>`: `mdroll` launcher shaped like `internal/cli/surface_backend.go`, a `doctor` row via `checkBinary` with `StateSkip`, HTML fallback | — |  |

## Later

- **#6** Preview fidelity: scroll-preserving SSE reload, KaTeX, `<details>` through bluemonday, copy-as-rich-text. Polish; waits because each is independent and none blocks reading.
- **#7** Backlinks panel in the reader, fed by #1's reverse index. Obsidian's own app and `obsidian backlinks` cover the data permanently; the only added value is seeing it inside the reader, so it waits for demand. A local graph is not planned.
- **#8** Knowledge-bundle trust signals: `docs check` flags frontmatter `stale_after` in the past and `status: deprecated`; the reader badges them. Open Knowledge Format v0.2 (Google, emitted by langchain-ai/openwiki) uses exactly those keys and the properties block already renders them. Read-only, no dependency; verify the OKF field names at attune before building.
- Upstreaming `.props`, `.callout`, `.embed`, and `.tree--dense` into Artificer proper (carried from the v2 plan, unchanged).

## Alternatives declined

- **Building a search index** (bleve, in-memory): qmd (`@tobilu/qmd`, MIT, local hybrid BM25 + vectors + reranker) is installed on this machine and already indexes the Compendium vault as collection `compendium`; forgectl consumes it and falls back to `rg`. Registering collections is a write to qmd state and stays out of the read-only reader.
- **graphify as a dependency**: graphify is docs-first (Claude-driven concept-and-relationship extraction over markdown and PDFs, tree-sitter for code), and its graph is LLM-extracted concepts with edges tagged EXTRACTED / INFERRED / AMBIGUOUS. That is a different artifact from #1's deterministic document-link graph, so it neither replaces #1 nor reopens the graph-view call. Its `graphify-out/wiki/` and `graphify-out/obsidian/` outputs are plain markdown with an `index.md`, so forgectl already renders them by adding the directory as a root; nothing to build.

- **Editing features** (Markdown Pro's source styling, table controls, list renumbering, live templates, smart paste): the reader is read-only; editing is a different product.
- **Format conversion** (Morph, AsciiDoc/rST/Typst): pandoc exists.
- **Style inspections** (heading order, trailing whitespace, image alt): markdownlint and vale exist. Link and orphan checking for the vault is covered by the Obsidian CLI; #2 is scoped to repo roots, where no tool sees forgectl's index.
- **Hand-writing a wikilink parser**: `go.abhg.dev/goldmark/wikilink` v0.6.0 already parses every form with a resolver seam; only the resolver is built here.
- **Porting Obsidian resolution from another project**: `jackyzha0/hugo-obsidian` has the semantics but is a `main` package with no importable API.
- **Cross-root link fallback** (repo link falls through to the vault): convenient, but it leaks vault note titles into repo output and makes `Ambiguous` hard to define; within-root only.
- **Revive the native Go TUI** (`feat/docs-tui-reader`, kitty graphics, abandoned 2026-08-29 as "not competitive with the existing HTML renderer"): mdroll already delivers the text-first terminal design that branch never tried, in Rust under MIT. Adopt it as an optional renderer (#5) rather than port it.
- **HTML-only, no terminal track**: loses the no-browser reading case; #5 covers it at near-zero build cost.
- **Search on the spine**: `rg` is a workaround, so the cancellation test leaves it supporting; it stays high in Next.
- **Local graph view**: Obsidian owns it; no reader value beyond novelty.

## Verification / done

- #1 spike read-out (miss rate by kind, index time, memory) on the live vault and on `forgectl/docs`, committed to #1's plan doc.
- Each item lands through its own `cadence:attune` pass, plan, and PR; this roadmap is the index and carries scope detail only in Now.
- The vault's field reports render with working wikilinks and embeds in `docs serve`; broken links are visibly marked; `docs check` reports zero unresolved links on `forgectl/docs`.
- **Tell-the-story test:** index the roots and resolve every link within its root (#1), render the vault the way Obsidian does (#3), report what is broken or orphaned in repo docs (#2), let the operator find anything (#4), and read it in the terminal when no browser is wanted (#5); then keep the page steady while it reloads (#6), show what points here (#7), and flag what has gone stale (#8).

Peer consultation: the `graph-based-wiki-stuff` session (running the wiki/graph tooling survey) was asked whether qmd and graphify belong here; its verdict shaped #4, #8, and the graphify decline.

## Risks / open items

- Obsidian resolution semantics are documented informally; the spike settles them against the live vault rather than the docs.
- `powerman/goldmark-obsidian` would be a third-party extension in the render path. Output still passes bluemonday, so the exposure is parse-time robustness, not HTML injection; #3's attune vets it (`cadence-forge:vet-dependencies`) before adoption.
- Whether `docs check` is worth keeping for vault roots once `obsidian unresolved` exists is an ergonomics call (one command across all roots); #2 defaults to delegating and Cameron can drop the vault arm at attune.
- The reverse index adds per-doc metadata; the spike measures it before an in-memory structure is chosen.
- `mdroll` is a 55-star single-maintainer project (last push 2026-08-01); #5 treats it as optional and never a required dependency.

## Panel

Panel: plan-reviewer, cameron-review ran — 12 findings, 11 folded in, 1 declined

## Panel review findings declined

- cameron-review #2 (drop #7 outright because `obsidian backlinks` covers it): declined in part. The CLI supplies the data, not an in-reader panel; #7 stays in Later with the reduced claim and the local graph dropped.

## Orchestrator

**Driver:** sonnet

Each item is picked up separately through `cadence:attune`; the roadmap itself has no implementation to drive. The #1 spike is Sonnet-drivable from its own plan; the resolver-semantics read-out returns to the seat.
