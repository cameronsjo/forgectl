## 1. Backend contract

- [x] 1.1 `backend.KindHc`, sources `hc-default-session` and `hc-named-session`, `HcIdentity{terminal, created_ms}`, the `FingerprintHcCore` server id, and the reference wire fields.
- [x] 1.2 Sensitive-seam command kinds for hc readiness, create, reconcile, probe, read, send, and cleanup.

## 2. Adapter (`internal/surface/hcadapter`)

- [x] 2.1 Start, with readiness, one reconciling listing after an ambiguous create, and the failure classes (`name_taken` is a name collision).
- [x] 2.2 Probe and Close under the ownership rules in `design.md`; close kills then removes.
- [x] 2.3 WorkerScreen (status from `hc get`, text from `hc read`), SendBrief, TerminalID, Session.
- [x] 2.4 Fake-runner tests for each outcome, including a label or `created_ms` mismatch and a changed core id.

## 3. CLI

- [x] 3.1 `--surface hc` for `surface launch` and `--worktree`; hc name check before anything is created.
- [x] 3.2 `--surface herdr|hc` on `list`, `read`, `ready`, `wait`, `brief`, `close`; `tag` refuses hc.
- [x] 3.3 Status-plus-screen readiness and a send-based brief for hc workers.
- [x] 3.4 The worker terminal's own `HC_TERMINAL_ID` and `HC_SESSION` through the pane-identity path.

## 3b. Launcher permission mode

- [x] 3b.1 `--inherit-mode` on worker launches; `LauncherMode` on the invocation request; ceiling-only rule in `applyWorkerProfile`; tests for every source, including that a claim never raises.

## 4. Docs and checks

- [x] 4.1 `docs/herdr.md`, `README.md`, the shipped skill, command help.
- [x] 4.2 `go build ./...`, `go vet ./...`, `go test ./...`, golangci-lint, `scripts/openspec.sh validate --all --strict --no-interactive`.
- [x] 4.3 Live smoke against a scratch `HC_SESSION`: launch a throwaway worker, read it, close it.
