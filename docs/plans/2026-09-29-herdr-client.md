---
status: in-flight
next: "Tasks 0-4 done; review and merge cameronsjo/forgectl#723, then roadmap item 2 (shared readiness checks with forgectl#536 T2) and item 3 (inbox)"
branch: plan/herdr-roadmap
pr: cameronsjo/forgectl#721
updated: 2026-09-29
approved_session_id: a86d73e4-2c61-43bb-a377-9455a4c62f53
date: 2026-09-29
session_id: a86d73e4-2c61-43bb-a377-9455a4c62f53
model: claude-sonnet-5-5
harness: claude-code 2.1.285
machine: cf6e768835c7
---

# internal/herdr: the shared herdr client (roadmap item 1)

Implementation ships on its own branch and PR (`feat/herdr-client`), created after approval. This branch carries the plan only.

## Goal

A small, tested `internal/herdr` package that reads and mutates the live herdr session it runs in through the `herdr` CLI, so `inbox`, `jump`, `organize`, and the notifier share one error model, one set of fixtures, and one fork-capability check. No command ships in this plan.

## Alternatives declined

- **Reuse `internal/surface/herdradapter` as the client** — it starts and closes surfaces on a pinned server through the bounded sensitive runner (`exec.BoundedOutput`); the read/move/focus commands act on the session they run in and use plain `exec.Runner`.
- **Move the adapter's `errorCode` and `parseWorkspaceList` into the client now** — they take `exec.BoundedOutput`; changing the adapter is out of scope. Task 4 files a follow-up to converge them.
- **Ship the client with its first command** — the roadmap keeps it separate so forgectl#536 can agree the surface before either lands.
- **A `FocusPane(id)` method** — herdr has no focus-pane-by-id verb (`pane focus` is directional only); `FocusTab` is the finest grain.

## Panel

Panel: 3 seats ran (2 plan-reviewer lenses, red-team-reviewer) — 35 findings, 34 folded in, 1 declined (see Panel review — findings declined)

## Panel review — findings declined

- **Task 3 dispatch "serial, could be parallel with Task 2"** — Task 3 has no data dependency on Task 2, but both edit the same package and order costs nothing; left serial.

## Architecture

`internal/herdr` has three pieces and no CLI code:

1. **`Client`** wraps an `exec.Runner` and targets the session the process runs in (herdr selects the server from the inherited `HERDR_SOCKET_PATH`). A caller that needs a pinned server, such as forgectl#536's workers, passes a `Runner` whose calls set `HERDR_SOCKET_PATH` (`RunWithEnv`); the client itself has no pin option. `New` performs no gate; running `Probe` first is the caller's duty.
   - JSON reads (`Workspaces`, `Tabs`, `Panes`, `Agents`, `PaneGet`, `TabGet`) decode the `{"id","result"}` envelope, ignore `id`, tolerate unknown fields, and fail closed on a missing or null `result`. An empty list is `[]`, not an error.
   - `ReadPane` is the exception: `herdr pane read` prints raw terminal text on success (measured), so `ReadPane` returns stdout as text and passes `--format text`. Only its failure path carries the JSON error envelope.
2. **`Error`** is a typed error carrying herdr's `code` and `message`. herdr fails with exit 1, empty stdout, and `{"error":{"code","message"},"id":...}` on stderr (measured), which `exec.Runner` surfaces as `*exec.CommandError` with `Stderr` set and an empty returned string. The client reads `CommandError.Stderr`; when stderr is truncated (`StderrDropped > 0`), carries log lines before the JSON, or is not valid JSON, it returns the wrapped `*exec.CommandError`, never a zero-value `*Error`. A mutation whose `move_result.changed` is `false` (herdr exits 0) returns `*Declined{Reason}` as the error.
3. **`Probe`** checks the session gate and the fork-only capability. It checks the CLI only, not the running server: a fork CLI talking to a server still running an older binary passes the probe and fails at `MoveTab`, so `MoveTab` maps an unknown-method server error to the same "needs the fork" message.

