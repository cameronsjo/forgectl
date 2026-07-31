# `forgectl resume` — cross-project Claude session resume + task rescue (forgectl#179)

## Context

**The problem.** Terminal restarts (updates, crashes) are frequent, and getting back into
a Claude Code session afterward is a three-step scavenger hunt: navigate to the right
folder, run `claude --resume`, then recognize the session in a picker that shows no repo
and no branch. Two things are lost along the way:

1. **Which session was I in, and where?** The session id and its cwd live in the
   operator's head, or in a message they have to ask for before restarting.
2. **The task list.** Tasks created during the session do not come back.

**What the investigation found** — the two halves have *different* root causes, and that
changes the shape of the fix:

| Source | Durable? | What it carries |
|---|---|---|
| `~/.claude/history.jsonl` | **yes, indefinitely** | every prompt: `sessionId`, `project` (= cwd), `timestamp`, `display` |
| `~/.claude/projects/<slug>/<id>.jsonl` | **yes** | `cwd`, `gitBranch`, and an `ai-title` record |
| `~/.claude/sessions/<pid>.json` | **live only** | `sessionId`, `cwd`, `name` (the `/rename` name), `status`, `version`, `pid`, `kind` |
| `~/.claude/tasks/<full-session-uuid>/<n>.json` | **deleted** | per-session task bodies: `id, subject, description, activeForm, status, blocks, blockedBy` |
| `~/.claude/tasks/session-<8hex>/<n>.json` | **deleted** | the same bodies, for an agent team (see the dialect correction under Step 3) |
| `<repo>/.claude/sessions/<name>.<id8>.json` | yes, per repo | lane name + declared branch |

So **session identity is already durable** — it needs a *reader*, not a persister. Every
registry file on this machine maps to a live pid, so `~/.claude/sessions/` is pruned on
exit; but `history.jsonl` and the transcripts survive indefinitely and carry both the id
and the cwd. Only the `/rename` name is genuinely ephemeral.

**Tasks are the real loss, and it is not an id problem.** 515 of 559 dirs under
`~/.claude/tasks/` have zero task JSON left while `.highwatermark` proves tasks existed
(`session-2ae3d047`: highwatermark 10, zero files). The id-fork theory is dead — 0 of 474
July transcripts fork a new session id (93 of 510 did in June), so a same-id resume
*would* find its task dir. Claude Code simply deletes the files. Rescuing tasks therefore
requires a real snapshot; rescuing identity does not.

**Intended outcome.** One short command from a cold terminal — `forgectl resume` — lists
every recent session across every repo with its name, repo, branch, and last activity, and
lands you back inside the one you pick, with its task list intact.

**Tier:** Substantial. **Not** security-sensitive, but not read-only either — the write
surface is worth stating precisely, since a threat model built on "read-only" would be
wrong:

- **Reads** the whole `~/.claude` session corpus (history, transcripts, registry, task
  dirs, team configs).
- **Writes** the snapshot store under forgectl's own config dir, **and** restores task
  JSON back into `~/.claude/tasks/<session-id>/` — a write into Claude Code's own tree.
  Restore never overwrites an existing file and only raises `.highwatermark`, which is
  what keeps that write safe rather than merely small.
- No new network, auth, or credential surface.

The one hardening obligation is that disk-sourced strings reach a terminal — covered by
reusing `sanitizeTerm` on both the text and `--json` paths.

---

## Step 0 — Clear the abandoned rebase (done, standalone)

`.git/rebase-merge` had been on disk since **Jul 17**, so every `git status` for 13 days
claimed a rebase in progress. It was fully stale: the todo list was empty, and its branch
`feat/docs-serve-pr1` had landed on `origin/main` as `634c58f` + `e03d122`. Local `main`
was **54 commits behind** `origin/main`.

**Not `git rebase --abort`.** `HEAD` was `refs/heads/main` — someone checked out main
mid-rebase — so `--abort` would have reset to `orig-head` (`d0e1d55`) and checked out
`feat/docs-serve-pr1`, moving the checkout off main. `--quit` clears the state and leaves
HEAD alone.

1. Preserved the modified `internal/workflow/exec.go` as a reference patch, then
   `git restore`d it. It added `slog` lines to `NewRegistry`, but written against the
   **pre-checkpoint** `exec.go`; `origin/main`'s version has `Recorder`/`resumeFrom`, so
   the patch cannot apply cleanly and is reference only.
2. `git rebase --quit`
3. Deleted the untracked copies that would block a fast-forward — all **older versions of
   files `origin/main` already tracks** (verified file-by-file against `origin/main`):
   `docs/discovery/2026-07-17-audience-dossier.md` (byte-identical),
   `docs/discovery/2026-07-17-audience-review-readme-cli.md` (pre-markdownlint, still says
   "mart" where main says "concordance"), and six superseded `.claude/agents/audience-*.md`.
   Left `.claude/agent-memory/` and `.claude/sessions/` alone.
