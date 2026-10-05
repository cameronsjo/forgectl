---
session_id: "23916e1d-e8cf-4d35-8994-3d4840f295e3"
model: "claude-opus-5-5"
harness: "claude-code 2.1.287"
machine: "cf6e768835c7"
approved_session_id: "c70b336a-7809-4bea-90a6-42015e71e281"
status: in-flight
next: "v0.25.0 is released and installed on this machine. Operator: forgectl upgrade on the other working machine, then mint and store the vikunja-write bot token. Then a session runs the live probe with -liveprobe.keychain-service=vikunja-write before any real close. Then PR 1035, cadence PR 1581, homelab#1220."
branch: feat/tasks-close-path
pr: "https://github.com/cameronsjo/forgectl/pull/1026"
updated: 2026-10-04
date: 2026-10-01
---

# A close path for board tasks (cameronsjo/forgectl#1022)

## Context

Agents can open a board task (`create_task` over `forgectl tasks mcp`) and nothing in forgectl lets them finish one. The operator closes work by hand in the web UI. Issue cameronsjo/forgectl#1022 asks for a plan and two decisions: how a task gets closed, and whether agent-created tasks need the operator's go. This plan leaves #1022 open; the build PR resolves it.

An earlier design put a `done` verb behind a 14-day usage test that counted finished tasks. The test failed and the verb was cancelled with it. The test could not pass: it counted closes while the only thing that lets an agent close sat behind it. This plan ships the closer first and measures afterwards.

**Measured 2026-10-01** from the local cache (read-only credential, fetched 17:13 UTC), with the recipe in Task 11:

```json
{"open":48,"done":9,"agent_closed":0,"open_by_project":[{"project":1,"n":21},{"project":2,"n":27}]}
```

The done count has not moved in the ten days since the failed test's last reading. 26 of the 48 open tasks carry a `created-by:` trailer. Only 9 name a PR or issue anywhere in their text.

**Verified against `origin/main` at `649a320` (v0.24.0):**

- CLI verbs are `ls`, `show`, `ready`, `mcp`. MCP tools are `list_projects`, `list_tasks`, `get_task`, `ready_tasks`, `create_task`, `add_comment`. Neither surface can update a task.
- `Client.do` already carries any method with a JSON body through the credentialed, host-pinned path (`internal/tasks/client.go`). No new transport is needed.
- Every CLI verb and the stdio MCP transport read one keychain entry, default `vikunja-readonly`. The HTTP transport reads `--token-file`.
- `tasks` has no `RunE` and no argument check, so an unknown subcommand prints the parent help and exits 0. Measured on the installed 0.23.0: `forgectl tasks done --help` and `forgectl tasks done 999999` both exit 0; `forgectl tasks done 999999 --evidence x` exits 1 with `Unknown flag: --evidence`.
- A missing keychain entry exits 1 on the read verbs today, not 3.
- ADR 0009 § Consequences: "Adding a *write* verb is a separate decision with a separate review." No PR that built this control carries a recorded security review.
- The repo squash-merges with the PR title as the commit subject, and the nightly ship workflow runs without anyone's go.

**Not verified. The build settles each one first (Task 2):**

- That an agent write credential may update a task. Operator records say yes.
- What Vikunja does to fields left out of an update body, to a repeating task marked done, and to keys echoed back that it computes itself.
- Which projects each agent credential reaches. Operator records say each agent has its own bot, and each bot is shared a different single project. A close outside that project fails its pre-read. The open tasks split 21 and 27 across two projects.

## Goal

An agent that finishes a piece of work can mark its board task done, wherever it can open one, with a record on the task of who closed it and why. The plan also says who closes and when, how a session knows which task is its own, and how the closer reaches the installed binary and the deployed MCP server.

## Decisions for the operator

Approving this plan in the session publishes it as a draft PR (Task 0) and nothing more. The decisions below are ruled on that PR. The build starts after the ruling.

- **D1 — Who closes, and when.** Recommended, in this order: (a) the session that merges the work closes the task at merge, naming the merged PR; (b) when the operator merges by hand, the next session that resumes that plan or repo closes it, reading the task id from the plan's `card:` or the PR's `Board-Task:` line and confirming the PR is `MERGED`; (c) the operator closes in the web UI any task only he can finish. In a turn the harness declares autonomous or background, a session does not close: it lists the id as ready to close in its final report. The narrower alternative is (a) and (c) only, with no close on resume.
- **D2 — Does creating a task need the operator's go.** Recommended: no go when the creating agent records what will finish the task (a repo plus an issue, PR, or plan). A go stays required for a task only the operator can finish. This is guidance in skill text (Task 10). The binary does not enforce it.
- **D3 — The existing 48 open tasks.** 39 name no PR or issue, so no join key can be recovered. Recommended: one triage pass with the operator after the closer ships, tracked by an issue filed in Task 1 and excluded from Task 11's reading.
- **D4 — Undoing a wrong close.** Recommended: no `reopen` verb, provided Task 2 shows a close changes only the done state, its timestamps, the board column, and the trailer. If Task 2 shows more, `reopen` joins this build. Either way the close result shows the closed task's title and project, so a wrong close is visible to the caller at once.
- **D5 — Which bot closes, and how far it reaches.** Each agent's write credential reaches only the project shared with its bot. Recommended: each harness closes with its own bot's credential, and the operator widens a bot's share only for a project he wants that agent to close in. Tasks outside the share stay his to close. The alternative, one widely shared closing bot, loses per-agent attribution. Closing with a token minted on the operator's own account is not an option (Constraint 12).
- **D6 — Gateway authorization for `complete_task`.** Recommended: its own rule, granted per consumer, not added to the existing write rule. Adding it there gives close to every current write holder.
- **D7 — A close cap.** Recommended: at most 10 successful closes per MCP session, a named constant, refused beyond that with a clear error. This bounds a loop driven by injected board text. It is a brake, not a boundary: a client can open a new session. The boundary is the project share.
- **D8 — The write-verb decision itself.** ADR 0009 calls a write verb a separate decision with a separate review. Recommended: ruling on this PR is that decision, and the review is Task 1's pre-build pass plus Task 7.
- **D9 — Where a keychain credential may be sent.** Today `--host` on any `tasks` verb sends the keychain token to whatever host is named, and the host pin accepts any public address on TLS. That is tolerable for a read-only token and not for a write token on the same machine. Recommended: a keychain credential goes only to the default host or a host listed in the user's config file; any other `--host` is refused with exit 4 on every `tasks` verb. This adds a small config key and changes behavior for anyone pointing the CLI at their own instance by flag alone.

