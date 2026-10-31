## 1. Backend contract

- [ ] 1.1 `backend.KindHc`, sources `hc-default-session` and `hc-named-session`, `HcIdentity{terminal, created_ms}`, the `FingerprintCore` server id, and the reference wire fields.
- [ ] 1.2 Sensitive-seam command kinds for hc readiness, create, reconcile, probe, read, send, and cleanup.

## 2. Adapter (`internal/surface/hcadapter`)

- [ ] 2.1 Start, with readiness, one reconciling listing after an ambiguous create, and the failure classes (`name_taken` is a name collision).
- [ ] 2.2 Probe and Close under the ownership rules in `design.md`; close kills then removes.
- [ ] 2.3 WorkerScreen (status from `hc get`, text from `hc read`), SendBrief, TerminalID, Session.
- [ ] 2.4 Fake-runner tests for each outcome, including a label or `created_ms` mismatch and a changed core id.

## 3. CLI

- [ ] 3.1 `--surface hc` for `surface launch` and `--worktree`; hc name check before anything is created.
- [ ] 3.2 `--surface herdr|hc` on `list`, `read`, `ready`, `wait`, `brief`, `close`, `status`, `usage`; `tag` refuses hc.
- [ ] 3.3 Status-only readiness and a send-based brief for hc workers.
- [ ] 3.4 `HC_SESSION` in the worker allowlist; `HC_TERMINAL_ID` through the pane-identity path.

## 4. Docs and checks

- [ ] 4.1 `docs/herdr.md`, `README.md`, the shipped skill, command help.
- [ ] 4.2 `go build ./...`, `go vet ./...`, `go test ./...`, golangci-lint, `scripts/openspec.sh validate --all --strict --no-interactive`.
- [ ] 4.3 Live smoke against a scratch `HC_SESSION`: launch a throwaway worker, read it, close it.