4. `git merge --ff-only origin/main` — never `reset --hard`; ff-only is the
   non-destructive equivalent.
5. Verified: `git log --oneline -1` == `e5a5033`, no rebase state, clean tree.

Folded in while we are here: `.claude/sessions/` joins `.claude/intros/` in `.gitignore`
— it is per-session scratch that shows up untracked in every session (today it is only in
`.git/info/exclude`, which is machine-local).

---

## Step 1 — The issue

forgectl#179, carrying the two findings above as justification. This PR closes it.
Adjacent but not overlapping: #8 wants `forgectl cmux launch` to *create* sessions;
nothing covered *recovering* them.

## Step 2 — Session entry posture

Branch-mode primary checkout, Substantial work → worktree first, and the plan is the first
thing on the branch: worktree at `/tmp/wt-fgl-resume` on `plan/resume-module` off
`origin/main`; this plan doc committed first (Lane A — code-coupled, rides the PR);
`git push -u origin plan/resume-module --no-follow-tags`; draft PR.

## Step 3 — `internal/resume` — the domain package

New package. Pure functions over injected paths so tests use a fixture `$HOME`; no
`os.UserHomeDir()` inside the risk-bearing core (mirrors `launch.resolve`).

**`resume.Session`** — the merged record:

```go
type Session struct {
    ID          string    // Claude Code session uuid
    Cwd         string    // where --resume must run
    Name        string    // /rename name, else ai-title, else ""
    NameSource  string    // "rename" | "ai-title" | "lane" | ""
    Repo        string    // filepath.Base(Cwd) — the picker's column
    Branch      string    // gitBranch from the transcript, if present
    LastPrompt  string    // history.jsonl display, truncated
    LastActive  time.Time
    Version     string    // Claude Code version that wrote it
    Live        bool      // registry pid is alive right now
    Tasks       []Task    // from the snapshot store
}
```

Files:

- **`index.go`** — `Scan(opts)` merges the five sources into `[]Session`, newest first,
  keyed on session id. Precedence for `Name`: registry `name` > store snapshot `name` >
  transcript `ai-title` > repo lane file > `""`. `history.jsonl` is the spine (it is the
  only global, append-only index); the transcript is opened **only** for sessions that
  survive the limit cut, so a 1208-file corpus is not parsed to show 10 rows.

  **Corrected during execution:** the spine is not the whole skeleton. Prompts are
  recorded under whichever session id was in force when they were typed, so a session
  that arrived through `/clear` can be live and entirely absent from `history.jsonl` —
  every currently-running session on this machine was, including this one. The store
  **and the registry** therefore contribute ids of their own rather than only decorating
  history's, and a registry `updatedAt` raises `LastActive` when it beats the last prompt.
- **`registry.go`** — reads `~/.claude/sessions/*.json`; liveness via
  `syscall.Kill(pid, 0)` (treat `EPERM` as alive, `ESRCH` as dead). A stale file whose pid
  is dead is reported dead, never trusted for `status`.
- **`store.go`** — the snapshot store: one JSON file per session at
  `config.ResumeStoreDir()/<session-id>.json`, i.e.
  `<os.UserConfigDir()>/forgectl/resume-sessions/`, matching every other forgectl path
  (`PrSessionsDir`, `NetCachePath`, `WorkflowStateDir`) — *not* `~/.local/state`.
  `ResumeStoreDir()` goes in `internal/config/config.go` next to `PrSessionsDir`, same
  doc-comment shape.
- **`snapshot.go`** — `Snapshot()`: for each live registry entry, write/merge a store file
  carrying id, cwd, name + nameSource, version, last-seen, and a **copy of every surviving
  task JSON**. Idempotent, and it must never lose tasks it captured earlier just because
  Claude Code has since deleted them — merge by task id, newest body wins.
