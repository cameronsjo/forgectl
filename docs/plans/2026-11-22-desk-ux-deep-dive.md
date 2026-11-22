---
title: forgectl desk UI/UX deep dive
status: planned
next: "Task 1 — worktree, build, fixture; then the dashboard's waiting-item view"
branch: docs/desk-ux-deep-dive
date: 2026-11-22
approved_session_id: 95bfb556-76cf-46c8-87da-02f2a32e4425
---

# forgectl desk UI/UX deep dive

## Goal

Cameron asked for a deep dive on how `forgectl desk` looks and behaves, run as a
conversation: render every view, walk two checklists, write a ranked findings doc
at `docs/research/2026-11-22-desk-ux-deep-dive.md`, agree the ranking with him,
then file the agreed items as `cameronsjo/forgectl` issues. No product code
changes in this plan.

Done means: the doc is on a PR, every finding cites the frame or command that
shows it, Cameron has ruled on the ranking, and each agreed item is an issue.

## First view (redirect here)

**The dashboard with one item waiting, at 66 columns**, the default width
`desk layout` gives the desk pane (`deskLayoutWidth` in
`internal/cli/desk_layout.go`), then at 60/80/120.

Why first:

- It is where a notification click puts the operator, so it is the first screen
  the sibling session's terminal-notifier fix will lead to.
- It is the one screen where a UX failure is a safety failure: the operator has
  to find the short sha256, compare it, and press `y`. If the sha, the what/why,
  or the key hint is cut off or buried at split width, the control is weaker
  than the help text says.
- Every other view (`v`, `l`, `r`, `t`, `h`, `?`) is opened from it, so its
  key hints decide whether the rest is found at all.

Order after that: dashboard states → its overlays and confirms → timeline and
finished-runs panel → run view → `desk ui` (asks) → non-interactive output.

## View inventory

| # | View | States to render |
|---|---|---|
| 1 | Dashboard (`desk`) | empty, 1 waiting, busy, running, done, changed item, lost run |
| 2 | Dashboard overlays | `?` keys, `v` script, `l` log, `s` skip confirm, `a` run-all confirm, `u` undo |
| 3 | Timeline `t`, finished runs `h` | empty, mixed days, long list (scroll) |
| 4 | Run view `r` | flow, events `o`, replay `n`/`p`, its `?`, failed step |
| 5 | `desk ui` (asks) | empty, each bucket, `d r l s` input prompts, `?` |
| 6 | Plain output | `--frame`, `status`, `runs`, `show`, `asks`, `plan`; piped, `NO_COLOR`, `--no-icons` |

Sizes: the full matrix (66/60/80/120 columns × 40 and 12 rows × color,
`NO_COLOR=1`, `--no-icons`) is for the first view only. Every other state is
captured at 66×40 and 120×40 in color, plus one extra size when a finding
needs the second capture § Verification asks for.

Read, not run: `desk serve` (a web page; I read `internal/desk/deskweb/assets/`
only to compare its wording and states with the TUI) and `desk layout` (it
splits the live herdr tab).

## Method

- Worktree `.claude/worktrees/desk-ux-deep-dive` from `origin/main` (the primary
  checkout is 16 commits behind), branch `docs/desk-ux-deep-dive`, pushed with
  `-u`, draft PR at entry.
- Build the binary from the worktree into a scratch dir. Never into the
  brew-linked path.
- Frames come from a throwaway `tmux -L deskux` server via `capture-pane -p`
  (and `-e` for color), not pyte. One capture script in the scratch dir drives
  it and writes one file per view/state/size into a frames directory under
  the session scratch dir that Task 2's teardown does not remove; it stays
  until the PR merges. The frames are not committed. The doc carries each
  finding's frame as a `text` fence, trimmed to the rows that show it, plus
  the command that reproduces it, so a reader of the PR or an issue needs no
  local file.
- Isolation: fixture `HOME` and `XDG_STATE_HOME` in a temp dir, `--dir` on a
  temp desk, and `env -u HERDR_ENV -u HERDR_PANE_ID` on every forgectl call.
  The herdr side rests on `HERDR_PANE_ID` being unset when an item is added
  (the item then carries no pane to signal), not on `notify_herdr`.
- The fixture config sets `notify_macos = false` and `notify_herdr = false`
  and goes where macOS forgectl reads it:
  `<fixture HOME>/Library/Application Support/forgectl/` (`os.UserConfigDir()`,
  `internal/config/config.go:1638`), not `.config/`. The config file name and
  the table the two keys sit in are read from `internal/config` at Task 1.
- Gate before the first `desk add`: `PATH` is prefixed with a stub dir holding
  fake `osascript`, `terminal-notifier` and `herdr` that each append a line to
  a marker file and exit 0. The marker file must be empty after the first add
  and after the whole capture; a non-empty file means the config was not read,
  the capture stops, and that is itself a finding. The stubs make a config
  mistake harmless instead of a real notification.
- Fixture items are `echo`/`sleep`/`exit 1` scripts queued with `desk add`
  under that isolation. States `y` cannot reach are built in the temp desk
  only: a changed item by editing its script after it is queued, a lost run by
  killing the runner of a `sleep` item, mixed days by backdating files in
  `done/` with `touch -t` (if the timeline reads a recorded time instead of
  mtime, by writing that field; checked in the code first).
- Keys: never Enter. `y` is pressed only on fixture scripts in the temp desk,
  to reach the running, done and failed states (my call; say so if you want
  those states built by writing desk files instead). Confirms are rendered and
  then cancelled.
