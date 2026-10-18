---
status: in-flight
---

# Goal-first Projects interface

Status: approved interface implemented; clean-build delivery verification in progress
Next action: build from the committed source, repeat the terminal acceptance checks, and push the feature branch.

## Outcome and authority

Cameron reported too many screens, no presets, and unfamiliar Ctrl+J/Ctrl+S navigation, then approved: one setup screen, normal multiline/field controls, useful presets, advanced settings tucked away, compact explicit approval, and one run view with progress, questions and evidence. This is an interface redesign, not permission to weaken the proven goal runtime or start arbitrary repository work.

Continue on `codex/cross-harness-loop` in its existing linked worktree. Commit and push this feature branch. Do not install globally, merge, deploy, open/change a PR, change shared services or modify the primary checkout. The prior native goal and its retained receipts remain intact. No new orchestration system or dependencies are needed.

## Decisions

- Setup becomes a single scrollable form with repository, preset and a multiline goal. Presets are Bug fix, Feature, Docs and Custom. A title is derived from the goal unless explicitly edited. Advanced expands in the same form for title, allowed paths, acceptance criteria, exact verification argv, model and duration.
- Enter adds a newline in multiline input. Tab and Shift+Tab move focus. Enter/Space activate visibly focused buttons; arrow keys operate selectors and editors. No required Ctrl+J/Ctrl+S choreography. Optional existing shortcuts must not intercept ordinary typed text.
- Presets fill suggested scope, acceptance and checks without overwriting the user's goal. Bug fix/Feature suggest known test commands from bounded, read-only manifest inspection; Docs stays scoped to docs/README paths and a whitespace check. Unknown projects clearly show that only a whitespace check was detected. No detection command or repository script runs during setup, and no dependencies are installed. Custom preserves explicit edits.
- Review is a compact approval section, not another field-by-field interview. It shows actual repository/base revision, dirty-source warning, scope, exact commands, model, duration and unchanged execution restrictions. Preparation is still read-only. Start remains a separate deliberate action against the frozen prepared specification. Any edit or preset/repository change invalidates preparation.
- A run stays in one workspace with a stable title/status, progress and role information, inline pending question/answer with a Send button, and labeled evidence tabs/sections for diff, all check receipts and details. Tabs do not create separate screens requiring an Escape chain. Questions never grant new permissions. Cancel confirmation is inline and defaults to keeping the run; detach remains distinct from cancel.
- Preserve every backend contract: exact revision confirmation, private isolated worktree, bounded native sessions, direct review/digest verification, exact approved commands, native tool isolation, retained evidence, unknown liveness and guardian-confirmed cleanup. CLI JSON behavior stays unchanged.

## Acceptance

1. A normal goal can be prepared using repository, preset and goal on one form, with no mandatory advanced editing or Ctrl shortcuts. Enter/newline, Tab/back-tab and visible buttons work over a real PTY.
2. Each preset produces independent editable slices, preserves goal text, makes detected commands visible, and executes nothing before approval. Invalid/unreadable/symlinked manifests cannot silently inject commands or broaden permissions.
3. Start cannot occur from ordinary text Enter, missing/invalid preparation, stale preparation after edits, or a duplicate key while an action is pending. Compact presentation does not hide the approved scope or command argv.
4. Progress, pending questions, cancellation and every check result are reachable in one run workspace. Sending a multiline answer uses normal editing plus an explicit Send button, not Enter-to-send. Polling must not replace edits or move focus.
5. Focused controls and the caret stay visible at 100x24 and 50x16. Long objectives, questions, paths, errors and evidence are wrapped/sanitized, with no terminal escape injection or clipped action labels.
6. Unit/negative tests cover these behaviors. Real-PTY acceptance uses the compiled clean build for setup, prepared approval, retained real-result inspection and inline question/cancel fixtures. A bounded real start/cancel run may verify the unchanged native handoff without editing repository files; no unrelated model task is needed.
7. A clean committed source build records its actual VCS revision, and the feature branch is pushed with exact remote equality. Document the new journey and preserve the earlier runtime evidence rather than pretending UI changes reran the whole native implementation.