The package does not import `internal/config` or `internal/cli`. Tab and pane ids renumber when a tab moves between workspaces, and it is unverified whether freed ids are reused, so callers re-list after every mutation and resolve a target by `terminal_id` immediately before moving it. `terminal_id` is stable across moves and is the join key.

### Client surface (the contract posted to forgectl#536)

```go
func New(r exec.Runner) *Client
func (c *Client) Workspaces(ctx context.Context) ([]Workspace, error)
func (c *Client) Tabs(ctx context.Context, workspaceID string) ([]Tab, error)
func (c *Client) Panes(ctx context.Context) ([]Pane, error)   // all panes in the session
func (c *Client) Agents(ctx context.Context) ([]Agent, error)
func (c *Client) PaneGet(ctx context.Context, paneID string) (Pane, error)
func (c *Client) TabGet(ctx context.Context, tabID string) (Tab, error)
func (c *Client) ReadPane(ctx context.Context, paneID string, src ReadSource, lines int) (string, error)
func (c *Client) MoveTab(ctx context.Context, tabID string, to MoveTarget) (MoveResult, error)
func (c *Client) MoveWorkspace(ctx context.Context, workspaceID string, index int) error
func (c *Client) FocusWorkspace(ctx context.Context, workspaceID string) error
func (c *Client) FocusTab(ctx context.Context, tabID string) error
func Probe(ctx context.Context, r exec.Runner, lookupEnv func(string) (string, bool)) error
```

`MoveTarget` is one of: existing workspace id, new workspace with a label, or an index in the current workspace. `MoveResult{TabID, WorkspaceID string}` carries the post-move ids on success. A decline returns a zero `MoveResult` and `*Declined{Reason}`. `ReadSource` is `visible | recent | recent-unwrapped | detection`.

Readiness checks (per-harness screen predicates, kept as TOML) are proposed to live in `internal/herdr/ready`, built by forgectl#536 T2. That package name is this plan's proposal; #536 decides.

## Tech Stack

Go per `go.mod`, `internal/exec` (`Runner`, `FakeRunner`, `CommandError`). No new dependencies.

## Global Constraints

- **Measured facts (live herdr 0.9.1 fork build, this session):**
  - a failing call, `herdr tab list --workspace wNOPE`: exit 1, stdout empty, stderr `{"error":{"code":"workspace_not_found","message":"workspace wNOPE not found"},"id":"cli:tab:list"}`
  - `herdr tab move --help`: exit 2, stdout empty, stderr first line `usage: herdr tab move <tab_id> --index N`; `herdr tab --help` (exit 0) does not list `move`
  - `herdr pane read <id> --source recent --lines 2`: exit 0, raw text on stdout
  - `HERDR_ENV=1` and `HERDR_SOCKET_PATH` are exported in herdr panes
  - `herdr pane focus` requires `--direction`; there is no focus-pane-by-id verb
- **Session gate:** `Probe` requires `HERDR_ENV=1` and a non-empty `HERDR_SOCKET_PATH` naming an existing socket. `HERDR_ENV` alone is inherited into nested shells and detached jobs and does not identify the server.
- **Fork dependency:** `tab move` is in `cameronsjo/herdr`, not upstream. Stock herdr's output for `tab move --help` is unmeasured (no stock binary available); the probe therefore keys on the fork's exact usage prefix and treats anything else as missing.
- **Fixtures:** capture read-only from a live session, then sanitize with an allowlist: rewrite every string field except the enum fields (`type`, `agent_status`, `agent`, `kind`, `source`). `terminal_title`, `terminal_title_stripped`, `label`, `cwd`, `foreground_cwd`, and `agent_session.value` carry live content and are rewritten. Map ids per component, including the workspace prefix inside composite `tab_id` and `pane_id`.
- **Changelog:** do not edit `CHANGELOG.md` (release-please-owned; `scripts/check-changelog-owner.sh` guards it). The `feat(herdr):` commit subject is the record.
- Conventional commits; merge, not rebase; a polish pass before the PR.

## Orchestrator

**Driver:** Sonnet — spec'd, no security-posture trigger.

---

## Tasks

### Task 0 — agree the surface with forgectl#536

**Files:** none

