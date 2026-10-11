---
status: proposed
next: "I1 (the read verb over desk, drain and herdr, plus drain-pr.json) before I2 (the decisions store). The atelier mod (P6) is being built against this shape in parallel."
branch: claude/project-thread-gvjp0y
date: 2026-10-11
updated: 2026-10-11
source_plan: "The Cockpit: north star (Claude Doc, 2026-10-11) § Sequence 2; cadence-ecosystem docs/plans/2026-10-05-atelier-a-herdr-work-queue-cockpit-for-claude-code.md; docs/plans/2026-09-28-forgectl-herdr-coordinator.md T11 (this plan's amendment)"
---

# atelier inbox: one needs-you list (forgectl T11)

## Goal

`forgectl inbox --json` prints every item on this machine that waits on the operator, in one shape, oldest first. It reads what forgectl and herdr already record and adds one new store, for decisions. Each item says what it is, why it needs the operator, and the forgectl or herdr command that acts on it. The verb never runs an item's action and never changes the state it reads; its only write is its own hold file.

Its readers are named, per the cockpit's "every surface names its reader" rule:

- The atelier pane (P6) draws it first, above agent state, and polls it.
- The cockpit's Questions pane walks it one item at a time.
- The chief of staff reads it to know what is already waiting before it adds more.

## Sources

The north star lists four sources: desk items, decisions, P4 `ready-to-close` rows and blocked herdr panes. P4 dropped board `ready-to-close` (P4 plan, Deviations), so that source becomes the worker PRs the drain does not merge on its own. The drain's own state is added, because a paused or stopped drain strands every queued task.

| Kind | Comes from | Needs the operator when | Actions |
|---|---|---|---|
| `desk` | the desk's `pending/` and `running/`, through a new read-only listing | an item waits, or a run is `lost` | `forgectl desk` (`tty`): the operator checks the hash and presses `y`, or `s` for a lost run |
| `worker` | `queue.json` rows at `needs-you` or `failed` (`drain.Summarize`) | the drain stopped a worker at a blocking screen, at its prompt with no report, or on a failure | blocked: `herdr --session <s> agent focus <pane>`; idle: `forgectl surface read <name>`; failed: `forgectl surface close <name>` |
| `pr` | `queue.json` rows at `reported`, joined to `drain-pr.json` (new, I1) | see "The `pr` kind" | by merge mode, below |
| `drain` | `drain.json` (`drain.Effective`) and the queue's counts | `paused`; `stale`, `stopped` or never started while rows are `queued` or live | `forgectl surface drain status`; stopped: `forgectl surface drain start`; a `github-auth` pause: `gh auth login` (`tty`) |
| `decision` | the decisions store (I2) | a decision is pending | `forgectl inbox answer <id>` (`tty`) |
| `pane` | `herdr agent list` per session read | herdr says `blocked` for at least `--hold` (default `20s`), and no item above covers that pane | `herdr --session <s> agent focus <pane>` |

A `pane` item is a hint, never a verdict (`docs/lore/herdr-fleet.md` in the meta-repo): any pane can report its own status. The item says "herdr reports blocked" and nothing more.

### The `pr` kind

The closer already reads each reported row's PR at most every 5 minutes (T10.3, `merge.Reader.Discover`), so the inbox reads what the drain recorded and calls no GitHub API itself.

