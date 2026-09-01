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