**Interfaces:**
- Consumes: the client surface block above
- Produces: an accepted package name and surface, recorded as a comment on forgectl#536

**Dispatch:** In-context · Sonnet — needs Cameron's confirmation, since #536 is Cameron's own draft

**Report:** —

**Steps:**
- [x] Post the client surface block, the launch boundary (launch stays in `herdradapter`), and the proposed `internal/herdr/ready` package on forgectl#536, scanned with `cadence-hooks cadence redact-scan --audience public` first
- [x] Gate: Cameron confirms the surface or asks for changes. Do not start Task 1 before that

### Task 1 — read client, error model, fixtures

**Files:**
- Create: `internal/herdr/client.go`, `internal/herdr/errors.go`, `internal/herdr/doc.go`
- Test: `internal/herdr/client_test.go`, `internal/herdr/errors_test.go`, `internal/herdr/testdata/*.json`, `internal/herdr/testdata/pane_read.txt`

**Interfaces:**
- Consumes: `exec.Runner`, `*exec.CommandError`
- Produces: `Client`, `New`, the read methods and `ReadPane`, `*Error{Code, Message}`

**Dispatch:** In-context · Sonnet — defines the interfaces everything else consumes

**Report:** —

**Steps:**
- [x] Capture read-only fixtures: `workspace list`, `pane list`, `tab list --workspace <id>`, `agent list`, `pane get`, `tab get`, one `pane read` (text), and one failing call for the stderr envelope. Sanitize with the allowlist; add a fixture-join test on `workspace_id`, `tab_id`, and `terminal_id`, and a scan that greps `testdata/` for `$HOME`, the hostname, and the live session id and expects nothing
- [x] Write failing tests: each read decodes its fixture; an empty list, a null `cwd`, a null `agent`, unknown extra fields, and a missing or null `result` (fail closed); `ReadPane` returns text and passes `--format text`; `*exec.CommandError` with the stderr envelope becomes `*Error{Code:"workspace_not_found"}`; truncated stderr, log lines before the JSON, and non-JSON stderr each return the wrapped `*exec.CommandError`
- [x] Run `go test ./internal/herdr/...` — expect RED
- [x] Implement; run — expect GREEN
- [x] Mutation sweep: delete the envelope parse; `TestErrorEnvelopeBecomesTypedError` must go red; restore
- [x] Commit: `feat(herdr): read client and typed errors`

### Task 2 — mutations and declined moves

**Files:**
- Modify: `internal/herdr/client.go`
- Test: `internal/herdr/mutate_test.go`, `internal/herdr/testdata/*.json`

**Interfaces:**
- Consumes: Task 1
- Produces: `MoveTab`, `MoveWorkspace`, `FocusWorkspace`, `FocusTab`, `MoveTarget`, `MoveResult`, `*Declined`

**Dispatch:** Serial (after Task 1) · Sonnet — extends the same file

**Report:** —

**Steps:**
- [x] Response shapes for `tab move`, `workspace move`, and the focus verbs change the live session and cannot be captured read-only. Ask Cameron to run one `tab move` in a throwaway workspace and capture the result, including a sole-tab move to see the decline; until then the fixture is synthetic and cites a fork commit SHA plus file path for the shape (not a search-index hit)
- [x] Failing tests: `changed:false` with `reason:last_tab_in_workspace` returns `*Declined`; `MoveTab` returns the post-move `TabID` and `WorkspaceID` from the response, not the input ids; `--new-workspace --label` returns the new workspace id; each `MoveTarget` builds the expected argv; `FocusTab` argv is `tab focus <id>` (no directional fallback); an unknown-method server error maps to the "needs the fork" message
- [x] Run — expect RED; implement; run — expect GREEN
- [x] Mutation sweep: delete the `changed` check; `TestMoveTabDeclinedReturnsDeclined` must go red; restore
- [x] Commit: `feat(herdr): mutations and declined-move handling`

### Task 3 — capability probe and docs

**Files:**
- Create: `internal/herdr/probe.go`, `docs/herdr.md`
- Test: `internal/herdr/probe_test.go`

