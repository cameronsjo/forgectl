# Native docs explorer

## Goal

Make `forgectl docs` a terminal-native, Artificer-styled document explorer while
retaining the existing HTTP reader for remote, phone, and exact Mermaid.js
fallback use. Local images, SVG, and Mermaid diagrams render in compatible
terminals through the Kitty graphics protocol and degrade to readable text when
graphics are unavailable.

## Chosen approach

- `forgectl docs [dir|file ...]` launches the TUI on an interactive terminal;
  `forgectl docs browse` is the explicit equivalent. Existing `serve`, `open`,
  and `list` behavior remains compatible.
- Reuse the docs index, root resolution, watcher, and browser opener. Add an
  adaptive Bubble Tea explorer with a filterable tree and scrollable Markdown
  pane, rendered with Glamour and an Artificer stylesheet.
- Use Charm's already-pinned Kitty encoder with Unicode virtual placements.
  Add capability detection, stable IDs, resize retransmission, and cleanup.
- Render local PNG, JPEG, static GIF, and SVG references only. Resolve every
  path relative to its Markdown document and keep it inside the indexed root.
- Render Mermaid with a pinned pure-Go renderer and rasterizer. Unsupported
  syntax remains visible as source and points to the retained web reader.
- External links require confirmation before opening the system browser;
  relative Markdown links and anchors navigate inside the TUI.

## Alternatives declined

- Full HTTP-reader replacement: loses remote and phone access and removes the
  exact Mermaid.js fallback.
- Headless Chrome: matches Mermaid.js more closely but makes a browser a hidden
  runtime dependency, contrary to the native-reader goal.
- Text-only first release: does not deliver the requested graphics experience.

## Checklist

- [x] Create an isolated worktree and feature branch from fresh `origin/main`.
- [x] Persist and commit the approved plan before implementation.
- [x] Add terminal Markdown, resource, diagram, and Kitty graphics primitives.
- [x] Add the adaptive docs TUI and wire the native-first CLI entry points.
- [x] Cover fallback, containment, navigation, resize, reload, and cleanup.
- [x] Update help, README, and changelog.
- [ ] Run fresh build, vet, tests, formatting, lint, and Ghostty acceptance.

## Acceptance

- Markdown, local raster images, SVG, and supported Mermaid diagrams render in
  cmux/Ghostty; scrolling and resizing keep images attached to document rows.
- Unsupported terminals, remote images, invalid diagrams, and decode failures
  show deliberate readable fallbacks without raw graphics control sequences.
- Live reload, internal links, history, filtering, and external-link
  confirmation work without opening a separate browser for ordinary reading.
- Existing `docs serve`, `docs open`, and `docs list` contracts and tests remain
  green.
