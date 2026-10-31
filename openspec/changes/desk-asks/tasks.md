## 1. Store

- [x] 1.1 `internal/desk/ask`: types, validation, fold (pure, cross-platform).
- [x] 1.2 Append-only log with `config.WithFileLock`, O_NOFOLLOW, 0700/0600, partial-line-safe reads.
- [x] 1.3 Notify through `hcadapter.Adapter.SendLine`; outcome logged.
- [x] 1.4 Tests, with a staged break for the lock, no-follow, the URL scheme check, and the answered check.

## 2. CLI

- [x] 2.1 `desk ask`, `desk asks`, `desk respond`.
- [x] 2.2 `desk watch --responses [ASK-ID] --since OFFSET`.

## 3. Front ends

- [x] 3.1 `desk ui` TUI with keyboard respond.
- [x] 3.2 `desk serve` web page: token login, Host allowlist, CSRF, CSP, inert agent text.
- [x] 3.3 Handler tests and a staged break for each web control.

## 4. Docs

- [x] 4.1 `docs/commands/desk.md` and the bundled skill's desk reference.

## 5. Review fixes

- [x] 5.1 Referrer policy `same-origin` (header and meta): under `no-referrer` browsers send `Origin: null` and every web answer was refused.
- [x] 5.2 Notify line is shell-inert; `SendLine` refuses control and format characters.
- [x] 5.3 Append heals a torn last line; an unscoped `--responses` watch starts at the log's end and folds from 0.

## 6. Second review

- [x] 6.1 `--notify` only the asker's own terminal; `SendLine` types only into a live agent at its prompt.
- [x] 6.2 Notify line quotes all free text as data and names who answered; responses record `origin`.
- [x] 6.3 At most 20 open asks per asker.
- [x] 6.4 Web: link hosts beside labels; notify survives a dropped request; session bound to its port, 7 days; `Secure` behind tailscale serve confirmed from tailscale's source.

## 7. Third review

- [x] 7.1 Shell-inert test checks a literal character list, not the implementation's constant.
- [x] 7.2 Desk-wide cap of 200 open asks; new asks refused past 24 MiB of log.
- [x] 7.3 Page version stamp taken before the asks read.
- [x] 7.4 Link hosts shown in punycode; decoded control and format characters refused in `SafeURL`.
- [x] 7.5 Docs and help say what the notify rule and `origin` enforce.