- Checklists: `artificer-design-system` (plus its `ux-design` skill for states,
  microcopy, cognitive load) and the CLI-tool design guidance's TUI
  review checklist. Both are walked against the saved frames; each checklist line gets
  pass, fail with frame, or not applicable.
- Each finding: what the operator sees, the frame or command that shows it,
  why it costs them, a suggested direction, and a rank. Rank = how often the
  operator hits it × what it costs (safety first, then lost work, then friction,
  then polish).
- Known ground (checked 2026-11-22): open `cameronsjo/forgectl` #1202 (`--what`
  truncated, no item order), #1152 (`TERM=dumb` garbles the dashboard), #1095
  (output misreports facts, errors name no next command), #1250 (flowchart
  layout). Closed and to be re-verified in the frames, not re-filed: #1098
  (focus panel with sha/what/why vanished under ~14 rows while `y` still ran),
  #1106, #1107, #1158. A finding that matches an open issue becomes a comment
  on it; a regression of a closed one is filed new, linking the old.
- Before ranking, check open and closed `cameronsjo/forgectl` issues for
  duplicates (the 2026-10-06 audit filed #1098–#1109) and the field-report
  folder for earlier desk write-ups.

## Global Constraints

- Do not edit `internal/notify`, `internal/cli/desk_signal_unix.go`,
  `surface_drain_notify.go`, `resume_hooks.go`, and write no tests for them.
  Fixture `desk add` and the `y`/`s`/`u` keys do execute the signal code in
  the built binary; I read "do not touch" as "do not change", and the stubs
  above keep that execution silent. Notifier observations go in their own doc
  section, not into issues unless Cameron says so.
- No real notification, herdr call, or `desk layout` run.
- forgectl is a public repo: the doc, this plan's copy, and every issue body
  pass a redaction scan before commit or post.
- No issue is filed before Cameron rules on the ranking.

## Loop

| Thing | Opened by | Closed by | Who |
|---|---|---|---|
| Notifier observations for the sibling session | Task 4's doc section | Task 5: one comment on draft PR `cameronsjo/forgectl#1476` once Cameron says go, or his word that the doc is enough | this session |
| Frames directory in session scratch | Task 2 | removed when the PR merges | this session |

## Workarounds

Workarounds: none — tmux capture is the repo's standing way to render these
screens, not a stand-in for a missing platform feature.

## Alternatives declined

- **`--frame` and the golden files only.** Cheap and deterministic, but they
  cover the dashboard alone and never show a key press, an overlay, or a
  confirm. Used as a cross-check, not the source.
- **Start with `desk ui` (asks).** It is the newer surface, but the dashboard
  is where notifications lead and where the sha check lives.
- **Review from source instead of frames.** Finds logic gaps, misses what the
  screen shows at a real width.

## Panel

Panel: plan-reviewer ran — 8 findings, 8 folded in, 0 declined (see Panel review — findings declined)

One seat by choice: the plan changes no product surface, security control, or
executable deliverable, so no other seat's trigger fires.

## Panel review — findings declined

- none declined — all findings folded in. Advisories taken: the capture
  matrix is cut to the first view; `area:forgectl` and `ux-review` labels
  confirmed present; the `serve`/`layout` gap is stated to Cameron up front.

## Orchestrator

**Driver:** fable — operator assigned the seat this deep dive; the judgment is
the deliverable. The seven-condition terminal probe is delegated.

## Tasks

- [ ] **Task 1 — Set up.** Worktree, branch, draft PR; build; fixture HOME,
  desk dir and items; duplicate check against existing issues and field
  reports.
- [ ] **Task 2 — Capture.** Write and run the capture script for inventory
  rows 1–6, first view first. Show Cameron the first-view frames and the first
  findings before going wide. Teardown: `tmux -L deskux kill-server`, remove
  temp dirs.
- [ ] **Task 3 — Terminal-condition probe.** One TUI-reviewer agent
  dispatch (`model: sonnet`, unnamed). It gives verdicts on terminal
  conditions only (`TERM=dumb`, no color, tiny size, resize, slow quit); it is
  not a source of frames, since its probe uses a raw pty. The brief names:
  the commands `<scratch>/forgectl desk --dir <probe desk>` and
  `<scratch>/forgectl desk ui --dir <probe desk>`, start dir = the worktree,
  quit key `q`, never Enter, never `y`/`a`. Its probe swaps in its own
  `HOME`, so isolation there does not come from the fixture config: the probe
  desk is filled beforehand by me and the agent runs no `desk add`, and the
  brief tells it to keep the stub dir first on `PATH` and `HERDR_ENV`,
  `HERDR_PANE_ID` unset. Each failure it reports is reproduced in tmux before
  it counts.
- [ ] **Task 4 — Checklist walk and doc.** Walk both checklists against the
  frames; write the ranked doc, including the notifier section; commit, push.
- [ ] **Task 5 — Ranking conversation.** Present the top of the ranking and
  the calls that are Cameron's (cut lines, anything that changes the `y`
  flow), and ask whether the notifier section goes to `#1476` as a comment.
  Update the doc with his rulings.
- [ ] **Task 6 — File.** One issue per agreed item via
  the issue-filing skill, labels `area:forgectl` + `ux-review`, each linking the doc; record the
  issue numbers in the doc. A docs polish pass, review at head, PR
  ready.

## Verification

- Every finding's frame file exists and shows what the finding says (re-read
  at doc time, not from memory).
- A finding seen at one size is re-captured at a second size before it is
  ranked, so a tmux artifact is not filed as a bug.
- `gh issue view` on each filed issue; the doc's issue list matches.
- `git diff --name-only origin/main` on the branch lists only the doc (and
  this plan's copy).

## Deviations

## Learnings