- **`tasks.go`** — `TaskDir(id)` resolves the dialect, and `Restore(id)` writes back only
  the task files **missing** from the live dir, then sets `.highwatermark` to the max id.
  A live session's own file always wins; we never overwrite.

  **Version coupling — corrected during execution (2026-07-30).** The plan had the two
  dialects backwards, and the error was load-bearing enough to stop on. They are not old
  and new; they are **per-session** and **per-team**, both current:

  - `tasks/<full-session-uuid>/` is keyed on the session id. **199 of 199** such dirs on
    this machine are exact session ids.
  - `tasks/session-<8hex>/` is keyed on the **lead session id of an agent team**
    (`~/.claude/teams/session-<8hex>/config.json`), which survives a `/clear` while the
    session id rotates. Only **139 of 360** match any session-id prefix — this session's
    own tasks were in `session-571bdef3/` while its id was `1557142e-…`.

  Nothing on disk records the association durably: the transcript carries no team, agent,
  or task-directory reference at all. So the store *is* that index — the first `Snapshot`
  to see a live session beside its task dir writes the pairing down, and every later
  `Restore` reads it back. `ResolveTaskDir` falls back to joining
  `teams/*/config.json`'s member `cwd` against the session cwd, but only once, and only
  for a live session.

  `Restore` targets the **per-session** dir even when capture came from a team one,
  because that is the dir a resumed session reads. Verified live rather than reasoned
  about: a headless session wrote `tasks/<its id>/1.json`; `claude --resume <id>` kept the
  same session id, reused that dir, saw the task through `TaskList`, and appended
  `2.json`. The `debt:` comment names the remaining trigger — *if a future Claude Code
  stops keying per-session dirs on the session id, restore writes where nothing reads* —
  and `DriftCheck` (surfaced by `forgectl doctor`) is the tripwire.

## Step 4 — `internal/cli/resume.go` — the verb

`resumeModule` at `module.TierExtension`, added to `allModules()` in `modules.go`, **with
the completeness pins in `modules_test.go` updated in the same diff** (ADR-0005 growth
policy — that test is the gate).

- **`forgectl resume`** — `huh` picker (same dependency and shape as
  `pr_pick.go`/`pickPRs`), newest first, columns: name · repo · branch · relative
  last-active · liveness. Enter resumes.
- **`forgectl resume <filter>`** — substring match on repo/name/cwd. Exactly one hit skips
  the picker; several filter it; none is a clean error.
- **`forgectl resume ls [--json]`** — list without acting. This is the inspection surface
  (a `--print` flag was considered and declined — see Alternatives).
- **`forgectl resume snapshot [--quiet]`** — the capture verb. **Always exits 0** and
  writes nothing to stdout under `--quiet`: it runs on a hook and must never be able to
  break a turn.

**Reuse, not reinvention:**

- **`sanitizeTerm`** already lives in `internal/cli/sessions.go` — *same package*. Every
  disk-sourced string that reaches the terminal (name, ai-title, last prompt, cwd) goes
  through it. Same hardening as `fix/162-terminal-sanitize`, and the `--json` path must
  call it explicitly: `encoding/json` escapes only 0x00–0x1F, so DEL and the C1 range
  (including 0x9B = single-byte CSI) would otherwise reach a terminal raw. `printWhyHits`
  documents exactly this.
- **`launch.Resolve(deps.Cfg.Launch, session.Cwd)`** — a **pure function of the global
  config and an arbitrary cwd**, so resuming into another repo gets that repo's profile for
  free. No per-project config loading needed.
- **`launch.ClaudePath()`** for the binary.

**Refuse to resume a live session.** If the picked session's registry pid is alive, error
out — two processes on one transcript is corruption, not a resume. This is the main new
failure mode the feature introduces.

## Step 5 — The resume action

Chosen path: chdir + exec-replace with the project's profile.

1. `resume.Restore(id)` — put missing task files back **before** exec.
2. `os.Chdir(session.Cwd)` — `--resume` resolves against the project dir, which is the
   whole reason the cwd is load-bearing.
3. `syscall.Exec(claudePath, argv, env)` with a new additive helper in `internal/launch`:

   ```go
   // ResumeArgs builds the interactive posture for resuming one known session id.
   func ResumeArgs(p Profile, model, sessionID string, fork bool) []string
   ```

   Additive on purpose — today's `SessionArgs` appends a **bare** `--resume` (which just
   opens Claude Code's own picker), and changing its signature would churn `launch_test.go`
   for no gain. `--fork` maps to `--resume <id> --fork-session`.

`launch` already execs in place, so this reuses an established path rather than
introducing one. Worth a glance at `plan/153-launch-auto-exit` before writing, since that
branch has an open question about `syscall.Exec`; if it lands a constraint, inherit it
rather than diverging.

## Step 6 — Wire the snapshot hook

`Stop` hook (fires at every turn end — cheap: a handful of registry files plus a few task
dirs, and idempotent). Documented in the README; **not** auto-installed — `--install-hook`
is scope the user did not ask for.

```json
{ "hooks": { "Stop": [ { "hooks": [
  { "type": "command", "command": "forgectl resume snapshot --quiet" } ] } ] } }
```

---

## Files touched