- **Where it is kept.** `decodeQueue` refuses unknown fields (`internal/surface/worker/queue.go:322-333`), so a new field on queue rows would make `queue.json` unreadable to an older binary after a rollback, pausing its drain and every `surface` verb. The facts go in a separate `drain-pr.json` beside the queue instead, keyed by row name and `launch_id`: `{number, state, head, read_at, merge_refused: {head, at} | null}`. The closer writes an entry only when the number, state or head changes, not at every read. `surface prune` drops an entry with its row. Every read-modify-write of `drain-pr.json` (the closer, the autopilot's `merge_refused`, the daily prune and a manual `surface prune`) holds the queue's flock for the whole update and replaces the file by atomic rename, so a prune running beside the drain can neither lose a `merge_refused` nor bring back a removed entry.
- **What the closer adds.** It records `state: "none"` on an `ErrNoPR` read, which today writes nothing. It records `state: "ambiguous"` on `ErrAmbiguousPR` (more than one open PR on the branch), and `state: "unreadable"` with the error's first line when any other read fails non-transiently or is refused (`merge.ErrRepoChanged`, `merge.ErrUnboundPR`). Today a row stuck on either waits on the operator with no record outside the event file. A transient failure (network, server, rate limit) writes nothing. The next successful read replaces the state. The URL is built from the row's GitHub repo and the number; `DiscoverQuery` does not change. The autopilot writes `merge_refused` when it emits a `merge-refused` event, so the condition survives a drain restart (today it lives only in the event file and in process).
- **A row with no GitHub repo recorded** is never read by the closer (`closer.go:50-54`). It is a `pr` item at once, "reported, no PR read", with `forgectl surface close <name>`.
- **When it needs the operator, and the actions, by `[surface.merge] mode`** (the inbox resolves merge settings from the config, one read):

  | State | `off` | `manual` | `auto` |
  |---|---|---|---|
  | PR open | item: `surface status <name>` and the PR URL | item: `surface status <name>`, then `surface merge <name>` (`tty`, it asks at a terminal) | item only with `merge_refused` at the current head: `surface status <name>` |
  | `none` | item: `surface close <name>` | same | same |
  | `ambiguous` | item: "close all but one PR", with `surface status <name>` | same | same |
  | `unreadable` | item: the recorded error, with `surface status <name>` | same | same |
  | merged or closed | no item: the closer acts | same | same |

### Sessions

herdr pane ids such as `w3p2` repeat across herdr sessions, and `internal/herdr`'s client follows the inherited `HERDR_SOCKET_PATH` rather than a named session. So:

- The inbox resolves its session the way `surface launch` does (`HERDR_SESSION`, then `default`; `--session` overrides) and calls herdr with an explicit `--session` and `HERDR_SOCKET_PATH` stripped, as the herdr adapter does.
- When the drain runs on another session (its `herdr_session` in `drain.json`), the inbox reads that session too: two `agent list` calls. Worker panes live there.
- Every pane is keyed by (session, pane id): cover rules, hold entries, item ids, and the `--session <s>` in every focus action.
- The desk records `signal_pane` without a session. I1 adds `signal_session` to the desk item's meta (additive and rollback-safe: the desk's meta reader does not refuse unknown fields). A desk item with no `signal_session` covers no pane.

### Covering a pane once

One stuck session can show up in several places. A pane gets one item, by these rules, in this order:

1. **Desk.** A pane is covered by a waiting desk item only when the item's `signal_pane` and `signal_session` name it **and** herdr's agent row for that pane is labelled `forgectl-desk` (the desk reports under that `--agent` label, `internal/herdr/pane.go:136`). Any other blocked pane a desk item names gets its own item too: `signal_pane` is written by whoever queued the item, and must not hide a real prompt.
2. **Decision.** The same rule with a pending decision's asker pane and the `forgectl-inbox` label.
3. **Worker.** A pane a ledger row names belongs to that worker. A `worker` item (row at `needs-you`) covers it. That includes the pane's `blocked` state under `forgectl-drain`, which the drain sets when it moves the row to `needs-you`. A row still `launched` while herdr reports the pane blocked under a label other than `forgectl-desk`, `forgectl-inbox` or `forgectl-drain` is a `pane` item carrying the worker's name. The drain moves rows on its own screen check, not on herdr's status, so a worker that is blocked in herdr may stay `launched`.
4. **Anything else** blocked past `--hold` is a `pane` item.

The I1 fixtures confirm that `herdr agent list` shows the reporting label for a pane blocked by `pane report-agent`. If it does not, rules 1 and 2 cover nothing and both items show.

### The hold, without a daemon

herdr gives no time for a status change, only `state_change_seq`, a per-server counter that restarts at 0 with the server. The verb keeps `inbox-hold.json` in forgectl's surface state directory, through the same pinned directory, flock and atomic rename as the queue, so concurrent pollers do not lose each other's writes.

- An entry is keyed by (session, server identity, pane id, `state_change_seq`) and holds the time a read first saw that pane `blocked`. The server identity is the socket's inode and change time; a new server drops that session's entries, so a restarted server's repeated seqs do not inherit old holds.
- A read prunes entries only for the sessions it read: a pane no longer blocked, or no longer listed there.
- With a positive hold, the first read after a pane blocks counts it in `settling`, not as an item. The pane polls every few seconds, so a 20 s hold costs it nothing. With `--hold 0s`, a blocked pane is an item on the first read that sees it; a one-off reader passes that.
- When the file cannot be read or written, `pane` items are shown with `since: null` and the `herdr` source row stays `ok` with the error in `error`. Items with `since: null` sort last.

