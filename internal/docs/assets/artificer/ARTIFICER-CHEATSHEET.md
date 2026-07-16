---
name: Artificer
description: Cameron's personal design system. AuDHD-optimized, Ghostty-rooted, dark-first with a paper-stock light mode and a Jazz Age Deco palette (burnished gold + royal purple). Use for tools, dashboards, agent UIs, terminals, settings panels — anything that must stay calm until something demands action. Do NOT use for marketing sites, kid-facing UI, or anywhere the goal is delight-via-stimulation.
---

# Artificer · v0.19.0

A neurodivergent-first design system for Cameron. Every token and rule here exists to reduce cognitive load for an AuDHD brain that **scans instead of reads** and holds **3–4 items** in working memory.

---

## When to use this

Use Artificer for:
- Internal tools, dashboards, admin panels
- Agent UIs, terminal-adjacent surfaces, dev tools
- Personal sites where "calm focus" is the brand
- Settings, configuration, log/history surfaces

Don't use Artificer for:
- Marketing/landing pages (it's intentionally not playful)
- Anything kid-facing or entertainment-focused
- E-commerce purchase flows (gold-as-interactive collides with sale-color conventions)
- Surfaces where bright energy is the point

---

## File map

| File | Purpose |
|---|---|
| `artificer.css` | All tokens + utility classes. The only stylesheet you need. |
| `artificer-theme.js` | Persistent theme control — dark / light / auto (auto follows the OS live); hydrates the empty canonical `.theme-toggle` button. |
| `assets/cameron-logo.jpg` | Personal mark — only used on Cameron-branded surfaces. |
| `README.html` | System overview, principles, do/don't. |
| `colors.html` · `typography.html` · `spacing.html` | Foundations specs. |
| `components.html` · `patterns.html` · `notifications.html` | Live component specs. |
| `layout.html` | Layout primitives: `.stack`, `.cluster`, `.grid-auto`, `.container`, `.page-shell`. |
| `motion.html` | Duration/easing tokens, the five motion patterns, reduced-motion rules. |
| `overlay.html` | Modal, popover, tooltip, scrim. Six-rung z-index scale. Pairs with `artificer-focus.js`. |
| `forms-extended.html` | Field anatomy, validation, fieldsets, helper/error text. The eight non-negotiable rules. |
| `data-display.html` | Tables, key-value lists, stat cards, progress. Tabular-nums everywhere. |
| `states.html` | Empty, loading, error — by duration. Skeleton sizing rules. |
| `a11y.html` | WCAG 2.2 audit, contrast ratios, focus rings, the 12-point shipping checklist. |
| `artificer-icons.js` | Lucide-rooted icon set, Lucide-canonical names (legacy names alias; unknown names render a dashed placeholder). Auto-hydrates `<i data-icon="…">` placeholders. |
| `artificer-focus.js` | `ArtificerFocus.trap(el, {onEscape})` — focus-trap helper for modals/dialogs. |
| `icons.html` | Icon catalog + usage rules (16px viewBox, 1.5 stroke, inherit color). |
| `voice-and-tone.html` | Microcopy spec — empty states, errors, success, loading. 7-point checklist. |
| `tokens.json` | Machine-readable token export (for Tailwind, Figma, non-CSS consumers). |
| `print.css` | Print stylesheet. Forces ivory/navy paper mode, strips chrome. Load `media="print"`. |
| `assets/favicon.svg` · `assets/og-image.svg` | Favicon (32px) and Open Graph card (1200×630). SVG, themeable. |

---

## The boilerplate (copy this)

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <title>...</title>
  <meta name="viewport" content="width=device-width,initial-scale=1" />
  <!-- Theme bootstrap — FIRST, before any CSS, so a dark page never flashes
       light. Key 'artificer.theme' (dot); dark-first. See QUICKSTART.md. -->
  <script>(function(){try{var s=localStorage.getItem('artificer.theme');var p=s==='light'||s==='dark';var l=window.matchMedia&&window.matchMedia('(prefers-color-scheme: light)').matches;document.documentElement.setAttribute('data-theme',p?s:(l?'light':'dark'));}catch(e){document.documentElement.setAttribute('data-theme','dark');}})();</script>
  <link rel="icon" type="image/svg+xml" href="assets/favicon.svg" />
  <meta property="og:image" content="assets/og-image.svg" />
  <link rel="stylesheet" href="artificer.css" />
  <link rel="stylesheet" href="print.css" media="print" />
  <script src="artificer-theme.js" defer></script>
  <script src="artificer-icons.js" defer></script>
</head>
<body>
  <button class="theme-toggle" data-theme-toggle aria-label="Toggle theme"></button>
  <main style="max-width:820px;margin:0 auto;padding:48px 24px">
    <!-- your content -->
  </main>
</body>
</html>
```

If you have **no file system** (chat-only artifact), inline the contents of `artificer.css` into a `<style>` block instead of linking it.

---

## Decision recipes

| User asks for… | Reach for… |
|---|---|
| Dashboard with file/agent list | `patterns.html` sidebar + tabs + content pane |
| Settings page | `forms-extended.html` `.field` blocks + fieldsets, grouped 3–5 per section |
| Form (any kind) | `forms-extended.html` — label → input → hint/error, never placeholder-as-label |
| Modal / dialog | `overlay.html` — `.scrim` + `.modal`, wire `ArtificerFocus.trap()` for focus-trap |
| Tooltip / popover | `overlay.html` — `.tooltip` (label) or `.popover` (body content) |
| Page layout (sidebar/main) | `layout.html` — `.page-shell`, `.container` + `.container--{sm\|md\|lg}` (the base class is required — it carries width, centering, and inline padding; the size modifier only sets max-width) |
| Blog / editorial / document top nav | `.masthead` (`artificer-editorial.css`) — non-sticky, no border; document counterpart to `.appbar`. Brand via `.wordmark`, toggle via `.theme-toggle--inline` — `navigation.html` |
| Stacking children | `layout.html` — `.stack` (vertical), `.cluster` (horizontal wrap) |
| Card grid | `layout.html` — `.grid-auto` with `--min: 240px`, never hand-rolled flexbox |
| "Search anything" / command palette / ⌘K | `.palette` (= `.palette__search` combobox input + `.listbox` body) on a `.scrim` — `ArtificerOptions.combobox(input, list)` + `ArtificerFocus.trap()`, Esc closes; 5–7 results visible — `components-extended.html` |
| Combobox / dropdown / menu | One option-popover: `.menu` (actions) / `.listbox` (selection) + `__option`/`__label`/`__sep`/`__hint`/`--danger`; `.is-active` is the cursor. Behavior: `data-options` / `ArtificerOptions.enhance()` — `components-extended.html` |
| Toast / alert | `notifications.html` — pick tier by *action required*, not severity; silent by default — audible escalation only as a named, opt-in carve-out |
| Transient toast placement | Mount the `.notif` in a `.toast-region` (fixed corner stack on `--z-toast`, `+N more` via `.toast-region__more`); roles set at INSERT: urgent → `alert`, attention/info → `status`, background → none — `notifications.html` |
| Tree / file explorer / nested nav | `.tree` > `.tree__group` > `.tree__row` (+ `.tree__twisty`, `.tree__leaf`); `role=tree/treeitem/group`; keyboard ships via `data-tree` / `ArtificerTree.enhance()` — `components-extended.html` |
| Pagination | `.pagination` + `.pagination__gap`; `[aria-current=page]` marks the page; prev/next disable at ends; counted ranges only — `components-extended.html` |
| Persistent page banner | `.banner` + `--info/attention/urgent/success` + `.banner__body`/`.banner__actions` — a standing layout band, NOT the transient `.notif` — `components-extended.html` |
| Footer / colophon / fine print / attribution | `.colophon` + `.colophon__label` (column headers) + `.colophon__fine` (legal tier); columns via `.grid-auto`; prose auto-sans even inside `.surface-tool` — `.surface-document` flips any other prose island back — `layout.html` |
| Selected card / selected row | `.card--active` (`background: var(--bg)` + accent left border) for a card, `.is-active` (`background: var(--bg-raised)`) for a list row — never `--accent-fill` as a large surface bg, its only rated text color is `--on-accent` — `components.html` |
| Status indicator | `.dot--{accent\|attention\|urgent\|success}`, no count |
| Count indicator | `.badge--{accent\|attention\|urgent\|success}` with number |
| Dense table status glyphs (anti-emoji) | `.glyph--{success\|muted\|attention\|na}` tints ✓✗~– with a theme token; graphical (SC 1.4.11) so each pairs with `aria-label` — sparse/labeled status stays `.badge`+`.dot` — `data-display.html` |
| Icon inside button/link | `<i data-icon="search"></i>` — see `icons.html` for full set |
| Table of data | `data-display.html` — `.table`, right-align numerics, `font-variant-numeric: tabular-nums` |
| Headline numbers (KPIs) | `.stat` — `.stat__label` + `.stat__value` (mono, tabular) + `.stat__row` + `.stat__delta`(`.down`); the cell of a `.kpi-strip`, max 4 per row — `data-display.html` |
| Empty state / error / loading copy | `voice-and-tone.html` — never improvise microcopy |
| Loading UI | `states.html` — pick by *duration*: nothing → disabled label → skeleton → progress → background |
| Long wait, nothing to count | `.progress--indeterminate` + `role="progressbar"` + `aria-label` with concrete copy, NO `aria-valuenow/min/max` — `states.html` |
| Refreshing a value in place | `.live-value[data-refreshing]` recedes the stale value + `.live-value__dot` pulses — NOT `.skeleton` (would blank it) — `composition.html` |
| Animation / transition | `motion.html` — `--dur-fast` + `--ease`. Don't invent durations. |
| z-index | `motion.html` / `overlay.html` — six rungs only: `--z-{base\|raised\|overlay\|popover\|modal\|toast}` |
| Pre-ship a11y check | `a11y.html` — 12-point checklist before merging |
| Token in non-CSS context | Read `tokens.json` |
| PDF / print output | Add `<link rel="stylesheet" href="print.css" media="print">` |
| Hero/landing page | **Don't** — wrong system. Suggest a brand-marketing system instead. |

### SPA lifecycle (icons / whimsy / theme + modal re-trap)

Icons and theme arm their own `MutationObserver` on DOM ready — SPA-mounted
`[data-icon]` / `[data-theme-toggle]` nodes hydrate and bind automatically.
Whimsy hydrates once on `DOMContentLoaded`; tabs, options, and tree enhance
on explicit calls. For nodes that mount after first paint in those modules,
use the lifecycle API:

- **`ArtificerIcons.observe(root?)` / `Whimsy.observe(root?)` /
  `ArtificerTheme.observe(root?)`** — framework-agnostic. Hydrates `root` now,
  then a `MutationObserver` re-hydrates inserted nodes (idempotent guards make
  it cheap). Returns a disconnect fn. Use for vanilla / Vue / Svelte / streamed
  content.
- **React:** `useIcons(ref?)` / `useWhimsy(ref?)` hydrate on mount; the `Icon`
  component already self-hydrates. For content that streams in after mount,
  prefer `observe()`.
- **Theme persistence key is `'artificer.theme'` (a DOT).** Vanilla and the
  React `useTheme` must agree on it or theme won't survive the
  SPA↔first-paint boundary.
- **Modal re-trap:** `ArtificerFocus.trap()` recomputes focusables on **every
  Tab**, so controls added while the modal is open are reachable automatically
  — you do **not** re-trap for that. The only thing fixed at open is **initial
  focus**. If you swap modal content (e.g. a wizard step) and want focus moved
  to the new first control, `release()` then `trap()` again (or `.focus()` the
  new target yourself).

---

## The ten non-negotiables

1. **Dark is default.** Light mode is a paper alternative, not the primary.
2. **Max 2 semantic colors on screen at once.** Brand purple + gold = one "Cameron" signal, doesn't count.
3. **Bold 3–5 anchor words per paragraph.** Use `class="anchor"` or `<b>`. Primary scanning mechanism. When prose arrives as data, mark anchors as `**…**` in the source and promote each marked span to `<b class="anchor">` at render — single-level only, never nested.
4. **One primary CTA per screen.** Always. Everything else is `.btn--secondary` or `.btn--ghost`.
5. **5–7 items max** in any list/palette/menu before progressive disclosure.
6. **No pure black.** `#1D1F21` is the darkest value in the system.
7. **Honor `prefers-reduced-motion`** — durations collapse to 0ms (already wired in CSS).
8. **WCAG AAA** for body text (7:1). Text-safe accent variants already hit this.
9. **Never mix rounded and sharp corners** in one view.
10. **Voice is literal, direct, lightly deadpan.** No metaphor, sarcasm, or figurative copy. Autism demands clarity.

---

## Token cheatsheet

```
SURFACES --bg / --bg-raised / --bg-overlay / --bg-inactive
TEXT --fg / --fg-secondary / --fg-muted / --fg-disabled / --border

INTERACTIVE --accent gold text (AAA) — links, focus, secondary buttons
                --accent-bright hover state
                --accent-fill gold background — SMALL controls only (buttons, badges);
                    never a selected-card/surface bg, pairs only with --on-accent

ATTENTION --attention rose text (AAA) — "look when you can"
                --attention-fill rose background

URGENT --urgent vermillion text — errors, blocking
                --urgent-fill vermillion background

SUCCESS --success olive — completed
META --steel / --steel-fill — chrome, secondary UI

BRAND --brand-purple / --brand-purple-fill — wordmarks, masthead, NOT semantic

TYPE --font-mono JetBrains Mono
                                  · BODY FACE for tool surfaces (dashboards,
                                    terminals, data tables, settings panels)
                                  · ALWAYS for code, identifiers, file paths,
                                    numerals — including inside documents
                --font-sans iA Writer Quattro
                                  · BODY FACE for document surfaces (writeups,
                                    READMEs, reports, design docs)
                                  · On tool surfaces: labels, hints, microcopy

                Decision rule. >3 paragraphs of running prose → document → sans body.
                Mostly chrome around data → tool → mono body. Same project can mix.
                Anti-pattern: setting prose in mono and then overriding `.meta`,
                headings, tables back to sans. If you're escaping the body face,
                the body face is wrong — flip it.
                Type utilities set size + line-height only — compose vertical
                rhythm with `.stack` (gap-owned), not margins.

SPACING --s-xs(4) sm(8) md(16) lg(24) xl(32) 2xl(48)
RADII --radius-sm(4 · buttons) md(8 · cards) lg(12 · overlays only) pill(999 · toggle/chip)
MOTION --dur-instant(80) fast(160) max(300) · ease cubic-bezier(.2,.7,.3,1)
```

---

## Utility classes

```
TYPE .t-headline-lg .t-headline-md .t-body-lg .t-body-md
           .t-label-md .t-label-sm .t-code
           .anchor (bold anchor word) · .meta (secondary color)

BUTTONS .btn + .btn--primary | --secondary | --ghost | --destructive
           [disabled] for inactive
           .btn--icon (square 44px, needs aria-label) + .btn--icon-prominent
           when it stands ALONE as a primary control (hamburger, modal close)

CARDS .card + .card--active | --attention | --urgent

FORMS .field > label + .input | .select | .textarea + .hint | .error

BADGES .badge + .badge--accent | --attention | --urgent | --success | --ghost
DOTS .dot + .dot--accent | --attention | --urgent | --success

PANES .pane--active (gold left border) · .pane--inactive (55% opacity, desaturated)

KBD <kbd>⌘ ↵</kbd>
THEME <button class="theme-toggle" data-theme-toggle aria-label="Toggle theme"></button>
        (empty — the module injects the glyph; cycles dark → light → auto)
```

Coming from Bootstrap/Tailwind `warning`? That tier is **`--attention`** here —
across badge, dot, card, notif, and banner. One name per tier; there is no
`warning` alias.

---

## Anti-patterns vs patterns

```html
<!-- ANTI: three semantic colors competing -->
<div>
  <button class="btn btn--primary">Save</button>
  <button class="btn btn--destructive">Delete</button>
  <span class="badge badge--attention">2 reviews</span>
</div>

<!-- PATTERN: one primary, others demoted -->
<div>
  <button class="btn btn--primary">Save</button>
  <button class="btn btn--ghost">Delete</button>
  <span class="badge badge--ghost">2 reviews</span>
</div>
```

```html
<!-- ANTI: prose with no anchors. Nothing to scan. -->
<p>The agent finished writing the section and is now waiting for the editor to review the changes before continuing.</p>

<!-- PATTERN: 3 anchor words. Bolded path makes sense alone. -->
<p>The <b>writer agent</b> finished the section. <b>Waiting on editor</b> to review before <b>continuing</b>.</p>
```

```html
<!-- ANTI: placeholder-only label, vague error -->
<input class="input" placeholder="API key" aria-invalid="true" />
<span class="error">Invalid input</span>

<!-- PATTERN: persistent label, specific remediation -->
<div class="field">
  <label for="k">API key</label>
  <input id="k" class="input" aria-invalid="true" />
  <span class="error">Missing <code>sk-</code> prefix. Paste the full key from console.</span>
</div>
```

```html
<!-- ANTI: 12 items in a flat list -->
<ul><li>...</li>... 12 items ...</ul>

<!-- PATTERN: 5 + "show more", or grouped with labeled dividers -->
<ul><li>...</li>... 5 items ...</ul>
<button class="btn btn--ghost">Show 7 more</button>
```

---

## Composition cheatsheet

When you're past atoms and need to lay out a real product surface, the shell
is always **`.dash`** (frame + `.dash__topbar`) + a `density-*` + a body built
from shipped primitives. The five recipes (these are RECIPES, not classes —
there is no `.dash-*` class):

| Surface | Recipe (`.dash` + body) | Density default |
|---|---|---|
| Metrics overview | `.kpi-strip` over a chart | `.density-cozy` |
| Ops console / log view | `.split-pane` + log stream + `.table` | `.density-compact` |
| Observability | chart grid (`.grid-2` / `.grid-auto`) | `.density-cozy` |
| Records / queues | `.table` is the body | `.density-compact` |
| Master / detail | `.split-pane` | `.density-cozy` |

### Charts — non-negotiable

- **5 series max.** Beyond that → split chart, group, or sequential ramp (`--ramp-1..5`).
- **No pies above n=3.** Bar chart. Always.
- **Sparklines have no axes.** Use `.sparkline` / `.sparkbars` inside tables.
- **2 gridlines max.** Baseline + one mid.
- **Bars start at zero.** Lines may fit Y-range.
- **Tabular numerals** wherever a number renders. Already wired via `--font-mono`.
- **No entry animation by default.** Honor `prefers-reduced-motion`.

### Diagrams — non-negotiable

- **`.dia-node` + `.dia-edge` + `.dia-edge-label`** — apply directly to inline SVG `<rect>`/`<path>`/`<text>`.
- **1 accent node per diagram** (`.dia-node--accent`) — the diagram's subject.
- **Ghost = planned** (`.dia-node--ghost`, dashed border).
- **Edge stroke encodes resolution:** default = step, `--strong` = closes the flow, `--dashed` = async/return.
- **9 nodes ceiling.** Group beyond that.
- **Mermaid:** initialize with `theme:'base'` + `themeVariables` reading CSS vars (snippet in `live-spec/diagrams.html`).
- **React Flow:** wrap in `.rf-artificer`.
- **Excalidraw palette:** strokes `#ffffff #c5c8c6 #e0b558 #e8836f` · fills `#292c33 #313540 #3c4150`. "Architect" roughness for system maps; "artist" for sketches.
- **`<defs>` ids are document-scoped.** Namespace per instance (`arrow-<id>`) when repeating a diagram component; never hard-code one inside it.
- **Interactive nodes:** `tabindex="0"` + `role="button"` + `aria-label` + Enter/Space. Focus ring free from the global catch-all.

---

## Voice rules

- **Literal.** Name the thing. Don't gesture.
- **Direct.** Front-load the action. "Save changes" > "Click here to save your changes."
- **Lightly deadpan.** Wit lives in restraint, not in jokes. A palette showing exactly 5 things is funnier than any pun.
- **No metaphor in errors.** "Build failed: missing close brace at line 214" beats "Looks like things went sideways."
- **No sarcasm anywhere.** Autism demands clarity; sarcasm is confusing.

---

## Versioning & attribution

- **v0.1** · 2026 · personal use, no license required for Cameron's projects.
- Palette inspired by 1920s–50s Jazz Age Deco screen-print posters.
- Surface scale rooted in [Ghostty](https://ghostty.org) terminal defaults.
- AAA contrast values pre-tuned; don't substitute hexes without re-running contrast checks.