### Ruling (2026-10-02)

The operator delegated the ruling to the executing session ("use your best judgement: decide every open item"), with merges withheld. Under that delegation every recommendation above is taken as written: D1 (a), (b), and (c); D2 no go when the finisher is recorded; D3 one triage pass after the closer ships; D4 no `reopen`, conditional on the Task 2 probe; D5 each harness closes with its own bot; D6 a separate gateway rule; D7 a cap of 10; D8 this ruling is the write-verb decision; D9 the host rule is built.

The operator can overturn any of these before he merges the build PR. The merge is his, and it is the go to release.

## Loop

| Thing created | Opened by (surfaces) | Closed by (surfaces) | Who closes |
|---|---|---|---|
| Board task | existing `create_task`: MCP stdio when started with a write credential; MCP HTTP behind the gateway | Task 4 `complete_task` on MCP stdio under the same credential that opened it; Task 5 `tasks done` on the CLI; MCP HTTP at Task 9 | D1: the merging session, else the next resuming session, else the operator |
| Join key (`card:` in a plan, `Board-Task:` in a PR body) | Task 10 | Task 10's resume rule reads it and closes the task; the key itself ends when the plan's `status:` is `done` | The resuming session |
| Scratch tasks on the live board | Task 2 probe | Task 2 marks each done and prints every id it created on every exit path. Removal is existing: the operator deletes them in the web UI. | The probe, then the operator |
| 14-day measurement window | Task 11 day-1 reading | Task 11 day-14 reading and ruling | Whoever picks up the tracking issue Task 11 files, on the date it names |

No row is deferred. The HTTP surface's closer lives in the deployment repo, so Task 9 is a precondition of Task 11: the window does not open while any surface can open and not close.

## Alternatives declined

- **Report names the task, the operator clicks done** — keeps outflow manual. That is the pattern the failed test measured.
- **A scheduled sweep that closes tasks whose PR merged** — puts a write credential in an unattended job. D1(b) gets the same effect from an attended session. Revisit if Task 11 fails.
- **Close by title match** — a guess. The join key is an explicit id or nothing.
- **Let an agent close only tasks carrying a `created-by:` trailer** — the trailer is board text anyone can write, so it cannot authorize anything (ADR 0009 §8).
- **Reuse `--keychain-service` for the write verb** — teaches every caller to pass the write entry to read verbs too.
- **Record evidence as a comment** — comment scope differs between agent credentials. A trailer rides the update call every closing credential already needs.
- **Close without a trailer when it will not fit** — leaves a close with no record. The close is refused instead.
- **Patch the local cache after a close** — the read verbs rewrite the whole cache on every run, and a refetch under the write credential would shrink the cache to one project. The next read or scheduled refresh corrects it.
- **CLI `add`, dedup at create, `tasks start` dispatch** — not asked for by #1022.

## Panel

Panel: plan-reviewer (two lenses) + security-posture-reviewer (two rounds) + red-team-reviewer + operability-reviewer + agent-experience-reviewer ran — 127 findings, 119 folded in, 8 declined (see Panel review — findings declined)

Counts include the same finding raised by more than one seat. The second security round ruled on all twelve lines the operability and agent-experience seats routed to it. No security finding was declined; the one that adds build scope is D9, put to the operator.

## Panel review — findings declined

- **[Tasks 4-5] A local JSONL close log with a `tasks closes` reader verb** — the trailer is now mandatory and a close-record line is always emitted. A second store and a new verb are more surface than a reversible close needs.
- **[Task 4] An optional `project_id` input that must match before a close** — the result already shows the closed task's project and title. The security seat ruled the match a convenience, not a control.
- **[Tasks 8, 11] Committed check scripts for the release steps and the measurement** — each step carries its check inline, and the measurement recipe is recorded with its measured output. Promote to a script if either is run a third time.
- **[Naming] One word for the action across tool, verb, and trailer** — `complete_task` follows the six existing verb_noun tool names, and `done` is the board's own word on the CLI.
- **[Task 5] Harden the cache patch (atomic write, embedded relation copies, `fetched_at`)** — the patch is removed instead; see Alternatives declined.
- **[Architecture] Detect two sessions closing the same task at once** — the second writer's trailer replaces the first. Both name real evidence, and the window is two requests wide. Recorded in the ADR with the lost update.
- **[D2] Enforce the join key with a required `create_task` field** — D2 is stated as guidance. A required field is revisited if Task 11 shows new tasks still arriving without a key.
- **[Inherited] The HTTP container staying healthy with a dead token, no write logging on the two existing write tools, a non-atomic cache write** — real, and older than this plan. Task 1 files them as one issue so they are not dropped.

## Architecture

One client method, two thin surfaces.

```text
MCP complete_task ─┐
                   ├─→ Client.CompleteTask(ctx, CloseRequest)
CLI tasks done ────┘     1. GET /tasks/{id}, raw JSON     pre-read; any failure → no write
                         2. already done → return already_done, zero writes
                         3. repeating task → refuse
                         4. build trailer; description + trailer over the limit → refuse
                         5. copy the raw object, replace only `done` and `description`
                         6. POST /tasks/{id} through Client.do
                         7. GET /tasks/{id} again; compare with the pre-read
```