The item id carries the server identity too: `pane:<session>/<pane_id>@<server>.<seq>`.

## The command

```sh
forgectl inbox [--json] [--hold 20s] [--session <herdr session>] [--kind desk,worker,pr,drain,decision,pane]
```

`--json` prints:

```json
{
  "v": 1,
  "generated_at": "2026-10-11T04:30:00Z",
  "items": [
    {
      "id": "desk:17-merge-1201",
      "kind": "desk",
      "title": "Merge PR 1201 once checks are green",
      "detail": "You own merges",
      "asker": {"kind": "session", "name": ""},
      "since": "2026-10-11T04:18:00Z",
      "age_seconds": 720,
      "recommend": null,
      "actions": [{"label": "open the desk", "argv": ["forgectl", "desk"], "tty": true}],
      "ref": {"desk": "17-merge-1201", "sha256": "3f1a9c0d2b7e…"}
    }
  ],
  "settling": 1,
  "sources": [
    {"name": "desk", "state": "ok", "error": ""},
    {"name": "drain", "state": "ok", "error": ""},
    {"name": "decisions", "state": "absent", "error": ""},
    {"name": "herdr", "state": "unreadable", "error": "session fleet: protocol_mismatch"}
  ]
}
```

- **`id`** is stable while the item waits: `desk:<name>`, `worker:<name>`, `pr:<name>@<launch_id>`, `drain`, `decision:<id>`, `pane:<session>/<pane_id>@<server>.<seq>`. A consumer keys its rows and its "new since you looked" marks on it.
- **`title` and `detail`** are text other sessions wrote: a desk item's WHAT and WHY, a decision's question, a worker's last error. They are data. Text output passes them through `termsafe`; JSON keeps them as strings, and a consumer strips control characters before drawing them, as the atelier plan already requires of the pane.
- **`asker`** says who raised the item where forgectl knows: `{"kind": "worker", "name": "<row>"}` for a decision asked from a drain worker, `session`, or `cli`. A recommendation from a worker reads as a worker's.
- **`recommend`** is set only for a decision: the index of the option the asker recommends.
- **`actions`** are argv arrays built only from identifiers forgectl has already validated (a queue or desk name, a herdr session and a pane id that passed `checkID`, a decision id). No action takes a word from `title` or `detail`. Word 0 is the program (`forgectl`, `herdr` or `gh`). `tty: true` marks an action that needs a terminal (opening the desk, `surface merge`, `inbox answer`, `gh auth login`), so a pane button shows it or opens it in a herdr pane rather than running it headless. A desk item's action opens the dashboard and never approves: there is no headless desk approve, and this plan adds none (ADR-0012).
- **`ref`** carries the keys a consumer needs to act or show more: desk name and short hash; worker name, repo, session and pane id; PR number and URL; decision id and options.
- **Order:** oldest `since` first, then `id`; `since: null` last. The Questions pane takes the first item.
- **`sources`:** one row per source, `ok`, `absent` (nothing to read: no desk directory, a drain that never ran, no herdr server for the session) or `unreadable`. An unreadable source never empties its items silently: the row says so.

Text output is one line per item, like `desk status`:

```text
inbox: 3 need you, 1 settling
  12m  desk      17-merge-1201   Merge PR 1201 once checks are green   → forgectl desk
   4m  worker    fix-lint        blocked: plan-approval dialog          → herdr --session fleet agent focus w3p2
   1m  decision  d-0007          Ship the inbox before P6? (rec: yes)   → forgectl inbox answer d-0007
```

Exit codes ([exit-codes.md](../exit-codes.md)): 0 when every source read `ok` or `absent`, the list empty or not. 1 when any source is `unreadable`, with the full verdict still on stdout (json-contract form 1). 2 for a usage error. The verb never exits 1 because something needs the operator: an inbox with items is a successful read. A consumer parses stdout on 0 and 1.

### Reads only

