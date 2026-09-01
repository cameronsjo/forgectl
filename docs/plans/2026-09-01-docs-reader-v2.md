---
status: "in-flight"
updated: "2026-09-01"
branch: "feat/docs-reader-v2"
session: "deft-sonata"
session_id: "7fc5913c-2346-479f-a249-9d871812e47d"
machine: "cf6e768835c7"
next: "execute the checklist below; visual check against the claude.ai/design prototype"
---

# Docs reader v2 — design handoff (execution plan)

Contract: the design handoff below, pasted verbatim by Cameron from the claude.ai/design
session ("In-browser markdown viewer design" → Docs Reader v2.dc.html) on 2026-09-01.
It supersedes the open levers in `2026-08-31-docs-reader-design-surface.md`.

Panel: none — externally designed prototype; every treatment was judged live behind
Tweaks props in the design session, and the operator pasted it as ready.

## Orchestrator

Driver: sonnet — implementation of a settled design spec (running seat: Fable, above floor)

## Contract (verbatim handoff)

> status: ready · source-prototype: claude.ai/design — "In-browser markdown viewer design" → Docs Reader v2.dc.html · branch-suggestion: feat/docs-reader-v2 · depends-on: PR #429, PR #430 (both merged before this plan was cut)

Design iteration on `forgectl docs serve`, judged live against the Artificer 0.25.0 live-spec plus the Obsidian-theme project as typographic inspo. Everything below is CSP-compatible: no inline scripts; all behavior is served assets. Inline `<style>`/`style=` remain fine (`style-src` allows them).

### Verdicts on the open levers (from the previous handoff)

1. **Tree leaf row height: 24px, not 44/34/28.** The Obsidian-density option won. Add to the shell `<style>` block (or upstream):
   `.sidenav .tree__row { min-height: 24px; font-size: 13px; }` · `.sidenav a.tree__row { min-height: 24px; }` · `.sidenav > a { min-height: 24px; font-size: 13px; }` · `.sidenav > a, .sidenav > .sidenav__group, .sidenav > ul { flex: none; }`
2. **Duplicate `aria-current`: drop it from Recent, keep it in the tree.** One marker per page; Recent is a shortcut list, not location.
3. **Code-block fit: keep scroll (no wrap).** `pre` keeps `white-space: pre; overflow-x: auto`. No change.

### Frontmatter as Properties (replaces the disclosure)

Treatment "A" (collapsed disclosure) is superseded by an Obsidian-style properties block — always visible, compact, no interaction cost:

- Container `.props`: `bg color-mix(in oklch, var(--bg-raised) 60%, transparent)`, 1px `var(--border)`, `var(--radius-md)`, mono 13px.
- Rows `.props-row`: flex, `6px 12px` padding, border-bottom between rows.
- Key cell `.k`: fixed 110px, 12px `var(--fg-secondary)`, an 11px stroke icon per known key (status→clock, branch→git-branch, next→arrow; fall back to a dot for unknown keys).
- Value cell `.v`: enum-ish values (status) render as `.chip` — `2px 8px`, 4px radius, `color-mix(in oklch, var(--accent-fill) 14%, transparent)` bg, `var(--accent)` text, 11px. Paths/branches get `tabular-nums`.
- Build in `render.go` the same way `frontmatterHTML` does today: post-sanitizer, escaped fragments only. Key→icon map is a Go map; unknown keys degrade gracefully.

### Note typography for the document pane

`surface-document` gets a "note" treatment (CSS only, in the shell template):

- Measure: `max-width: 820px`.
- Body: 16px / 1.65, `letter-spacing: 0.01em; word-spacing: 0.12em`.
- Headings: `var(--font-mono)` 700, `var(--accent)`, tight `-0.01em`; h1 28px, h2 22px, h3 18px; `margin: 32px 0 8px`.
- Inline code: `var(--accent)` on transparent (no pill bg).
- Callouts (GFM `> [!NOTE]` etc.): `.callout` — 2px left border, `color-mix(in oklch, <tier> 8-10%, transparent)` bg, radius on the right corners only; title row is mono 700 13px with a small stroke icon. Tier colors: note→accent, warning→attention, danger→urgent, tip→success.

### Outline pane ("On this page")

- Wide (>1100px): third grid column, 220px, `border-left: var(--border)`; mono 12px items, h2 indented 14px, current-ish item `var(--fg)`, rest `var(--fg-secondary)`. Built server-side from the heading tree goldmark already has.
- Narrow (≤1100px): collapses into an inline `<details class="outline-inline">` at the top of the document — native details, CSS twisty, no JS.

### Status bar

Bottom strip, mono 11px `tabular-nums`, `var(--fg-secondary)`, 1px top border. Items left→right: live dot (`var(--success)`) + `serving · 127.0.0.1:<port>`, doc path, spacer, docs-indexed count, `<words> words · <n> min`, `markdown`. All server-rendered. Items other than word count get a `.opt` class.

