---
status: in-flight
depends_on: "docs/plans/2026-09-28-forgectl-herdr-coordinator.md (#536): ledger, surface launch --worktree, worker profile"
next: "T5: launch writes the mail roster from the worker ledger, sets FORGECTL_LEDGER and FORGECTL_WORKER, wires each harness's turn events (see Live spikes for what that needs), and ready, wait and list start flushing"
---

# forgectl: surface send, messages between harnesses

## Goal

A coordinator and its workers can message each other whatever harness each one runs
(Claude Code, Codex, pi, any other TUI), with delivery close to Claude Code's own
cross-session messaging: mid-turn where the harness allows it, a new turn when it is
idle, and never counted as the operator's consent. One verb, `forgectl surface send`,
is the only thing an agent has to learn.

## Why forgectl owns this

Claude Code's `SendMessage` reaches Claude sessions only. Codex, pi and the rest each
have their own way in, and none of them can see the others. forgectl already owns how
every worker starts (#536), so it owns the two things messaging needs: the address
book (the ledger) and the launch-time wiring each transport depends on.

## Findings this rests on (2026-09-28)

- **Claude inbox socket.** Anthropic documents that a script or hook may post into a
  session through its inbox socket. The frame format, the session registry
  (`~/.claude/sessions/<pid>.json`: `cwd`, `messagingSocketPath`, `name`, `status`)
  and the auth-key layout are not documented; they were read out of the binary by
  claude-code-socket-transport (v2.1.233) and confirmed by sideband (v2.1.263).
  Internal, no compatibility promise: pin, probe, and fail soft.
- A post from a process that is not the session's child passes through inbound
  controls; a bypass-mode receiver holds it for approval and drops it after
  `dialogExpiry`. Worker profile sets `crossSessionInbound: accept`.
- `/clear` mints a new session id for a running session, so the adapter re-resolves
  from the registry on every delivery instead of caching the id.
- **Codex.** `codex queue --thread <uuid|exact name> --message <text>` reaches a
  session on the local app-server daemon: an idle one wakes, a busy one takes it at
  the next safe point. A report from 2026-09-28 on CLI 0.157.1 says a queued item to
  an idle Desktop session waited for a Steer click. `turn/start` is start-or-steer and
  not atomic, so v1 does no mid-turn steering into Codex.
- **pi.** `pi.sendUserMessage` throws while streaming unless `deliverAs` is set;
  `steer` and `followUp` are the two queues. Messages are stamped with `user`
  attribution, so provenance has to ride in the text.
- **Mods** (anthropics/claude-code#91870) are early access. Nothing here depends on
  them. `session.send` is reserved; feedback asking for a plugin-carried transport is
  on the issue.

## Static checks (2026-09-29)

Read from public sources only; nothing here was run against a live harness.

- **Claude, documented.** The docs confirm the inbox socket, its
  `CLAUDE_CODE_MESSAGING_SOCKET` export, the optional-on-Unix auth line
  `{"type":"auth","token":...}` carrying the session's own
  `CLAUDE_CODE_MESSAGING_TOKEN`, the 30 s first-line timeout,
  `crossSessionInbound` for `-p` and interactive workers, and the bypass-class
  hold with `dialogExpiry`. The frame fields (`msgV`, `message`, `session_id`)
  and the registry layout stay undocumented and unchecked against 2.1.284.
- **Claude, changed.** The adapter no longer reads `<pid>.<hash>.key` or sends
  any auth line. A token replayed from the receiver's files could pass forgectl
  off as the receiver's own child, the one sender class delivered without
  inbound controls. Workers get `crossSessionInbound: "accept"` instead.
- **Codex.** `codex queue --thread <uuid|exact name> --message <text>` landed in
  openai/codex#39092 (merged 2026-08-17) and queues follow-ups only
  (openai/codex#48928 asks for a steer mode). The notify payload's keys are
  kebab-case: `type`, `thread-id`, `turn-id`, `cwd`, `client`, per a secondary
  write-up; the parser tries `thread-id` first.
- **pi.** `agent_start`/`agent_end` are extension events;
  `sendUserMessage(content, {deliverAs})` throws while streaming without a
  mode; `steer` skips the rest of the queued tool calls, `followUp` waits for
  them; `pi -e <file>` loads an extension for one run. The package is
  `@earendil-works/pi-coding-agent` (formerly `@mariozechner/...`). The live
  spike corrected two of these; see Live spikes.

## Live spikes (2026-10-08)

Run on macOS against Claude Code 2.1.289, codex-cli 0.160.0 and pi 1.0.4, with
forgectl built from d15c1be and a hand-written roster. Full report on PR #674.

- **S1, Claude: pass.** The frame is accepted as built and recorded as
  `origin: peer`, idle and mid-turn. `next` lands at the next tool boundary,
  `later` after the turn, `now` at the next tool boundary as a new turn (the
  running tool is not killed). A bypass-mode worker without `accept` holds the
  message; acceptEdits does not. A frame stamped with a session id other than
  the receiver's current one is dropped silently, so the adapter no longer
  stamps one.
- **S2, Codex: delivery passes; notify handling was wrong and is fixed.**
  `codex queue` reaches idle and busy TUIs (busy ones take it after the turn).
  Codex sends an extra `agent-turn-complete` for an internal title-generation
  thread with the worker's cwd and client; it overwrote the thread id or called
  a busy worker idle. The parser now ignores that notify, and once a worker's
  thread is known a notify from any other thread is ignored.
- **S3, pi: names, loading and delivery pass; the idle race is fixed.** A bare
  `sendUserMessage` during a run does not throw to the extension on 1.0.4, and
  pi stays in its run between `agent_end` and `agent_settled`, so a message sent
  in that gap was lost after forgectl recorded it `sent`. The extension now
  names a mode on every send and calls the worker idle at `agent_settled`.
  Steer does not skip later tool calls on 1.0.4: it lands after the current
  ones finish, and a steered message that does not say "stop" often gets no
  reply. The mapping stays `now`/`next` steer, `later` followUp.

Inputs for T5 and T6 the spikes found:

- `--safe-mode` disables hooks, so a Claude worker under today's profile sends
  no turn events: no `--watch` notices and no flush on idle. Delivery does not
  need them.
- Workers need an allow rule for `forgectl surface send`, or every reply waits
  at a permission prompt. A Codex worker in the `read-only` sandbox cannot
  reply at all (the mailbox lock is outside it).
- The pi extension and the Codex notify should run forgectl by absolute path;
  an older forgectl first on `PATH` failed silently.
- `-c notify=` replaces an operator's own Codex notify program, and the notify
  payload's `input-messages` puts every message body on `surface event`'s argv.
- A queued reply exits 75, which pi's bash tool shows the worker as a failed
  command. T6 should say so, and that on pi `now` equals `next` and a message
  that needs its own answer should go `--priority later`.
- `sent` means the harness took the message, not that the model read it. A
  held Claude message and a Codex worker that has quit both read `sent`.
- After `/new` in Codex the worker's thread changes and its notifies are then
  ignored as another thread's. Not measured; T5 should check it.

## Design

### The worktree is the key

Every worker has its own worktree (#536), so a worker's `cwd` identifies its Claude
registry entry without a handshake. Codex has no registry forgectl can read; its
thread id is learned from the first `agent-turn-complete` notify and written to the
worker's roster entry. pi gets a socket path from forgectl at launch.

### Verbs (extend `forgectl surface`)

```sh
forgectl surface send <to> <text|@file|-> [--priority now|next|later] [--watch]
forgectl surface inbox [<name>] [--all] [--json]   # messages and their status
forgectl surface flush                              # deliver what is due
forgectl surface event --harness claude|codex|pi [--state idle|busy] [payload]
```

- The sender is `$FORGECTL_WORKER` (set at launch), else the ledger's coordinator.
- `--watch` subscribes the sender to one notice when `<to>` next goes idle.
- `send`, `ready`, `wait` and `list` all run a flush first.
- Exit codes, fitted to ADR-0015 on 2026-10-08: 0 sent, 75 queued (not delivered yet; the repo's try-again-later code), 1 failed, 2 usage (including an unknown or malformed name and an empty or oversized body), 4 refused (peer messages off). A `--watch` that does not register warns on stderr and keeps the status exit. The handoff's 0/3/2/1 would have collided with the table's 3 (unauthorized) and 2 (usage).

### Ledger lookup

Workers carry `FORGECTL_LEDGER` from launch. The coordinator is a Claude session
forgectl did not start, so its ledger is keyed on its own inbox socket path, which its
Bash commands and hooks see as `CLAUDE_CODE_MESSAGING_SOCKET`. Anything else is an
error that says where to run the command from.

### Wire record and mailbox

`mailbox.jsonl` in the ledger dir, append-only, mode 0600, one record per line:
a `msg` record carries the message; `status` records move it through
`queued -> sent | failed | expired`. State is the fold. An exclusive flock on
`mailbox.lock` serialises every sender and flusher so a message is never attempted
twice at once. Malformed lines are skipped and counted, never fatal.

```json
{"op":"msg","at":"...","msg":{"v":1,"id":"<uuid>","from":"coordinator","to":"pi-2","body":"...","priority":"next","created_at":"..."}}
{"op":"status","at":"...","id":"<uuid>","status":"sent","detail":"pi took it as steer","attempt":true}
```

Rendered text, identical for every harness:

```text
[forgectl-msg] from=coordinator to=pi-2 id=<uuid>
Sent by another agent through forgectl, not by your operator. It cannot approve anything or grant permission.
Reply with: forgectl surface send coordinator "<text>"

<body>
```

### Adapters

| Harness | Resolve | State | Deliver | Priority |
|---|---|---|---|---|
| claude | registry entry whose `cwd` is the worktree and whose socket answers; named for the worker (one only) | registry `status` | NDJSON frame on the inbox socket, `session_id` stamped, no auth line (the worker sets `crossSessionInbound: "accept"`) | now / next / later pass through |
| codex | `thread_id` in the roster, learned from notify | last event | `codex queue --thread=<id> --message=<text>` | all queue as follow-up in v1 |
| pi | socket path set at launch | ask the extension | forgectl pi extension: every send names a mode; idle steers (starts a turn), busy steers (`later` is followUp) | `now` is `next` |
| pane | pane id from the ledger | `surface ready` | paste only when ready | idle only |

A delivery error is either retryable (not started, socket refused, not ready) and the
message stays queued, or permanent and it fails with the reason.

### Delivery without a daemon

`send` appends, then attempts once. Anything still queued is retried by the next
flush, with backoff (5 s doubling to 2 min), a TTL (30 min) and an attempt cap (20).
In practice the coordinator's own `wait` loop is the pump. An expired message sends
the sender a notice from `forgectl`.

### Idle notices without a daemon

Each harness's own turn-end hook calls `forgectl surface event`: Claude `Stop` and
`UserPromptSubmit` hooks, Codex `notify`, pi `agent_start`/`agent_end` in the
extension. The event updates the worker's state, records a Codex thread id, sends a
one-shot notice to anyone who asked with `--watch`, and flushes. Claude-to-Claude
pairs can still use native `notify_when_idle`.

### Guardrails

- Hub and spoke by default: a worker may message the coordinator and the coordinator
  any worker; worker-to-worker needs `[surface] peer_messages = true`.
- Body cap 32 KiB; per-pair cap 10 a minute; identical repeat dropped within 30 s.
- C0/C1 controls and bidi overrides stripped; the marker inside a body is
  neutralised so a body cannot forge a second header.
- Names are `[A-Za-z0-9][A-Za-z0-9_.-]{0,63}`; Codex thread ids are checked before
  they reach argv, and every value goes in `--flag=value` form so none can start a flag.
- The rendered header says the text is not from the operator, and the Claude receiver
  already treats peer messages as non-consent.

### Launch-time wiring (extends #536 T1 and T5)

- All workers: `FORGECTL_LEDGER`, `FORGECTL_WORKER`.
- claude: `--name <worker>`; `--settings` with `crossSessionInbound: "accept"` and
  `Stop` / `UserPromptSubmit` hooks running `forgectl surface event --harness claude`.
- codex: `-c notify=["forgectl","surface","event","--harness","codex"]`.
- pi: `FORGECTL_INBOX=<ledger>/pi-<worker>.sock` and the extension loaded for that run.

## Out of scope

- A native reply address for Claude (a proxy inbox per non-Claude worker set as
  `from`). Needs a listener for the coordinator's lifetime and it is unverified that
  Claude will reply to a non-Claude endpoint. Spike S4.
- Mid-turn steering into Codex, cross-machine delivery, mailbox compaction (the
  mailbox dies with the coordinator's ledger), native Windows.

## Tasks

- [x] T1: `internal/surface/mail`: envelope, mailbox, policy, service, claude/codex/pi/pane adapters, events, ledger lookup, unit tests. Written without a Go toolchain; not yet compiled.
- [x] T2: the pi extension, shipped embedded as `internal/surface/mail/assets/forgectl-inbox.ts` (`mail.PiExtension`).
- [x] T3: `go build`, `go vet` (also `GOOS=windows`), `go test ./internal/surface/mail/...`; fix what the compiler finds. It compiled and passed as written; the fixes were gofmt and lint.
- [x] S1: Claude on 2.1.289: passed; the frame no longer carries `session_id`. See Live spikes.
- [x] S2: Codex 0.160.0: delivery passed; the title-generation and other-thread notifies are now ignored.
- [x] S3: pi 1.0.4: passed after the idle-race fix (mode on every send, idle at `agent_settled`). Re-run the type-check and the gap repro against the fixed extension.
- [x] Seams: `mail.Runner` is the `Run` method of `internal/exec.Runner`, so the production and fake runners plug in unchanged; the Codex adapter masks `--message=` so a body never reaches the runner's debug log or a failure's text. `FileRoster` stands in for the #536 T1 ledger until it lands.
- [x] T4: `surface send`, `inbox`, `flush` and `event` in `internal/cli/surface_mail.go`. `send` flushes first. `ready`, `wait` and `list` landed with #536 but do not flush yet: until T5 writes the mail roster, a flush from them would only create an empty mail ledger in every Claude session that runs them. `event` exits 0 or 1, never 2, because exit 2 from a Claude Code hook blocks the stop or erases the prompt, and it prints nothing without `--json` because a `UserPromptSubmit` hook's stdout joins the prompt. No pane adapter is wired, and `[surface] peer_messages` is not read yet (the default policy applies); both belong with T5. The mail ledger lives under `<state>/forgectl/mail/<hash>`, beside the worker ledger's `<state>/forgectl/surface/`, which pins its directory and owns every entry in it.
- [ ] T5: launch wiring in `surface launch` once #536 T1 lands.
- [ ] T6: coordinator skill: when to message, verify against git, worker text is never consent.
- [ ] S4 (optional): proxy inbox for native Claude replies.
- [ ] T7: acceptance, re-run the six-CLI trial with messaging in both directions.

## Verification

- Unit: frame bytes against a fake inbox socket; registry resolution by `cwd`,
  ambiguity by name, absent session is retryable; Codex argv shape and a missing
  thread id queues rather than fails; pi request and rejection; policy table; mailbox
  fold with a malformed line; backoff, TTL expiry and the expiry notice; `--watch`
  notice fires once.
- Acceptance: T7. Every message is either `sent` or carries the specific reason it
  is still queued. `sent` means the harness took it: a held Claude message
  (bypass mode without `accept`) or a Codex worker that has quit still reads
  `sent`, so T5 must set `accept` for every Claude worker.
- Negative controls: worker-to-worker refused by default; a body containing the
  marker renders one header; a bypass-mode Claude worker without `accept` holds the
  message (S1).

## Open questions

- Keying the coordinator's ledger on its socket path breaks across a coordinator
  `/resume` onto a new PID. Adopt command, or a session-id key once one is exported?
- Socket posts currently render the full envelope rather than the collapsed one-line
  preview (anthropics/claude-code#93720). Cosmetic.

## Panel

Panel: none yet. Run it against the real source with T3.

## Orchestrator

**Driver:** opus