The desk's normal scan writes: it pins the hash of a hand-dropped item the first time it sees it, moves a changed item to `skipped/`, and its leave hook releases the herdr pane state (`internal/desk/queue_unix.go:19-34`). Polled by several readers, it would skip items and clear signals. I1 adds a read-only desk listing beside the desk's other readers: no hash pinning, no moves, no hooks. A hand-dropped item with no pinned hash shows as a `desk` item whose action opens the dashboard, which pins it. Likewise the decisions source reads files and never computes an expiry into a write.

### What it costs

The verb makes no GitHub call and reads no pane screen. Per run it reads `config.toml` (merge mode), `queue.json`, `drain.json`, `drain-pr.json`, the desk directories, the decisions directory, the ledger rows of live queue rows (for pane ids), one `herdr agent list` per session read (one, or two when the drain runs on another session), and the hold file, which it writes when an entry changes. That keeps it cheap enough to poll every few seconds at 15 workers. A test counts the herdr calls and file reads per run against fixed fixtures at 1 and 15 panes, rather than timing it.

## Decisions (I2)

The north star's second gap: decisions live in chat reports and `cadence:ask-questions` slates today, so nothing durable lists them. I2 adds a small store, built like the desk's, so a session can ask one question and get the answer back.

```sh
forgectl inbox ask --question TEXT --option LABEL --option LABEL [--option …] --recommend N [--context TEXT] [--json]
forgectl inbox answer <id> [<index|label>] [--note TEXT]
forgectl inbox wait <id> [--deadline 9m] [--skip N]
forgectl inbox withdraw <id>
forgectl inbox prune [--days 30]
```