**Interfaces:**
- Consumes: `exec.Runner`, `lookupEnv func(string) (string, bool)`
- Produces: `Probe`; `docs/herdr.md` documenting the fork requirement, the CLI-only limit of the probe, and the id-renumbering caveat

**Dispatch:** Serial (after Task 2) · Sonnet — small, standalone

**Report:** —

**Steps:**
- [x] The measurement is done (see Global Constraints): `herdr tab move --help` exits 2 with `usage: herdr tab move <tab_id> --index N` on stderr, stdout empty. Do not re-derive it from the tests; cite it in the test fixture
- [x] Failing tests: a real-shaped `*exec.CommandError{ExitCode: 2, Stderr: "usage: herdr tab move <tab_id> ..."}` -> ok (usage text is on `CommandError.Stderr`/`Output`, never the returned string); exit 0 with that prefix on stdout -> ok (clap-style help); `tab frobnicate --help` output with the fork's command list -> rejected (does not begin with the exact prefix); a stock-style listing without `move` -> error saying the `tab move` verb needs the `cameronsjo/herdr` fork; `HERDR_ENV` unset, empty, or not `1` -> error naming the variable, and the gate error wins over the fork error; `HERDR_SOCKET_PATH` unset or not an existing socket -> error naming it
- [x] Run — expect RED; implement; run — expect GREEN
- [x] Commit: `feat(herdr): session and fork-capability probe`

### Task 4 — follow-ups and polish

**Files:** none

**Interfaces:**
- Consumes: Tasks 1-3
- Produces: a follow-up issue

**Dispatch:** In-context · Sonnet — needs judgment

**Report:** —

**Steps:**
- [x] File a follow-up issue to converge `herdradapter`'s `errorCode` and `parseWorkspaceList` onto the client
- [x] run a pre-PR polish pass over the diff (review, simplify, repo formatter and linter); fold findings (run from the implementation worktree)

---

## Deviations

- **2026-09-29 — Task 2: move fixtures are real captures, not synthetic.** The plan expected a maintainer to capture one `tab move` in a throwaway workspace. The build did it: a workspace created for the purpose, moves exercised inside it, only that workspace closed, layout and focus verified unchanged. Reality-forced improvement.
- **2026-09-29 — Task 2: `MoveResult` has no `Changed`/`Reason`; an index move has no `move_result`.** The measured reply to `tab move --index` is the workspace's tab list with no `move_result`. `MoveTab` derives the workspace from that list and fails closed if the tab is absent. Reality-forced.
- **2026-09-29 — Task 2: the unknown-method to `ErrForkRequired` mapping is dropped.** herdr's code for a missing verb cannot be measured without a stock binary, so `MoveTab` returns the typed `*Error` with whatever code herdr sends; `docs/herdr.md` says so. Reality-forced.
- **2026-09-29 — Task 3: the probe's discriminator is a line-start `usage: herdr tab move` match.** Measured: the fork's reply to an unknown subcommand also lists `herdr tab move` verbs, so exit 2 alone proves nothing. Reality-forced.
- **2026-09-29 — Tasks 1-2: pre-PR review changes.** `MoveTab` fails closed on four more reply shapes; a list reply missing its list key is an error; an error envelope counts only from exit status 1; `*Error` unwraps to the `CommandError`; control characters are stripped from error text (security Nit). Chosen improvements, found by review.
- **2026-09-29 — Task 4: docs arm of the pre-PR review skipped.** The only docs are new, for a package with no consumers. Recorded in the polish marker as skipped.
- **2026-09-29 — Implementation ships in cameronsjo/forgectl#723**, a separate PR from this plan's branch, as the plan's header said.

## Learnings

- **herdr renumbers on a cross-workspace move.** `MoveResult.TabID` is the id after the move (measured `w7D:t17` to `w7D:t19`); `terminal_id` is the stable key, and only `Panes()` and `Agents()` carry it.
- **`exec.OSRunner` logs any non-zero exit at `ERROR`,** including the probe's expected exit 2.
- **The polish conformance canary miscounts under this repo's lint config:** it splits every output line on `:`, so source and caret lines inflate the file count. Fixing the three real lint issues made it pass. Worth a fix in the polish script.
