## Context

The desk already has a careful threat model for scripts (ADR-0012): it defends against accidents, not a same-uid process. Asks are lower stakes (nothing runs), but they add two new surfaces: text from agents rendered in a browser, and an HTTP listener that accepts writes.

## Goals / Non-Goals

**Goals:** one place for everything waiting on the operator; answering from a terminal or a phone; the asking agent finds out without polling chat; scripts visible beside asks.

**Non-Goals:** running scripts from the web page (approval stays on the dashboard, where the hash check and the terminal live); multi-user access; a hosted service.

## Decisions

### One log, folded

Asks, responses and notify outcomes are records in one JSONL file. A reader folds the records into per-ask state (open, snoozed, answered). Why: one file is one lock and one tail for watchers; an append-only log keeps a full history for free; a fold is a pure function that tests without I/O.

A reader skips a line with no trailing newline (a write in progress) and resumes at the byte offset after the last complete line. `--since OFFSET` is that offset.

### Locking

Writers hold `config.WithFileLock(log.jsonl)` (flock on a sibling `.lock` file, the helper the config and herdr writers share) across read-fold-check-append. Two front ends answering the same ask at once: the second sees it answered and gets `ErrClosed`. Notify runs after the lock is released, and its outcome is appended under the lock again.

### Answer semantics

`done`, `reply` and `link` answer an ask; a later response is refused. `snooze` hides it until a time at most 30 days out; after that it is open again. An agent that needs more after a reply asks again.

### Notify

The ask records an hc address (`--notify t1k`, or `self` for `$HC_TERMINAL_ID`). After a response, forgectl runs `hc send --json -- <addr> <line>` with the launcher handles scrubbed, as every hc call does. It does not pass `--force`, so hc's refusal of a blocked terminal stands. The line is one line: `DESK-RESPONSE <id> <kind>[: text|url| until T] -- via <front end> -- ask: <title> -- full record: forgectl desk watch --responses <id>`. hc presses Enter after it, so if the agent has exited and a shell holds the terminal, the shell reads the line: it carries no shell metacharacter (free text is stripped, URLs percent-encoded). A failed notify is logged and shown; it never undoes the response.

### Web auth: a per-install token in the URL fragment

`desk serve` prints `http://127.0.0.1:PORT/#k=TOKEN`. The token is 32 random bytes in `asks/web.token` (0600), created once and kept until `--rotate-token`. A small same-origin script reads the fragment, POSTs it to `/session`, and the server answers with an `HttpOnly; SameSite=Strict` session cookie. The cookie is stateless (expiry, random, HMAC with a key derived from the token), so sessions survive a restart and rotating the token ends them all. The script then strips the fragment from the address bar.

Why the fragment: browsers never send it to the server, so it stays out of access logs, proxy logs (including `tailscale serve`), and `Referer` headers.

Rejected: trusting the tailnet identity header (`Tailscale-User-Login`). `tailscale serve` sets it on proxied requests, but on a loopback listener any local process, or a page that DNS-rebinds to 127.0.0.1, can send the same header. It also gives no protection for plain `localhost` use.

### Web hardening

- Loopback bind only. Phone access is `tailscale serve --bg http://127.0.0.1:PORT`, with `--allow-host <name>.ts.net` so the Host check accepts the proxied name.
- Host allowlist on every request (DNS rebinding).
- CSRF: every POST needs the session cookie, a hidden token equal to HMAC-SHA256(server key, session id), and a same-origin `Sec-Fetch-Site` or matching `Origin`.
- `Referrer-Policy: same-origin` (header and meta). Under `no-referrer` a browser sends `Origin: null` on a form POST, which the origin check refuses.
- `Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; …; frame-ancestors 'none'`, `nosniff`, `no-referrer`, `no-store`.
- Agent text is untrusted. Templates are `html/template`; steps are markdown rendered with raw HTML off and then sanitized, with links limited to http and https. Ask links render only when `ask.SafeURL` holds.

### Terminal rendering

The TUI passes every agent string through `termsafe`, as the dashboard does.

## Risks / Trade-offs

- A same-uid process can read `web.token` or append to the log. Same as the desk's threat model: this defends against accidents and remote pages, not local processes.
- The token in a phone's history: the fragment is stripped after login, but the first URL may be saved where it was pasted. `--rotate-token` invalidates it and every session.
- The session cookie is host-scoped, not port-scoped, so other servers on the same host see it.
- The log grows without bound. It is capped at 32 MiB per read; past that the reader refuses and says to archive it. A `desk prune` for asks is a follow-up.