- **Raw round trip.** The body is a `map[string]json.RawMessage` decoded from step 1 with exactly two keys replaced. `tasks.Task` models ten fields and would drop the rest. If Task 2 shows the server refuses or mangles an echoed key, that key is removed by a list recorded from the probe.
- **Step 7 decides the outcome.** `closed` when the read-back says done. `not_confirmed` when the write or the read-back failed after the request was sent: the write may have landed, and the message says to read the task. A read-back that differs from the pre-read outside the expected keys still reports `closed` and names the changed keys.
- **Retries cannot stack trailers.** An already-done task is never written. An open task whose description already ends with a `closed-by:` line carrying the same evidence is written without a second trailer.
- **Lost update, accepted.** An edit made in the web UI between steps 1 and 6 is overwritten. The window is two requests wide. ADR 0009 records it.
- **One trailer sanitizer for both writes.** Closer and evidence go through a single-line sanitizer that refuses CR, LF, tab, and control characters in evidence, strips them from the client-declared closer name, and escapes `<`, `>`, and `&`. `sanitizeBoardText` keeps newlines by design and is not enough. The existing `created-by:` trailer moves onto the same sanitizer.
- **Per-verb credentials on the CLI.** Read verbs keep `--keychain-service`. `done` reads `--write-keychain-service`, default `vikunja-write`, never falls back, and refuses an explicit `--keychain-service`.
- **MCP credentials are unchanged.** The server holds the token it was started with. Under a read-only token `complete_task` is refused, as `create_task` is today. Whoever can open over MCP can close over MCP with the same credential.
- **Join key.** `card: [<id>, …]` in plan frontmatter and one `Board-Task: <id>` line per task in a PR body. Both hold the global task id the API returns, never the per-project `#N` the web UI shows.

## Global Constraints

Every task obeys these. Sources: ADR 0009 and the existing `internal/tasks` code.

1. The token stays a `tasks.Token`. It never enters argv, a shell variable, a log line, an error string, the cache, or `--json` output. No environment-variable source. No `curl`, and no `security … -w` in a session shell.
2. A write is preceded by a passing read of the same object and refused when that read fails.
3. Status is asserted before a body is decoded. An auth failure never falls back to cache, and `done` never reads the cache.
4. Exit codes: unreachable 2, server rejected the credential 3, host refused 4, everything else 1. A missing keychain entry is 1, as on the read verbs. `--json` failures use the existing stderr object in `docs/json-contract.md` with the `code` values Task 5 lists.
5. Evidence is one line, at most 300 characters (`maxEvidenceRunes`). Closer is at most 100. Both pass the trailer sanitizer.
6. MCP tool names are raw names. The deployment's gateway authorizes by raw name and denies an unlisted tool.
7. One task id per close call. No bulk close.
8. This repo gains no new hostname, no bot identity, no token detail, and no keychain entry name other than `vikunja-readonly` and the generic default `vikunja-write`. Operator-side specifics stay in the deployment repo's runbook.
9. `CHANGELOG.md` is written by release-please only. The build PR's title, `feat(tasks): …`, is the changelog entry because the repo squash-merges.
10. Every commit message ends with the producer-tuple block (`Session-Id`, `Model`, `Harness`, `Machine`) in the same paragraph as the co-author line.
11. Scope is the two working machines. Unattended executors get no write credential, and a declared-autonomous turn on a working machine does not close; those tasks close through D1(b). The CLI verb needs the macOS keychain.
12. The entry `done` reads holds a bot-user token scoped to task read and update and shared only the projects that bot may close in. It is never a token minted on the operator's own account. Any same-user process can read a keychain entry, so the token's scope and share are the boundary, not this binary's checks.
13. Skill text never tells a session to read the keychain entry directly.

## Pre-build security review — amendments (2026-10-02)

Task 1's review returned 1 Critical, 9 Important, and 12 Nit findings. Every Critical and Important is taken. Where a line below conflicts with a task's own text, this section wins.

**Task 2 (C1, I4):**

- A write credential must not sit in the keychain before the D9 host rule is installed: until then any `tasks` verb sends a named keychain entry to any public host with a valid certificate. The probe therefore uses a short-expiry scratch bot token under the entry `forgectl-liveprobe-scratch`, never the entry `done` reads, and has no host flag. The operator deletes the entry and revokes the token as the probe's last step.
- The probe gains scratch task C (`repeat_mode` set, `repeat_after` zero) and scratch task D, which is updated with a minimal body of `done` and `description` only to measure which keys the server resets. Four writes per phase.

**Task 3 (I1, I2, I3, I4, I5, N1, N2, N8):**

- Pre-read shape is checked, and any failure is zero writes: the body is a JSON object of at most `maxCloseBodyBytes`; `id` is a number equal to the requested id; `done` is a JSON boolean; `description` is a string, absent, or null; `repeat_after` and `repeat_mode` are each absent or a number, and a non-zero value in either is `ErrRepeatingTask`. Values stay `json.RawMessage` end to end and are never decoded through `any`.
- The retry rule changes. A matching `closed-by:` line is never a reason to skip the trailer, because that text is writable by anyone who can edit the task. If the description's last line matches the strict trailer grammar, that one line is removed and this call's trailer is appended. `EvidenceRecorded` is true only when this call's line is the last line of the read-back.
- The trailer sanitizer refuses evidence that is not valid UTF-8 or that holds any rune where `termsafe.IsUnsafeTerminalRune`, `termsafe.IsInvisibleRune`, or `!unicode.IsGraphic` is true. That covers CR, LF, tab, U+000B, U+000C, U+0085, U+2028, U+2029, bidi overrides, and zero-width characters. Evidence is also refused when it contains `tk_` followed by 20 or more hex characters anywhere in the text.
- The closer is reduced to the allowlist `[A-Za-z0-9._()/@ -]`, capped at 100 runes, with every occurrence of ` via ` collapsed so a closer cannot forge the trailer's own fields. The `created-by:` trailer uses the same closer sanitizer.
- `CloseResult.ChangedKeys` names a key only when it matches `^[a-z_]{1,40}$` and is in the known task key set; any other difference is counted in `CloseResult.UnnamedChanges int`.
- One expected-to-change list, in code and in the fixtures' notes: `done`, `done_at`, `updated`, `description`, `bucket_id`, `position`.
- A test pins the production transport: `CheckRedirect` returns `http.ErrUseLastResponse`, `Proxy` is nil, the TLS floor is 1.2, `InsecureSkipVerify` is false, and `RootCAs` is nil. Each is staged broken once.
- `security` and `route` are run by absolute path (`/usr/bin/security`, `/sbin/route`). `AssertVikunja` sends its `/info` request without the Authorization header. `no_env_token_test.go` also covers `structured.go`, `hostpin.go`, and `cache.go`.