- **Ask.** 2 to 4 options, one recommended. Question at most 300 characters, each option at most 60, context at most 1000, all one line with no control or bidi characters (the desk's `--what` rule). Prints `{id, state, duplicate}`. Asking the same question with the same options from the same asker while one is pending returns that one (`duplicate: true`), so a retry after a timeout never asks twice. Caps: 20 pending per asker, 100 in all; past either, `ask` exits 1 naming the cap.
- **The asker.** Inside a drain worker (`FORGECTL_DRAIN_WORKER` set), the asker is the worker's ledger row: its name, session and pane come from the ledger, not from the environment, and `asker.kind` is `worker`. Elsewhere it is the pane in `HERDR_PANE_ID` with the session the inbox resolves, when herdr lists that pane, else the session id from `cadence-hooks session status`, else `cli`. A same-uid process can still name any pane; the cover rules above keep such a claim from hiding a real prompt.
- **Signal.** Like `desk add`: a herdr notification, and the asker's pane put in `blocked` under source and agent label `forgectl-inbox`; a macOS notification. `[inbox] notify_herdr` and `notify_macos` turn them off.
- **Answer.** Needs a terminal on stdin, and refuses inside a drain worker (`refuseInDrainWorker`); both refusals exit 2. With no option given it prints the question, the options and the recommendation, and reads a choice. Both rules stop accidents only: a same-uid process can write the store directly (ADR-0012's posture, unchanged).
- **Wait.** The asker runs it under a monitor, as with `desk watch`: it waits while the decision is pending and exits 0 printing `{id, choice, label, note, answered_at}`. On `--deadline` it exits 75 with the decision still pending, its last line the resume command. An expired or withdrawn decision exits 1 naming which.
- **Life.** Pending, then `answered`, `withdrawn` or `expired` (7 days after asking). One JSON file per decision under `$XDG_STATE_HOME/forgectl/inbox/`, 0600 in a 0700 directory, opened through `privdir` with `O_NOFOLLOW`. `prune` deletes ended decisions older than `--days`.
- **Clearing the pane.** `answer`, `withdraw`, and a `wait` that sees the decision ended release the asker's `forgectl-inbox` state, unless other decisions from that pane are still pending (the desk's rule). A `wait` that exits 75 leaves it. Expiry has no actor of its own: the next `ask`, `answer`, `wait`, `withdraw` or `prune` that touches the store marks expired decisions and releases their panes. Until then a stale block can outlive expiry by however long the store goes untouched; the inbox stops listing the decision at expiry regardless, so the pane then shows as a `pane` item.

An answer is a choice among labels the asker wrote. It is not an approval for anything forgectl gates: no verb reads a decision as consent to merge, run a desk item or loosen a posture. The asker acts on the answer with its own permissions, as it would on a chat reply.

## Loop

| Thing created | Opened by | Closed by | Who closes |
|---|---|---|---|
| Decision file | I2 `inbox ask` | `inbox answer`, `inbox withdraw`, or 7-day expiry; the file goes with `inbox prune` | the operator, the asker, or prune |
| Asker pane's `blocked` state under `forgectl-inbox` | I2 `inbox ask` | `answer`, `withdraw`, a `wait` that sees the decision ended, or the next store verb after expiry | forgectl |
| Hold entry in `inbox-hold.json` | I1, the first read that sees a pane `blocked` | a read of that session that sees the pane unblocked, gone, at a new seq, or on a new server | the next `inbox` read |
| `drain-pr.json` entry | I1, the T10.3 closer (and the autopilot's `merge_refused`) | removed with its queue row by `surface prune` | the drain |
| `signal_session` on a desk item | I1, `desk add` | goes with the item | the desk |

The read verb itself closes nothing. Every item closes where it came from: the desk runs or skips it, the drain moves the row, the merge or close ends the `pr` item, the operator answers the decision.

## Alternatives declined

- **Decisions on the desk.** A desk item is a script pinned by hash, and `y` runs it. A decision is a choice among labels, and running it means nothing. Overloading `y` would blur the one key that runs code.
- **A verdict per `pr` item.** It would call GitHub on every poll for every reported row. The closer already reads each row every 5 minutes; the verdict stays one command away in `surface status`.
- **PR facts as a field on queue rows.** Strict decoding makes any new field a rollback hazard (see "The `pr` kind"). A side file costs one more read.
- **Naming the blocking screen for every blocked pane.** That needs a screen read per pane per poll, which is multiplicative work. Drain workers already carry the screen name in `last_error`; other panes get herdr's word only.
- **A streaming `inbox --watch`.** v1 is a poll, like the pane's other reads. `drain events --since` already streams the drain's changes; a watch mode waits for a reader that needs it.
- **Reaching the phone.** The north star leaves open whether the inbox reaches a Projects thread or a push. v1 stays on the machine; the JSON is what a later bridge would read.
- **Several machines.** The verb reads the machine it runs on. The fleet on the M5 and the Air each have their own inbox until cross-machine dispatch exists (coordinator plan, Out of scope).

## Tasks

### I1: the read verb (forgectl, one PR)

- [ ] Fixtures first, captured on sjomba: a desk with a waiting item, a hand-dropped item and a lost run; `queue.json` with `needs-you`, `failed` and `reported` rows; `drain.json` paused on `github-auth`, and one stopped with rows queued; `herdr agent list` from two sessions with blocked panes that a worker, a desk item and nothing cover, including one pane blocked by `pane report-agent --agent forgectl-desk` (to confirm the label shows).
- [ ] `internal/inbox`: one function per source returning items and a source row, and a pure merge that applies the cover rules, the hold and the order. No herdr or file access in the merge.
- [ ] The read-only desk listing (no hash pinning, no moves, no hooks), and `signal_session` written by `desk add`.
- [ ] herdr reads with an explicit `--session`, `HERDR_SOCKET_PATH` stripped; the drain's session read too when it differs; panes keyed by (session, pane id).
- [ ] The hold file: versioned, pinned directory, flock, atomic rename, server identity in the key; a read that cannot write it still prints, with `since: null` on pane items.
- [ ] `drain-pr.json`: every update under the queue flock with atomic rename; the closer writes on change and records `none`, `ambiguous` and `unreadable`; the autopilot records `merge_refused`; `surface prune` removes entries with their rows; `surface queue --json` joins it.
- [ ] The drain's pause kinds: `drain.json` keeps them only in the joined `pause_reason` (`"kind: reason; …"`). I1 parses the kind prefix against the `PauseKind` constants and treats an unknown prefix as a plain pause, rather than adding a field to the strictly decoded `drain.json`.
- [ ] `forgectl inbox` text and `--json`, exit codes as above, `docs/commands/inbox.md`, the command in `docs/README.md`'s command list, `inbox` added to `docs/json-contract.md`'s form-1 list, and a line in the embedded skill (`internal/skill/skill/SKILL.md`) so agents find it.
- [ ] Tests:
  - each cover rule with a negative control: a pane a desk item names, blocked under `forgectl-desk`, gets no `pane` item; the same pane blocked under the agent's own label gets one; the desk item gone, it gets one;
  - a worker waiting on a decision (row `launched`, pane blocked under `forgectl-inbox`) shows once, as the decision;
  - the same pane id in two sessions gives two items with two focus actions;
  - the hold: first read `settling`, a later read an item, `--hold 0s` an item on the first read, a new seq or a new server restarts it, a read of one session leaves the other's entries;
  - a poll changes nothing on disk outside the hold file (desk, queue, `drain-pr.json`, decisions);
  - each merge mode's `pr` actions; an `ErrAmbiguousPR` read and a refused read each give a `pr` item, and a transient failure changes nothing; `merge_refused` survives a drain restart;
  - a stopped drain with queued rows is a `drain` item;
  - an unreadable herdr read keeps the other sources' items and exits 1;
  - an action argv never contains text from `title` or `detail` (a desk WHAT holding `; rm -rf ~` stays in `title` only);
  - the call-count test at 1 and 15 panes.

### I2: decisions (forgectl, one PR, after I1)

- [ ] The store under `$XDG_STATE_HOME/forgectl/inbox/`, desk-style: pinned directory, 0600 files, versioned JSON, refuse an unknown version.
- [ ] `inbox ask`, `answer`, `wait`, `withdraw`, `prune`, with the limits, the asker rule, duplicate rule and caps above; `[inbox]` config keys in `docs/configuration.md` and `init`'s template.
- [ ] The `forgectl-inbox` herdr source: `SourceInbox` beside `SourceDesk` and `SourceDrain` in `internal/herdr/pane.go`; set on ask; released per "Clearing the pane".
- [ ] The `decision` kind in `inbox`, with `recommend`, `asker` and the options in `ref`.
- [ ] `inbox wait`'s 75 added to `docs/exit-codes.md` "Outside the table" beside `desk watch`.
- [ ] Tests: `answer` exits 2 with no terminal and inside a drain worker; inside a drain worker `ask` takes its asker from the ledger even with `HERDR_PANE_ID` naming another pane; a duplicate ask returns the first; the 21st pending ask from one asker exits 1; `wait` exits 0 on answer, 75 on deadline with a working resume line and the pane still blocked, 1 on withdraw; a question with a control character refuses; an expired decision's pane is released by the next store verb.

### Follow-ons outside this plan

- **atelier P6** (the mod, cadence-ecosystem atelier plan): its brief gains an inbox panel above the queue, drawn from `inbox --json`, one row per item, its buttons running only non-`tty` actions on the mod's allowlist. Workstream 3 of the north star; being built in parallel against this shape.
- **Chief of staff** (`cadence-forge:chief-of-staff`): asks with `forgectl inbox ask` instead of a chat-only decision line, and reads `inbox --json` before its report. Workstream 4.
- **Board cards**: a `board` kind returns when board intake does (P3 Deviations), or rides a named merge if cameronsjo/cadence#1673's proposal is taken.

## Verification

- forgectl: `go test ./...`, plus the I1 and I2 tests above.
- Live on sjomba, I1: with the drain running one worker, queue a desk item from a herdr pane and let a second pane sit at a Claude permission prompt. `forgectl inbox --json` shows one `desk` item, one `pane` item after 20 s, and the worker only once it reaches `needs-you`; the desk item's pane gets no second item. Stop the drain with a row queued: a `drain` item appears.
- Live, I2: `inbox ask` from a Claude session in a herdr pane puts that pane in `blocked`; `inbox wait` under a monitor exits with the choice after `inbox answer` at a terminal, and the pane clears.

## Panel

Panel: one fresh-eyes reviewer (plan, security-posture, red-team and operability lenses) against forgectl and herdr source, 2026-10-11. 12 findings, all folded in: herdr sessions and repeated pane ids; the desk scan writes; strict `queue.json` decoding (PR facts moved to `drain-pr.json`); the closer's missing URL and no-PR result; `merge-refused` not persisted; actions per merge mode; drain moves rows on screens, not herdr status; spoofable `signal_pane` and asker; decision block clearing; hold-file concurrency and server restarts; stopped drains; exit-code and contract docs.

## Orchestrator

**Driver:** opus. I2 adds a store other sessions write to and a signal into herdr panes, so its security review runs at Opus before it merges (the atelier plan's rule for each new control).
