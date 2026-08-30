# Embedded docs preview

## Goal

Explore whether `forgectl docs` becomes a genuinely pleasant reading tool when
its existing HTML reader is embedded in the caller's cmux workspace. Optimize
this slice for learning: remove the Kitty/TUI prototype, make the HTML path the
ordinary path, and address the reading gaps exposed by the live cmux proof.

## Evidence and chosen approach

- A loopback docs server was opened with `cmux new-pane --type browser` in the
  caller's workspace. It rendered the existing sidebar, sanitized Markdown,
  syntax highlighting, live reload, and Mermaid without taking focus.
- `forgectl docs [dir|file ...]` will start the existing foreground loopback
  server, open its URL in a right-hand cmux browser pane when
  `CMUX_WORKSPACE_ID` is present, and otherwise fall back to the system browser.
  The terminal remains the server owner, so Ctrl-C stops the preview.
- `docs serve`, `docs open`, and `docs list` keep their explicit contracts.
  `docs serve --open` remains the spelling for a separate system-browser tab.
- Add an unobtrusive reading-settings control for body, heading, and code font
  families plus text size, line height, and measure. Settings are browser-local
  and persist with local storage, making this exploratory without expanding the
  config schema.
- Rewrite relative Markdown image URLs to a same-origin resource endpoint. The
  endpoint will resolve files through the indexed root's existing containment
  boundary, reject excluded/hidden paths and unsupported media types, and keep
  remote images blocked by the current content-security policy.
- Remove the terminal explorer, Kitty graphics, Glamour, and pure-Go Mermaid
  code and dependencies. The completed native-reader plan remains in history as
  the record of the explored approach and this plan records the deliberate
  pivot.

## Alternatives deferred

- A background daemon would return the invoking terminal immediately, but it
  introduces lifecycle and stale-process questions before the reading model is
  proven.
- Bundling proprietary or large font binaries would make typography identical
  across machines, but system/local font stacks are sufficient to evaluate the
  interaction first.
- Opening a generic external browser inside forgectl would couple the command
  to browser automation. This experiment uses cmux's supported CLI when it is
  present and preserves the portable system-browser fallback.

## Checklist

- [x] Prove a loopback reader can open in the caller's cmux workspace without
  stealing focus.
- [x] Persist and commit the approved pivot before implementation.
- [x] Replace the bare docs/TUI entry point with embedded-cmux preview startup.
- [x] Add persisted reading typography and measure controls.
- [x] Serve contained local Markdown images through the loopback reader.
- [x] Remove terminal-reader code and dependencies.
- [x] Update help and README; leave generated changelog prose to Release Please.
- [x] Run fresh build, vet, tests, formatting, lint, and live cmux acceptance.
- [ ] Update and push the existing pull request, then monitor its checks.

## Acceptance

- From a cmux terminal, `forgectl docs [dir|file ...]` creates a readable
  right-hand browser pane in that same workspace and leaves keyboard focus in
  the invoking terminal; Ctrl-C stops its foreground server.
- Markdown, syntax highlighting, tables, Mermaid, inline SVG, and contained
  relative raster/SVG images render without a network dependency.
- The reader offers visibly different body, heading, and code font choices and
  persists the chosen typography, size, line height, and content width.
- Outside cmux, the same command opens the system browser and explains the
  server lifecycle; explicit `serve`, `open`, and `list` behavior remains green.