**Task 4 (I5, I6, I7, I8, N7):**

- The cap is keyed on the MCP session under a mutex. A nil session is one shared bucket, never "no cap". A slot is reserved before the POST and released when no POST was sent. An already-done task does not count.
- A close record is one JSON line on the record writer for every call that sent a POST, with `outcome` of `closed`, `not_confirmed`, `write_refused`, or `unauthorized`, and one for every cap refusal with `outcome` `close_cap`. Fields: UTC time, task id, project id, surface, closer, evidence, credential source name, host, outcome. Never the token. It does not go through the global logger.
- Tool text names changed keys only from `ChangedKeys`, and otherwise gives the count.
- The smoke check asserts the exact code `complete_task: not_found:` and is labelled as proof that the tool is registered and its pre-read gates the write. It is not a scope check: a missing id answers the same under any credential. The scope proof is Task 3 test (d) and Task 9's read-only consumer check.
- `get_task` shows a description's last line separately, inside the fence, when the description was truncated, so a reader sees the closing trailer.

**Task 5 (I6, I9, N3, N4):**

- The host rule has two enforcement points. `ReadToken` takes the allowed host set and the returned token remembers it, so `NewClient` refuses to build a client that would send a keychain token to any other host, whoever calls it. The CLI also checks before any keychain read, through one function every `tasks` verb calls, and a test walks the verb list and asserts exit 4 with zero keychain reads.
- A host is a plain hostname: letters, digits, `.`, and `-`; no port, userinfo, path, trailing dot, or IP literal. Comparison is lower-cased and exact against the default host and the config list. Config entries are validated at load.
- `mcp` over stdio is covered. `mcp --http` reads `--token-file`, which carries no host restriction of its own, and stays governed by the required `--pin-ip` list.
- The CLI appends each close record to a file under the forgectl config directory as well as writing it to stderr. The file is independent of `log_level`. The same user can erase it; it outlives the session, which stderr does not. There is no reader verb.
- `--closer` stays self-declared with the default `cli`, and the help says so. It does not read a harness session variable: this package reads no environment variable by design.
- A keychain service name outside `^[A-Za-z0-9._-]{1,64}$` is refused before the keychain read, on every verb. `tasks show` prints a relation kind through the same sanitizer as a title.

**Task 6:** §3 records that the read token had the any-public-host exposure from the start and that D9 is not a boundary against a process running as the user, which can edit the config file. §3b states that TLS as the control depends on the system trust store. §8 names, per surface, which record is authoritative and who can alter it.

**Task 9:** the deployment issue also measures whether the gateway holds one upstream session or one per consumer, because that decides what the cap of 10 bounds.

**Not built here, added to cameronsjo/forgectl#1025:** a failed cache write is logged through a logger that discards by default; a mistyped `log_level` turns logging off silently; `create_task` and `add_comment` have no session cap; transport error text reaches an MCP client unfenced.

## Orchestrator

**Driver:** opus — security posture: a forgectl surface gains a write call and a second credential source

---

## Tasks

Each dispatched task works in the worktree Task 1 creates and replies per its `Report:` line. `[REPORT_PATH]` is substituted by the orchestrator with an absolute path outside the worktree before dispatch.

### Task 0 — Publish this plan (this session, then stop)

**Files:** Create `docs/plans/2026-10-01-tasks-close-path.md`

**Dispatch:** In-context · **Report:** —

- [x] `git worktree add` from `origin/main` on `plan/tasks-close-path`
- [x] Copy this plan in; set `approved_session_id`
- [x] Run the redaction scan over the plan and the PR body
- [x] Commit, `push -u`, open a draft PR that references #1022 with no closing keyword
- [x] Write `.claude/intros/2026-10-01-tasks-close-path.status.md` in the primary checkout (gitignored, never committed): PR URL, D1-D9 with recommendations, and the fact that gateway clients cannot close until Task 9
- [x] Stop. Nothing below runs until the operator rules on the PR.

### Task 1 — Setup and pre-build security review

**Files:** none changed

**Dispatch:** In-context for setup; the review is the dedicated security-review agent on Opus, or the built-in security review command or an Opus reviewer handed the files if that agent type does not resolve · **Report:** `[REPORT_PATH]`

- [x] Record the ruling on D1-D9 in this plan; amend any task the ruling changes
- [x] Create `feat/tasks-close-path` in its own worktree from `origin/main`; open the draft build PR titled `feat(tasks): close a board task from the CLI and MCP`
- [x] File three issues: Task 9 on the deployment repo, Task 10 on `cameronsjo/cadence`, and D3's triage on `cameronsjo/forgectl` Filed: cameronsjo/homelab#1220 (Task 9), cameronsjo/cadence#1570 (Task 10), cameronsjo/forgectl#1024 (D3 triage).
- [x] File one forgectl issue for inherited gaps this plan does not fix: the HTTP container stays healthy with a dead token; `create_task` and `add_comment` log nothing; `SaveCache` is not atomic Filed: cameronsjo/forgectl#1025.
- [x] Security review of the control as it stands, whole files: `docs/adr/0009-credentialed-http-client-posture.md`, `internal/tasks/token.go`, `client.go`, `write.go`, `mcp.go`, `structured.go`, `hostpin.go`, `errors.go`, `types.go`, `cache.go`, `internal/cli/tasks.go`, `internal/cli/tasks_mcp.go`, `internal/config/config.go` (`SetupLogger`), `scripts/mcp-stdio-smoke.sh`. It must answer, among its own questions, where `--host` can send a keychain credential (D9).
- [x] Fold Critical and Important findings into Tasks 2-6. The review finishes before Task 2's first live write. Folded as "Pre-build security review — amendments".

### Task 2 — Probe update behavior on the real board

**Files:**
- Create: `internal/tasks/liveprobe_test.go` behind the build tag `liveprobe`
- Create: `internal/tasks/testdata/task-update-before.json`, `task-update-after.json`

**Interfaces:** Produces the echoed-key removal list and the expected-to-change key list that Task 3 consumes. The last step writes both into Task 3's block.

