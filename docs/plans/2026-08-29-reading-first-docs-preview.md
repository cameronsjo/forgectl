# Reading-first docs preview

## Goal

Make the embedded HTML reader feel like a native editor preview rather than a
regular web application. The document must dominate the pane immediately,
especially at the narrow widths created by a right-hand cmux split.

## Chosen approach

- Replace the persistent/stacking sidebar with an off-canvas document drawer
  that is closed by default at every viewport width. A compact toolbar button
  opens it; Escape, the scrim, or choosing a document closes it.
- Reduce the app bar to editor-preview chrome: navigator button, current
  document title, reading settings, and theme. Remove the oversized brand-first
  presentation from the reading path.
- Keep the article centered on a configurable reading measure with compact,
  responsive gutters. The document begins immediately below the toolbar and
  never waits below a stacked navigator.
- Preserve the existing server, local-image authorization, Mermaid, live
  reload, typography persistence, filtering, and explicit CLI contracts.
- Add a small same-origin behavior asset for drawer state, focus return, Escape,
  and scrim dismissal; keep the no-inline-script CSP invariant.

## Alternatives declined

- A permanently visible VS Code-style activity rail still consumes meaningful
  width in the common half-screen cmux pane without helping the reading task.
- A desktop-only persistent sidebar would reintroduce the dashboard feel and
  make behavior jump as the split crosses one breakpoint.
- Removing navigation entirely would make one-file previews pleasant but turn
  indexed doc sets into a dead end.

## Checklist

- [x] Persist and commit the approved reading-first refinement.
- [x] Replace the stacking sidebar with an accessible off-canvas drawer.
- [x] Reduce the header and make the current document the primary label.
- [x] Tune article spacing and responsive behavior for a half-screen cmux pane.
- [x] Update tests and user-facing documentation.
- [x] Run fresh formatting, JavaScript syntax, lint, vet, and full tests.
- [ ] Complete live cmux visual acceptance. The content-first shell was
  captured successfully; drawer open/filter/Escape/scrim behavior remains to
  be exercised after the session was stopped.
- [ ] Push and document the stopping point in the draft pull request.

## Acceptance

- Opening a document at the current cmux split width shows article content at
  the top of the pane; the full navigator is not stacked above it.
- The navigator opens as a drawer, focuses its filter, closes with Escape and
  scrim click, and returns focus to its toggle.
- The compact toolbar identifies the current document and keeps `Aa` and theme
  controls available without visually competing with the article.
- Existing document rendering, local images, Mermaid, appearance persistence,
  and server security tests remain green.

## Stopping point

The reading-first shell is implemented and the automated verification is
green. Live inspection confirmed that the article now starts directly beneath
the compact toolbar with no stacked navigator. The remaining acceptance step
is to exercise the navigator interactions in cmux and decide whether this
visual direction is sufficiently native-feeling before taking the PR out of
draft.

Next action: rebuild the branch, run `forgectl docs .` inside cmux, then verify
the navigator toggle, filter focus, Escape, scrim dismissal, and focus return.