## Work

- [x] Review the plan and approval/preset risks independently; resolve supported findings.
- [x] Add bounded preset suggestions and negative tests without executing repository code.
- [x] Replace setup wizard with one form, normal controls and invalidatable compact approval.
- [x] Consolidate run questions, evidence and cancellation into one workspace.
- [x] Update documentation and real-PTY probes for the new journey.
- [ ] Verify unit/negative tests, actual terminal behavior and clean-build provenance; commit and push.

## Ownership and review

Root Codex owns this plan, preset helpers/tests, documentation and Python PTY probes. The UI subagent implemented `internal/tui/project_goals_unix.go` and its tests; root Codex took ownership again for final integration. Reviewer assignments were read-only. The initial UI inspection confirmed seven screen states plus an eight-step wizard. Independent review found stale preparation/action/artifact responses, polling-sensitive answer input, missing busy guards and narrow-screen clipping. Request generations and goal/question identity checks now reject stale responses without clearing a newer operation; polling has separate loading state. Root Codex independently found and fixed a shallow-copy renderer that mutated the retained question, tested its failing case before the fix, and verified that a refreshed question cannot leave answer submission permanently busy.

## Completion evidence

Baseline is clean committed `8b668d7b`; its native documentation goal already completed review/checks/cleanup. Preset helper tests pass both inside the TUI package and as the isolated two-file unit while the UI refactor is in progress. Tests cover Go/Rust/JavaScript suggestions, absent/invalid/oversized manifests, symlink refusal, independent editable slices and docs-only paths. An executable fake-runner control first writes a marker to prove detection would be observable; preset discovery then leaves that marker absent with every runner stubbed on PATH. No model or repository test command is run by discovery. Existing unrelated menu/terminal-safety test failures remain outside this UI task. No full-suite or hosted-CI success is assumed.

Root Codex ran `go test -p=2 ./internal/tui -count=1`, focused goal tests with `-race`, the repository-pinned golangci-lint 2.13.1 against the changed TUI package, and `python3 scripts/test-probe-project-goals.py`. All passed with the current UI source; the existing TTY tests require host PTY permissions, and their sandbox-only failures were not attributed to the product. Literal argument spaces and long evidence lines retain their content while wrapping; multiline editors display real newlines, not escaped newline text.

Actual PTY checks at 100x24 and 50x16 exercised normal multiline goal entry, preset selection, advanced editing, frozen approval with Edit focused by default, and invalidation without creating a run. Separate PTYs verified long-path Home/End, long-goal beginning/middle/tail carets and the next visible button. The retained real native goal's patch, both check outputs and JSON are readable through one workspace with pinned identity/status. A clearly synthetic private registry fixture verified actual answer/control callbacks, ordinary multiline typing across polling, explicit Send, safe Keep-running default and explicit cancellation; it did not start a model or claim native cleanup. The earlier real native goal and its source/receipts remain unchanged.

The new-form verifier rejects the old wizard binary. Probe regressions also distinguish focus from an unfocused mention, hard-wrapped argv from changed bytes, and genuine scrolling from a static screen even when the first and last viewport match. Clean committed-build provenance and remote equality remain the final delivery gate.

The first clean committed build (`7b4c406e`) passed all four terminal journeys and exact remote-head verification. Final integration also exposed ordinary Down being overridden by caret-following; its regression failed before the fix. Ordinary evidence arrows now scroll, and editing restores caret tracking after Page Up/Down. The full TUI package, pinned lint and probe contract tests passed again after that bounded fix.

## Future work

- Saved personal presets and cross-session preference persistence are separate work; this pass supplies useful built-in presets without introducing hidden remembered permissions.
- Additional project-specific test detection can extend the fixed suggestions after proving its runner semantics; unknown configurations remain explicit rather than guessed.