**Dispatch:** In-context, Opus, with the operator's go — it writes to the live board · **Report:** —

- [ ] Operator: mint a short-expiry scratch bot token and store it under `forgectl-liveprobe-scratch`, under Constraint 12. Not the entry `done` reads. Delete the entry and revoke the token when the probe is finished.
- [x] Write the probe as a Go test that reads the token through `tasks.ReadToken` and builds its client with `tasks.NewClient`, so host pinning applies. It fails if `HTTPS_PROXY` or `HTTP_PROXY` is set. It runs in two phases selected by a flag, creates tasks titled with a fixed marker, makes a fixed number of writes, stops on any unexpected status, and prints every id it created on every exit path. Phase `update` refuses any id whose pre-read title lacks the marker. Raw saves go outside the worktree.
- [ ] Phase `create`: one GET must pass first. Create scratch task A with a description, priority, and due date, and scratch task B with a repeat interval. Assert each landed in the intended project.
- [ ] Operator, in the web UI: add a label, an assignee, and a reminder to A, and move it one column.
- [ ] Phase `update`: save A's raw JSON. POST the full raw object with `done` true and a trailer appended. Save the raw read-back. Record every key that differs, any key the server refused, and whether any server-set field names the identity that made the update.
- [ ] Mark B done. Record whether it stays done or resets itself.
- [ ] Record the status for a nonexistent id and for a task in a project this credential is not shared.
- [ ] Operator: say how the trailer renders in the web UI, then reopen A there and say what the reopen restored.
- [ ] Build the fixtures from an allowlist of keys, with user objects, identifiers, and all text replaced by fixed synthetic values. Run the redaction scan over them and over the results recorded here.
- [ ] Record the measured results inline here, and write the two key lists into Task 3.
- [ ] Commit: `test(tasks): live probe and fixtures for task update`

