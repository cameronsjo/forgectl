---
status: in-flight
---

# Goal-first Projects interface

Status: Cameron approved the proposed redesign; implementation in progress
Next action: replace the wizard and separate result/question screens, then verify the real keyboard journey.

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

- [ ] Review the plan and approval/preset risks independently; resolve supported findings.
- [ ] Add bounded preset suggestions and negative tests without executing repository code.
- [ ] Replace setup wizard with one form, normal controls and invalidatable compact approval.
- [ ] Consolidate run questions, evidence and cancellation into one workspace.
- [ ] Update documentation and real-PTY probes for the new journey.
- [ ] Verify unit/negative tests, actual terminal behavior and clean-build provenance; commit and push.

## Ownership and review

Root Codex owns this plan, preset helpers/tests, documentation and Python PTY probes. The UI subagent owns `internal/tui/project_goals_unix.go`, its tests and an optional form-specific companion file. Reviewer assignments are read-only. The initial UI inspection confirmed seven screen states plus an eight-step wizard, and identified preparation invalidation and polling-safe inline input as critical acceptance gates. Independent plan/security/UX review is pending; it will examine the explicit approval boundary, default-command suggestions and the actual view/focus design.

## Completion evidence

Pending. Baseline is clean committed `8b668d7b`; its native documentation goal already completed review/checks/cleanup. Existing unrelated menu/terminal-safety test failures remain outside this UI task. No full-suite or hosted-CI success is assumed.

## Future work

- Saved personal presets and cross-session preference persistence are separate work; this pass supplies useful built-in presets without introducing hidden remembered permissions.
- Additional project-specific test detection can extend the fixed suggestions after proving its runner semantics; unknown configurations remain explicit rather than guessed.
