# Artificer adaptations

How this project bends the Artificer design system, and why. Each entry mirrors a
feedback issue filed upstream.

## A1 — pinned, verified vendor script

- **Dated:** 2026-08-31 · app @ pre-1.0 · Artificer @ 0.25.0
- **What diverges:** `scripts/vendor-artificer.sh` wraps `npx @cameronsjo/artificer vendor` with a pinned version, provenance verification, and an advisory `--check` mode, rather than calling the raw command directly.
- **Why necessary:** the bare `npx` invocation has no repeatability or provenance guarantee on its own.
- **Upstream issue:** #448 (filed via this skill)
- **Retire when:** cameronsjo/artificer-design-system#447 ships a native `vendor --check` flag covering the same ground.

## A2 — version-ledger gap workaround (0.23.0/0.25.0)

- **Dated:** 2026-08-31 · app @ pre-1.0 · Artificer @ 0.25.0
- **What diverges:** cross-checked primitives' own `minted` fields instead of trusting `versions{}` alone, because `versions{}` has no entry for 0.23.0 or 0.25.0.
- **Why necessary:** a version-boundary walk keyed only on `versions{}` silently skips releases with no ledger entry, even when they mint primitives or are the current release.
- **Upstream issue:** #448 (filed via this skill)
- **Retire when:** `versions{}` gets entries for every real release.

## A3 — manual adoption sweep for signal-less mints

- **Dated:** 2026-08-31 · app @ pre-1.0 · Artificer @ 0.25.0
- **What diverges:** manually inspected consumer usage of colophon, masthead, bar-meter, diagram-flow, spine-phase, and workbench instead of grepping for `adoption.signals[]`.
- **Why necessary:** all six primitives minted in this upgrade window carry zero adoption signals, so the mandated sweep has nothing to search for.
- **Upstream issue:** #448 (filed via this skill)
- **Retire when:** new mints ship with populated `adoption.signals[]`.

## A4 — `field__label` class cleanup deferred

- **Dated:** 2026-08-31 · app @ pre-1.0 · Artificer @ 0.25.0
- **What diverges:** forgectl's docs shell template uses class `field__label`, which was never an `artificer.css` class (the system styles `.field > label` via a child selector).
- **Why necessary:** N/A — this is forgectl's own bug, not an Artificer gap; flagged upstream only because it shows an upgrade-verification class list built from a template can overclaim without a canonical class inventory to check against.
- **Upstream issue:** #448 (filed via this skill)
- **Retire when:** forgectl fixes its own template to drop the unused class.

## A5 — terminal palette consumed from `_palette.json`, not `tokens.json`

- **What diverges:** forgectl's TUI and CLI colours come from `themes/_palette.json` in the design-system repo, not from the `tokens.json` shipped on npm alongside the web assets. Both are vendored; they are different palettes and they disagree — `brandPurple` is `#9070d0` in one and `#5a3a9a` in the other.
- **Why necessary:** `tokens.json` is the WEB palette. `_palette.json` is the terminal one, and it is what already generates the design system's ghostty, tmux, gitmux and cmux themes. forgectl runs inside a tmux pane under Ghostty; drawing from the web palette would make it the one thing on screen not matching its own status bar. The design system treats the split as intentional (`$notes.brandPurpleSurfaceSplit`): the same hue has two jobs, and only one is foreground text.
- **How it is consumed:** `scripts/vendor-artificer.sh` fetches both artifacts and fails when their `$version` values diverge. The palette is not on npm — `themes/` is repo-only — and that repo is private with no tags, so the fetch is `gh api` at a pinned commit SHA rather than an unauthenticated URL at a tag. `internal/theme/artificer_gen.go` is generated from it; a test fails if the two drift.
- **Role mapping:** seventeen roles, documented with their palette keys in [`docs/commands/theme.md`](commands/theme.md). Three are worth noting because the obvious name is wrong: `danger` is `urgentText` and not bare `urgent` (which fails AA as text at ~2.27:1 on the dark background), `onaccent` is `ink` in **both** modes, and there is no `urgentFill` token — bare `urgent` is the fill hue.
- **Upstream issue:** none filed. A forgectl target in `themes/build.mjs` would be the canonical shape, but it needs a delivery channel for the terminal palette that does not exist yet; the pinned fetch gets the same bytes today.
- **Retire when:** the design system publishes `_palette.json` on npm, or gains a Go target that generates this file directly.