### Narrow screens

Grid, not split-pane, three breakpoints:

- **>1100px**: `280px | minmax(0,1fr) | 220px` (nav / doc / outline).
- **≤1100px**: outline column drops (inline details takes over): `280px | minmax(0,1fr)`.
- **≤900px**: sidenav becomes a fixed off-canvas drawer — `width: min(300px, 85vw)`, `var(--bg-raised)`, shadow, `transform: translateX(-100%)` → open `none`, 200ms; scrim `rgba(0,0,0,.55)` closes it. Toggle button (sidebar icon, 32px) sits left of the wordmark in the appbar at ALL widths — wide it collapses the column, narrow it opens the drawer. Served asset `assets/nav-toggle.js` (same pattern as sidenav-filter.js): toggle sets `data-nav="open|closed|auto"` on the shell root; scrim click resets to `auto`.
- **≤640px**: `.opt` status items hide (word count survives).

### Embeds

- **Mermaid**: wrap output in `.embed`: bordered card, header bar (`.embed-bar`, mono 10px uppercase, kind label + reset button), body centered with pan/zoom. Theme via CSS variables onto Artificer tokens: node fill `color-mix(in oklch, var(--accent-fill) 12%, var(--bg-raised))`, stroke `var(--accent)` (alt nodes: `var(--steel)`), labels mono `var(--fg)`, edges `var(--fg-disabled)`.
- **Pan/zoom** (extend existing SVG pan/zoom): wheel → scale ×1.12 / ×0.89 clamped [0.5, 4]; pointer-capture drag pans; reset button zeroes the transform. Applies to mermaid output and large inline SVGs.
- **Inline SVG**: passes bluemonday's SVG policy; inherits `currentColor` where authored.
- **Raster images**: `figure` + `figcaption` (mono 11px `var(--fg-secondary)`), image `var(--radius-md)` + 1px border, served from doc root.
- **Task lists**: `list-style: none`, flex rows, `accent-color: var(--accent)` disabled checkboxes.

### Upstream candidates (artificer-design-system)

- The 24px tree density as a `.tree--dense` modifier (+ the tree--static twisty/count CSS already flagged in #430).
- The four pending syntax tokens (successBright, attentionAlt, cyan, urgentBright) — chroma.css still carries `var()` fallbacks.
- `.props`, `.callout`, `.embed` are generic enough to upstream if the Obsidian theme project wants them too.

### Reference

The live prototype has every treatment behind Tweaks props so alternates can be re-judged. Prototype-only artifacts to ignore: `.ind` indent spans in code samples (real chroma keeps literal tabs), `image-slot` placeholder.

## Checklist (execution zone)

- [x] Worktree `feat/docs-reader-v2` from fresh `origin/main`; plan persisted + committed; push -u; draft PR
- [x] Density + aria-current: 24px tree rows, Recent loses `aria-current`, `flex: none` rows (template CSS + server.go)
- [x] `render.go`: properties block replaces the disclosure (icons map, `.chip` for status, escaped post-sanitizer); tests updated
- [x] `render.go`: GFM callout transform (`> [!NOTE|TIP|WARNING|DANGER/CAUTION]` → `.callout` tiers); tests
- [x] `render.go` + `server.go`: outline extraction (h2/h3 tree) + word count into template data; tests
- [x] Template: grid layout + 3 breakpoints, appbar nav toggle, outline pane + `outline-inline` fallback, status bar, note typography, props/callout/embed CSS
- [x] `assets/nav-toggle.js` (new, CSP-served); extend `svg-panzoom.js` with `.embed` wrap/reset; mermaid `.embed` chrome + token theme check in `mermaid-init.js`/`diagram.css`
- [x] Task-list + figure/figcaption + inline-code styling
- [x] `go build ./... && go vet ./... && go test ./...` green; live visual pass at all 3 breakpoints
- [x] Reconcile `2026-08-31-docs-reader-design-surface.md` → superseded by this plan
- [ ] Polish (full roster) + conformance; PR body with override block + tuple; redaction; ready flip

## Deviations

- **Review finding declined:** the code-review arm restored `aria-current` on the flat Recent link, reading its removal as a regression — the design contract (verdict 2: one location marker per page; Recent is a shortcut list) removed it deliberately. Reverted the restoration; the reviewer's other fixes (outline entity unescape, `countWords` fence-rule reuse) stand.

- **Mermaid/pan-zoom already shipped**: the spec's "vendor mermaid.min.js" and parts of pan/zoom predate it in-tree (`mermaid.min.js`, `mermaid-init.js` themed from `--dia-*` tokens, `svg-panzoom.js`). Those items execute as "wrap in `.embed` chrome and extend", not "vendor and build".

## Learnings

*(empty at start)*
