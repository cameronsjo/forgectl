---
status: in-flight
branch: fix/docs-frontmatter-and-tree
next: design-iterate the docs reader (tree density, frontmatter treatment, code-block fit) — read this stub, open `forgectl docs serve`, compare against the Artificer live-spec
---

# The docs reader as a design surface

## Continuation

Design-iterate the docs reader: tree density, frontmatter treatment, code-block fit. Open `forgectl docs serve` on real docs and judge against the Artificer live-spec.

Branch: fix/docs-frontmatter-and-tree
PR: #430 (this branch); #429 (Artificer 0.19.0 -> 0.25.0 re-vendor + docs slate, ready)

### What the surface is

`forgectl docs serve` is a pure-Go, server-rendered markdown reader on loopback HTTP. The shell is an Artificer surface-tool layout: appbar + split-pane + sidenav on the left, `surface-document` main pane. Assets are vendored Artificer (0.25.0 once #429 merges), embedded via `internal/docs/assets.go` — only `artificer.css`, `artificer-theme.js`, and `artificer-tree.js` from the vendored set are served. Two hard constraints shape every design change:

- CSP sends `script-src 'self'` — no inline scripts ever; any behavior must be a served asset (see `assets/sidenav-filter.js` for the precedent).
- `style-src` allows inline styles, so template-level `<style>` blocks and `style=` attributes are legitimate.

### What shipped 2026-08-31

- Frontmatter renders as a collapsed `Front matter · N keys` disclosure — accordion wrapper + `.kv` grid. Treatment "A" (collapsed) chosen by Cameron over open-card "B" and meta-strip "C".
- The sidenav renders each root as a `.tree--static` (nested details/summary): per-directory counts, path to the current doc pre-expanded, filter-aware via `assets/sidenav-filter.js`. Recent stays a flat list.
- Syntax highlighting maps chroma's classes onto Artificer syntax roles in `assets/chroma.css` (replaced hardcoded monokai; follows light/dark).

### Design levers still open

- `.sidenav a` min-height makes tree leaf rows taller than the 28px `.tree__row` intent — flagged in review, deliberately unchanged; needs a design-fit judgment.
- Duplicate `aria-current` — the current doc is marked in both the Recent list and the tree.
- The tree--static backing CSS (twisty rotation, count styling) lives as an inline `<style>` block in `templates/shell.html.tmpl` — candidate to upstream into Artificer proper.
- The chroma role mapping is a first pass: four roles use `var()` fallbacks pending upstream token minting (successBright, attentionAlt, cyan, urgentBright — noted in `artificer.css` around line 868).
- Mermaid diagrams + SVG pan/zoom exist and were untouched this round.
- The static prototype that drove the treatment choice was `/tmp/docs-reader-proto/index.html` (ephemeral); the shipped code on this branch is now the reference.

### Key files

- `internal/docs/templates/shell.html.tmpl` — shell, recursive `treeNodes` template, tree CSS backing.
- `internal/docs/render.go` — goldmark -> bluemonday pipeline; `frontmatterHTML` builds the disclosure post-sanitizer from escaped fragments only.
- `internal/docs/assets/chroma.css` — the Artificer token mapping for syntax highlighting.
- `internal/docs/assets/sidenav-filter.js` — filter box; hides empty branches, auto-expands matches.
- `internal/docs/server.go` — `buildTree` / `buildGroups` shape the sidenav data.

### Artificer references

- Tree and `tree--static` recipes: design-system live-spec `components-extended.html`.
- `.kv` and accordion recipes: live-spec `data-display.html` and `components.html`.
- Upstream feedback already filed: artificer-design-system#447 (`vendor --check`), #448 (ledger gaps from the 0.25.0 crossing).

### Open questions

- Upstream the tree--static twisty/count CSS into Artificer, or keep it local to forgectl?
- Tree leaf row height: 44px touch target vs the 28px density intent — needs Cameron's eye on the live surface.
- Run the full artificer-design-system sync into claude.ai/design for component-true prototyping? Declined this session in favor of a local static prototype.
