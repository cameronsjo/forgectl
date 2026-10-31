## Why

`forgectl desk` queues scripts for the operator to run, but most of what an agent needs from the operator is not a script: sign in somewhere, approve a key in a password manager, check a page on a phone, paste a link back, or say "not now". Those requests live in chat or in an external checklist, so the operator has no single place to see everything waiting on them, and an agent cannot learn the answer unless someone tells it.

The operator asked for one live place to see and answer those requests, from a terminal or a phone.

## What Changes

- **Asks.** `desk ask` records a request: a one-line title, markdown steps, a bucket (now, soon, optional, coming), links, who asked, an optional hc terminal to notify, and the responses it allows (done, reply, link, snooze).
- **One append-only log.** Asks, responses and notify outcomes go to `<desk>/asks/log.jsonl`, under one flock. State is a fold over the log; nothing is rewritten.
- **Agents learn the answer.** `desk watch --responses ASK-ID` blocks until the ask is answered and prints each response as a JSON line. With `--notify`, each response is also typed into the asker's hc terminal as one `DESK-RESPONSE` line.
- **Two front ends over the same store.** `desk ui` is a Bubble Tea TUI (keys `d` done, `r` reply, `l` link, `s` snooze). `desk serve` is a phone-first web page bound to loopback, reachable from a phone through `tailscale serve`.
- **Scripts in the same view.** Queued desk scripts show in both front ends, read-only, with their sha256. Running one stays on the existing dashboard.
- **CLI for the rest.** `desk asks` lists asks; `desk respond` answers one from a shell.

Tracking: forgectl#1391.

## Capabilities

### Modified Capabilities

- `desk`: asks, the asks log, the response watch, and two front ends.

## Impact

- New verbs: `desk ask`, `desk asks`, `desk respond`, `desk ui`, `desk serve`. New flags: `desk watch --responses [ASK-ID] --since OFFSET`.
- New directory in the desk: `asks/` (mode 0700) with `log.jsonl`, `log.jsonl.lock` and `web.token` (each 0600). The existing `pending/`, `running/`, `done/` and `skipped/` protocol is unchanged.
- `desk serve` opens a local HTTP listener. It refuses a non-loopback bind.
- New hc call: `hc send` to an arbitrary terminal address recorded on an ask (`hcadapter.Adapter.SendLine`).