| Path | Change |
|---|---|
| `internal/resume/{index,registry,store,snapshot,tasks}.go` | new domain package |
| `internal/resume/*_test.go` | fixture `$HOME`, built programmatically rather than as checked-in `testdata/` — each test needs a different corner of the tree (a dead pid here, a team config there), and a builder makes each one state its own dependencies |
| `internal/cli/resume_test.go` | render/sanitize coverage, plus the `resumePaths` seam that keeps `snapshot`'s test off the developer's real `~/.claude` |
| `internal/cli/resume.go` | verb + `resumeModule` manifest |
| `internal/cli/modules.go` | register `resumeModule` |
| `internal/cli/modules_test.go` | completeness pins (ADR-0005 gate) |
| `internal/config/config.go` | `ResumeStoreDir()` |
| `internal/launch/launch.go` | additive `ResumeArgs` |
| `internal/cli/doctor.go` | task-dialect drift check |
| `.gitignore` | `.claude/sessions/` |
| `README.md`, `docs/plans/2026-07-30-resume-module.md` | verb docs + the hook snippet |

## Verification

Evidence required before any completion claim:

1. `gofmt -l . | grep -v '^$'` empty; `go build ./...`; `go vet ./...`.
2. `go test ./internal/resume/... ./internal/cli/... ./internal/launch/... ./internal/config/...`
   — the fixture-`$HOME` unit tests must cover: merge precedence for `Name`; ordering by
   last-active; a dead pid classified dead and a live one classified live; a
   control-byte-laden session name rendered inert on **both** the text and `--json` paths;
   `Restore` refusing to overwrite an existing task file.
3. **Read-only against the real tree:** `forgectl resume ls --json | jq` lists today's
   sessions with correct `cwd`, `repo`, and names — cross-check two rows against
   `~/.claude/sessions/*.json`.
4. **Snapshot round-trip:** run `forgectl resume snapshot`, then confirm a store file
   exists for the current session carrying its name and cwd. Create two tasks, snapshot,
   delete the task JSON by hand, run the restore path, and confirm both files return with a
   correct `.highwatermark`.
5. **Live-session refusal:** `forgectl resume` against the currently-running session must
   refuse, naming the live pid.
6. **End-to-end (manual, the actual acceptance test):** quit the terminal, open a new one,
   run `forgectl resume`, pick the session, and confirm you land in the right cwd with the
   task list present.
7. Pre-PR polish pass before the PR leaves draft.

## Execution record (2026-07-30)

Evidence for each verification item, in order:

1. `gofmt -l ./internal` empty; `go build ./...` and `go vet ./...` clean.
2. `go test ./...` green across every package.
3. `forgectl resume ls --json` listed the live sessions with correct cwd, repo, branch,
   pid, and `/rename` names; two rows cross-checked against `~/.claude/sessions/*.json`.
   This is what surfaced the history-coverage gap above — before the registry fold-in, the
   live sessions were missing entirely.
4. `forgectl resume snapshot` → `snapshotted 7 live session(s), 11 task(s) held`. The store
   record for this session carried its name, cwd, version, all 7 tasks, and
   `task_dir: ~/.claude/tasks/session-571bdef3` — the team-dialect join, resolved on real
   data. Round trip proved on the probe session: 2 real tasks captured, the directory
   deleted, `RestoreFor` returned them byte-identical with `.highwatermark` = 2, and a
   second run was a no-op (`written=0 skipped=2`).
5. Live-session refusal: `forgectl resume <this id>` exited 1 naming pid 93657.
6. End-to-end (quit the terminal, `forgectl resume`, land back in place) is the one item
   only the operator can run — it needs the restart this feature exists for.
7. Pre-PR polish pass before the PR left draft.

Three defects came out of step 3 alone, none of which the fixtures could have caught: the
history-coverage gap, a snapshot test that wrote to the real store, and picker columns
padded in bytes rather than runes. Fixed in `44115da`.

## Alternatives declined

- **A persistence daemon / launchd poller for identity.** Rejected on evidence:
  `history.jsonl` and the transcripts already hold id + cwd + title permanently, so a
  poller would duplicate durable data and add an always-on process — against the launchd
  traps already documented (no inherited cwd, no shell env, TCC prompts). Snapshotting is
  scoped to the two things that genuinely vanish: the `/rename` name and task bodies.
- **`forgectl sessions resume`.** `sessions` is the Postgres concordance ETL; it needs a
  DSN and speaks pgx. A zero-dependency local picker there would make one group mean two
  things.
- **`forgectl launch resume`.** Reuses more scaffolding, but `launch` is cwd-bound and it
  is longer to type at exactly the moment you have no context. We reuse the `launch`
  *package* instead and keep the verb short.
- **`--print` instead of exec-replace.** `resume ls --json` covers inspection; a second
  emit-only path is a flag and a test for a need already met.
- **Restoring tasks across a session-id fork.** Would have mattered in June (93/510
  transcripts forked); 0 of 474 July transcripts do. Not worth building for behavior Claude
  Code has stopped exhibiting.

## Note on plan review

The adversarial plan-review panel was **not** dispatched — the authoring session carried a
standing instruction not to invoke the Agent tool unless asked.
