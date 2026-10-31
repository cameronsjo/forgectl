## 1. Store

- [x] 1.1 `internal/desk/ask`: types, validation, fold (pure, cross-platform).
- [x] 1.2 Append-only log with `config.WithFileLock`, O_NOFOLLOW, 0700/0600, partial-line-safe reads.
- [x] 1.3 Notify through `hcadapter.Adapter.SendLine`; outcome logged.
- [x] 1.4 Tests, with a staged break for the lock, no-follow, the URL scheme check, and the answered check.

## 2. CLI

- [x] 2.1 `desk ask`, `desk asks`, `desk respond`.
- [x] 2.2 `desk watch --responses [ASK-ID] --since OFFSET`.

## 3. Front ends

- [ ] 3.1 `desk ui` TUI with keyboard respond.
- [ ] 3.2 `desk serve` web page: token login, Host allowlist, CSRF, CSP, inert agent text.
- [ ] 3.3 Handler tests and a staged break for each web control.

## 4. Docs

- [ ] 4.1 `docs/commands/desk.md` and the bundled skill's desk reference.