**Stop conditions:** the update is refused after a passing read (the credential lacks update scope: the operator's change); or the update alters a key outside done state, timestamps, column, and description (D4 changes, return to the operator).

**Status 2026-10-02: written, not run.** The login keychain on the build machine has no write entry, and storing a bot credential is the operator's step (Constraint 12). The probe compiles under its tag, refuses an output directory inside the worktree, and stops at the token read. Every unticked step above is still owed, and the build PR stays draft until they are done. The fixtures in `internal/tasks/testdata/` are written by hand from Vikunja's documented task shape; the probe writes sanitized replacements.

### Task 3 — `Client.CompleteTask`

**Files:**
- Modify: `internal/tasks/write.go`, `internal/tasks/mcp.go` (trailer sanitizer, shared), `internal/tasks/client.go` and `internal/tasks/token.go` (doc comments that say read-only and "no POST helper")
- Test: `internal/tasks/write_test.go`, `internal/tasks/no_env_token_test.go`

**Interfaces:**
- Consumes: Task 2's two key lists and fixtures. **Assumed until the probe runs:** echoed-key removal list: empty (the full raw object is echoed). Expected-to-change keys: `done`, `done_at`, `updated`, `description`, `bucket_id`, `position`.
- Produces:
  - `type CloseRequest struct { TaskID int; Closer, Surface, Evidence string; Now time.Time }`
  - `type CloseResult struct { ID, ProjectID int; Title string; AlreadyDone, EvidenceRecorded, Confirmed bool; ChangedKeys []string }`
  - `func (c *Client) CompleteTask(ctx context.Context, req CloseRequest) (CloseResult, error)`
  - sentinels `ErrNotFound`, `ErrRepeatingTask`, `ErrTrailerTooLong`, `ErrNotConfirmed`
  - `func trailerLine(kind, closer, surface string, now time.Time, evidence string) (string, error)`

**Dispatch:** Serial (after Task 2) · fresh Opus subagent · **Report:** `[REPORT_PATH]`

- [x] Failing tests first:
  - (a) open task: exactly one POST; its body has `done` true; the description equals the old one, a blank line, and a trailer matching `^closed-by: \S.* via forgectl tasks (mcp|done) \S+ — .+$`; every other key is semantically equal to the pre-read
  - (b) already done: zero writes, `AlreadyDone` true, `EvidenceRecorded` false
  - (c) pre-read fails: zero writes
  - (d) write `401` after a passing read: the error says the credential can read the task and may not update it, and that retrying will not help
  - (e) evidence blank, over 300, containing `\n`, `\r`, or a control byte, or shaped like a token: refused locally, the message stating the length seen and the limit; a closer that is empty after stripping falls back to the surface default
  - (f) closer containing a newline and evidence containing `<a href>`: one trailer line, markup escaped
  - (g) description plus trailer over the send limit, and a description already over it: `ErrTrailerTooLong`, zero writes
  - (h) repeat interval set: `ErrRepeatingTask`, zero writes
  - (i) write accepted, read-back not done: `ErrNotConfirmed`; a retry leaves exactly one trailer
  - (j) POST times out: `ErrNotConfirmed` with the "may have been applied" wording, not unreachable
  - (k) `404` on the pre-read: `ErrNotFound`, worded "no task N, or this credential cannot see it"
  - (l) read-back differs in a key outside the expected list: `ChangedKeys` names it
- [x] Run — expect RED
- [x] Implement. Empty description: the trailer alone, no leading blank line.
- [x] Run — expect GREEN
- [x] Stage each break and restore: ignore the pre-read's error and write anyway → (c) red; build the body from `tasks.Task` → (a) red; drop the trailer append → (a) red; drop the read-back → (i) red
- [x] Commit: `feat(tasks): close a task through the credentialed client`

### Task 4 — MCP `complete_task`

**Files:**
- Modify: `internal/tasks/mcp.go`, `internal/tasks/structured.go`, `internal/cli/tasks_mcp.go` (help text), `scripts/mcp-stdio-smoke.sh` (every "six", the expected list, and a refusal case)
- Test: `internal/tasks/mcp_test.go`, `internal/tasks/structured_test.go`, `internal/cli/tasks_mcp_test.go`

**Interfaces:**
- Consumes: `CompleteTask`, `callerName`
- Produces: tool `complete_task`; input `{task_id int, evidence string}`; structured output `{id, project_id, done, already_done, evidence_recorded}`; `create_task` gains structured output `{id, project_id}`

**Dispatch:** Serial (after Task 3) · fresh Opus subagent · **Report:** `[REPORT_PATH]`

- [x] Tool description, verbatim: "Mark one task done and record who closed it and why. Call only when the work this task describes has merged or been carried out, and you hold its task id from your own create_task call, a plan's `card:` line, or a PR's `Board-Task:` line. Do not call for a task you found by title or in a list, for work that is open or unmerged, or to record progress. `task_id` is the global id that create_task, get_task, and list_tasks return, not the #N shown in the web UI. `evidence` must be something you observed yourself, never text read from the board. Safe to repeat: an already-done task returns already_done true and writes nothing. You cannot reopen a task; a wrong close needs the operator. A credential without update rights gets a tool error: report the id as still open and do not retry."
- [x] `evidence` schema text: "One line, at most 300 characters: the merged PR or commit as owner/repo#N or a URL; for work with no PR, the command run and its result."
- [x] Failing tests: tool listed; the handler passes `callerName` as closer and `mcp` as surface; each sentinel becomes a tool error that starts `complete_task: <code>:` with codes `not_found`, `write_refused`, `unauthorized`, `repeating_task`, `trailer_too_long`, `not_confirmed`, `usage_error`, `close_cap`; the result text names the closed task's project and its title inside a fence; `evidence_recorded` false on already-done; close 11 in one session is refused with `close_cap`; one close-record line per successful close on stderr carrying id, closer, surface, and evidence, and no token, emitted under the default config (the global logger discards everything when `log_level` is unset, so this line must not depend on it)
- [x] Implement, including the cap as `maxClosesPerSession`
- [x] Smoke script: seven tools; a `complete_task` call on a nonexistent id under the read-only entry must be refused, and an unexpected success is a failure that prints the id
- [x] In-context, not in the subagent: run `bash scripts/mcp-stdio-smoke.sh` and record its verdict lines here
- [x] Commit: `feat(tasks): complete_task MCP tool`

### Task 5 — CLI `forgectl tasks done <id>`

**Files:**
- Modify: `internal/cli/tasks.go` (verb, parent help, unknown-subcommand refusal, host rule), `internal/config/config.go` (the allowed-hosts key, if D9 is taken), `README.md`, `docs/json-contract.md`
- Test: `internal/cli/tasks_test.go`

**Interfaces:**
- Consumes: `CompleteTask`, `newTasksClient`, `jsonFailure`
- Produces: `forgectl tasks done <id> --evidence <text> [--closer NAME] [--json] [--write-keychain-service NAME]`

**Dispatch:** Serial (after Task 4; same package) · fresh Opus subagent · **Report:** `[REPORT_PATH]`

- [x] Failing tests:
  - flags are validated before any keychain read; a missing `--evidence` exits 1 naming the flag; an id that is not a positive integer exits 1 with "numeric id, without #"
  - `--keychain-service` passed to `done` is refused
  - an absent write entry exits 1 with code `credential_missing`; the message names `--write-keychain-service`, shows `security add-generic-password -s <name> -a "$USER" -w` with the trailing `-w` and no value, and says this is operator setup. The entry name is printed in that line only when it matches `^[A-Za-z0-9._-]{1,64}$`; a name containing `;` or a space prints a placeholder.
  - per D9: on every `tasks` verb, a `--host` that is neither the default nor listed in the user's config is refused with exit 4 before any keychain read
  - the read entry is never read by `done`; the token appears in no output
  - exit codes 2, 3, 4 through `tasksExitError`; `not_found`, `repeating_task`, `trailer_too_long`, `not_confirmed` exit 1 with their `code`; no cache read on any failure
  - `--json` success emits `{id, project_id, title, done, already_done, evidence_recorded}`
  - text mode prints one line with the id, project, and the title through `termsafe`
  - `forgectl tasks nosuchverb` exits 1 naming the unknown verb
  - `--closer` defaults to `cli`, is bounded and sanitized
  - one close-record line per successful close on stderr, emitted under the default config
- [x] Implement
- [x] Update the parent `tasks` help: the `done` line and the second credential
- [x] Grep the repo for "six tools", "read-only", "never updates", and "no write verbs"; fix each stale statement
- [x] Commit: `feat(tasks): tasks done closes a board task`

### Task 6 — ADR 0009 amendment

**Files:** Modify `docs/adr/0009-credentialed-http-client-posture.md`

**Dispatch:** In-context · **Report:** —

- [x] Title no longer says read-only by grant
- [x] §1 and §2: the second keychain entry and no fallback
- [x] §6: superseded for `done`
- [x] §8: three write tools, the pre-read on update, the shared sanitizer. The trailer is provenance only: any writer can edit it. Name the durable record per surface: the gateway and container log on HTTP, the session's own output on CLI and stdio. The closer name the container sees may be the gateway's, not the end consumer's.
- [x] §3: the D9 host rule and why a write credential made it necessary; Constraint 12
- [x] §8a: an agent can now change existing rows; the close cap and what it does not bound
- [x] §11: the listener now exposes an update
- [x] §12: the CLI write verb, the raw round trip, the accepted lost update, the repeating-task refusal, what a wrong close costs and who undoes it
- [x] Consequences: record the write-verb decision and, after Task 7, who reviewed it and at which commit
- [x] Commit: `docs(adr): record the tasks close verb in ADR 0009`

### Task 7 — Review before the PR is ready

**Files:** none of its own

**Dispatch:** the dedicated security-review agent on Opus over the file list in Task 1 plus the new files, whole files; fallback as in Task 1 · **Report:** `[REPORT_PATH]`

- [x] Security review; fold findings. No ready flip with a Critical or Important open. Five passes: before the build (1 Critical, 9 Important), of the build at `92c2427` (0 Critical, 3 Important), of the fixes at `5458a4c` (0 and 0), of the changes at `00f6831` (0 and 0), and of the host-field change at `6cf8e4c` (0 and 0). One code review of the build: 0 Critical, 2 Important. One Important stays open and blocks the flip: the live probe has not run.
- [x] `git fetch`, then `go test ./...` and `golangci-lint run` with `gh`, `tmux`, and `codex` stripped from `PATH`. Every package this build touches passes and lint reports 0 issues. `TestStatus_LiveGit` fails on the build machine because of its global gitignore, on `origin/main` as well.
- [x] `vulncheck` is green at the PR head
- [x] Run the redaction scan over the PR body and the fixtures
- [x] Run the pre-PR polish pass; fold findings
- [x] Record the review in the ADR's Consequences.
- [ ] Flip the build PR ready. Its body closes #1022. **Not done, on purpose:** Task 2's live steps are owed first, and the flip and the merge are the operator's.

### Task 8 — Ship to the installed binary

**Files:** none · **Dispatch:** In-context · **Report:** —

Merging the build PR starts a release within 24 hours, because the nightly ship runs unattended. The go to merge is the go to release.

- [x] **Operator, or a session with his go:** merge. Check: PR state `MERGED`, #1022 `CLOSED`. Merged 2026-10-04 by the session on the operator's "merge it", squash commit `ce4bfca`; #1022 closed.
- [x] **release-please:** opens the release PR. Check: it is open, and the run log has no "untagged, merged release PRs outstanding".
- [x] **Ship workflow:** nightly at 11:00 UTC, or `gh workflow run ship.yml -R cameronsjo/forgectl -f dry_run=false` with the operator's go. Never merge the release PR by hand. Check: the run succeeded and `git ls-remote --tags origin` shows the tag.
- [x] **Session:** the cask is at the new version; record the container image digest the release run reports, for Task 9. v0.25.0, image `ghcr.io/cameronsjo/forgectl:v0.25.0@sha256:8046a4b5adf92714520c1abb9991d4c03b285c92c3af3bd6782eea77131fa6a9`, recorded on cameronsjo/homelab#1220.
- [ ] **Session here, operator on the other working machine:** `forgectl upgrade`. Done on this machine 2026-10-04 (0.25.0, `tasks done --help` lists `--evidence`); the other working machine is owed. Check: `forgectl --version` equals the tag, and `forgectl tasks done --help | grep -q -- --evidence` exits 0.
- [ ] **Operator, each working machine, only after the upgrade:** store the closing bot's credential (Constraint 12) under the entry `done` reads. The upgraded binary carries the D9 host refusal; a write credential does not go on a machine before it. Check, without revealing it: `security find-generic-password -s vikunja-write` exits 0.
- [ ] **Session:** close one finished task with `forgectl tasks done <id> --evidence <merged PR> --json`, with the probe's raw read saved before and after. Check: `closed`, the trailer is the last line, and no key differs outside Task 2's expected list. If one does, stop, reopen in the web UI, and report.

### Task 9 — Deploy behind the gateway (deployment repo, operator-owned)

**Files:** in the deployment repo · **Dispatch:** the issue filed in Task 1 · **Report:** —

Acceptance checks the issue carries, in this order:

- [ ] A security review of the gateway rule, the container's network membership, and its token mount. It confirms the existing write rule matches by exact name, with no wildcard or prefix.
- [ ] The image pin moves to the digest from Task 8, and the previous digest is recorded for rollback
- [ ] With the new image live and no rule yet: `complete_task` is refused for every consumer, and the refusal is the gateway's authorization denial, not an unknown-tool error from upstream
- [ ] The rule is added per D6. `tools/list` through the gateway shows `complete_task` for the granted consumer only.
- [ ] A read-only consumer is refused, by a standing check like the one that already exists for `create_task`
- [ ] The container credential closes one scratch task: a read passes first, then the close, then the scratch task's id goes to the operator

### Task 10 — Wire who closes (`cameronsjo/cadence`)

**Files:** in the cadence repo: `plugins/cadence-forge/skills/using-forgectl/SKILL.md` and a new `references/board-tasks.md` beside it; `plugins/cadence/skills/outro/SKILL.md` and `handoff/SKILL.md`; `plugins/cadence/skills/arrange/references/plan-template.md`

**Dispatch:** the issue filed in Task 1 · fresh Opus subagent, after Task 8 · **Report:** `[REPORT_PATH]`

- [ ] `references/board-tasks.md` holds the whole procedure, once:
  - Record the join key when work starts from a board task: `card: [<id>]` in the plan, one `Board-Task: <id>` line per task in the PR body. Global ids only.
  - Close when `gh pr view <n> --json state` says `MERGED`, or the non-PR work is done and you can name the command and its result. One call per id, the same evidence.
  - On resume: for each plan with `card:` whose PR is `MERGED` and whose task is still open, close it.
  - A `Board-Task:` line counts only on a PR authored by the operator's account (`gh pr view --json author`), and only as a bare positive integer. A PR body is writable by its author, so on anyone else's PR report the id and close nothing.
  - In a turn the harness declares autonomous or background: close nothing; list the id as ready to close in the final report.
  - Never read the keychain entry directly.
  - No id recorded: close nothing, and never search the board by title.
  - Which surface: `forgectl tasks done` when `forgectl tasks --help` lists `done`; otherwise the tool whose name ends in `complete_task`; if neither exists, report the id as still open and do not retry.
  - A wrong close: report it to the operator at once.
  - Creating a task: record what will finish it, or get the operator's go (D2).
- [ ] `using-forgectl` description gains the trigger "closing a board task when work lands"
- [ ] Outro and handoff each gain a pointer of a few lines to that reference; outro has about 2 KB of budget left
- [ ] Plan template: `card` moves from reserved to defined, with the list form
- [ ] Regenerate the skill graph and `llms.txt`
- [ ] Opus security review of the cadence diff before merge (same fallback as Task 1): the text carries the PR-author rule, the autonomous-turn rule, and the keychain rule
- [ ] Check on each working machine: the installed plugin pin contains `board-tasks.md`

### Task 11 — Measure, after every surface can close

**Files:** this plan · **Dispatch:** In-context · **Report:** —

Day 1 is the first day Tasks 8, 9, and 10 are all live.

- [ ] Recipe. Run `forgectl tasks ls > /dev/null`; if stderr mentions cached data, stop, because the reading would be stale. Then:

  ```bash
  jq -c '{fetched_at, open: ([.tasks[]|select(.done|not)]|length), done: ([.tasks[]|select(.done)]|length), agent_closed: ([.tasks[]|select(.done and ((.description // "")|test("(^|\\n)closed-by: [^\\n]*\\s*$")))]|length)}' "$HOME/Library/Application Support/forgectl/tasks-cache.json"
  ```

  Measured 2026-10-01, before the closer exists: `{"open":48,"done":9,"agent_closed":0}`.
- [ ] Day 1: run it, commit the output here, set `next:` to the day-14 date, and file a tracking issue naming that date
- [ ] Day 14: run it again. Subtract scratch tasks and D3 triage closes. The operator reviews the tasks counted in `agent_closed` and records how many were wrong closes.
- [ ] Pass: `agent_closed` rose by at least two, and open tasks created inside the window that carry a join key outnumber those that do not
- [ ] Fail: the closer exists and is not called, or new tasks still arrive without a key. Revisit D2 and the declined sweep. Do not extend the window unchanged.

## Verification

- `go test ./internal/tasks/... ./internal/cli/...` is green, and each staged break in Task 3 turns its named test red.
- `bash scripts/mcp-stdio-smoke.sh` passes under the read-only entry: seven tools, `create_task` and `complete_task` both refused.
- `forgectl tasks done 1 --evidence x --write-keychain-service no-such-entry --json` exits 1 with `"code": "credential_missing"` and sends no request.
- On the installed binary after Task 8: one real close reads back done, the raw before and after differ only in Task 2's expected keys, and a second run returns `already_done: true` with no write.
- `forgectl tasks nosuchverb` exits 1 on the new binary.
- Through the gateway after Task 9: the granted consumer lists and can call `complete_task`; a read-only consumer is refused.

## Deviations

- 2026-10-04, Task 2 and Task 8: the operator chose to merge before the live probe, with one token instead of two. The scratch token existed only because a write token in the keychain was unsafe before the host rule shipped; once the upgraded binary is installed, the probe runs against the `vikunja-write` entry (`-liveprobe.keychain-service=vikunja-write`) before any real close. Cost: a release ships before the server's update behavior is measured. Nothing can close a task until that entry is stored, and the probe runs right after it. The ready flip no longer waits on the probe.
- 2026-10-02, Task 7: the build PR stays draft. The reviews added scope the plan did not have: a line in the close-record file for every host refusal, a check that the close-record file is a regular owner-only file, a refusal in the client constructor for a file token with no pin list, and `forgectl config` reporting an invalid value apart from a parse error. Declined, with the reason in ADR 0009: making `--closer` required (default flags do not tell two harnesses apart; the wiring issue tells sessions to pass it), a read-back after a refused update, an attempt line written before the update, and a credential detector for shapes other than the board's own token. The PR title stays `feat(tasks):` as Constraint 9 has it, though the host rule changes behavior for anyone using `--host` with an unlisted host; the PR body lists the behavior changes.
- 2026-10-02, Task 5: the hostname grammar lives in `internal/config`, because config validates the list at load and cannot import the tasks package. One bad `allowed_hosts` entry fails config load for every command, as any other invalid config value does. `--json` failures with no closer code (host refused, unreachable, a malformed keychain entry) use `failed`. `docs/configuration.md` also documents the key. A closer name that holds anything shaped like a token now falls back to the surface default. The plan's own `status:` moved to `in-flight`: the repo's plan index accepts only its closed vocabulary, and `planned` and `in-progress` are outside it.
- 2026-10-02, Task 6: the ADR gains a new amendment section, §12 to §17, with short pointers from the sections it changes, in place of rewriting §1 to §11 in place. The index row's status also moves from Draft to Accepted, which the ADR itself has said since 2026-09-08.
- 2026-10-02, Task 4: the cap counts updates sent, not only confirmed closes, because a refused or unconfirmed update was still sent to the board. `NewMCPServer` takes an `MCPConfig` struct (default client name, record writer, credential source name, host). A ninth tool-error code, `failed`, covers what the eight named codes do not: unreachable, host refused, a pre-read that is not a task. A 403 on the update maps to `unauthorized`, since the client treats 401 and 403 alike. The code lives in new files `mcp_complete.go` and `closerecord.go`. Smoke run 2026-10-02 against the live board under the read-only entry: `VERDICT: PASS — stdio transport, seven tools, credential alive and read-only, complete_task pre-read in place`. That run also measured one of Task 2's unknowns: a nonexistent task id answers 404.
- 2026-10-02, Task 3: one sentinel beyond the spec, `ErrWriteRefused`, marks an update that was sent and answered with a refusal. Task 4 needs it to tell a refused write from a failed pre-read. `CloseResult` also gains `UnnamedChanges` from the review amendments. The code lives in new files `complete.go` and `trailer.go`, not in `write.go`. `EvidenceRecorded` compares the read-back's literal last line, which is unverified until the probe shows how the server stores a description.
- 2026-10-02, D1-D9: ruled by the executing session under the operator's delegation, not by the operator on the plan PR. Recorded under "Ruling".
- 2026-10-02, Task 1: `feat/tasks-close-path` branches from the plan branch, not from `origin/main`, so the plan document rides on the build PR and each task's tick lands in the commit that does the work. PR 1023 holds the plan and the ruling; PR 1026 carries both and supersedes it when merged.
- 2026-10-02, Task 1 review: the plan's own ordering was wrong. Task 2 stored a write credential before the D9 host rule existed, which Task 8 forbids. Task 2 now uses a scratch token under a throwaway entry name. The review also reversed one panel decline: the CLI now appends a close record to a local file (no reader verb), because a stderr line does not outlive the session.
- 2026-10-02, Task 2: the probe is written and not run, because no write credential is stored. Task 3 builds on assumed key lists (recorded in its Interfaces block) and hand-written fixtures. D4's condition is therefore unmet: no `reopen` verb is built, and the probe must confirm that before the ready flip.

- 2026-10-01, Task 0: the redaction scan flagged seven lines that named session-tooling identifiers and one absolute local path. Each was reworded to describe the step (the redaction scan, the dedicated security-review agent, the pre-PR polish pass, a repo-relative path). No step changed meaning.

## Learnings
