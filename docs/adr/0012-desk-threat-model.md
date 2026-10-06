# 0012. Desk threat model: defend against accidents, not against a same-uid process

**Status: Accepted**

Date: 2026-10-05

## Context

An agent session often reaches a step it should not take itself: a merge it does not own, a `sudo` command, a change to a shared machine. The agent's own guardrails stop it there. The work still has to happen, so the agent writes the script and a person runs it.

Before `forgectl desk`, that hand-off was a loose directory of scripts and a Bash dashboard. A script could change between the moment the agent described it and the moment the person ran it, and nothing would notice. Two dashboards on one directory could run the same script twice. Script text went straight to the terminal.

`forgectl desk` ([docs/commands/desk.md](../commands/desk.md)) replaces that with a queue: the agent stages an item with `desk add`, which fixes the item's sha256 and prints it for the agent to report, and the person approves it on the dashboard with one key, `y`, after matching the short hash shown for the item to the one the agent reported. This ADR records what that queue protects against, and what it does not.

## Decision

### The desk defends against accidents

The person approving an item is the control. The desk's job is to make sure that what they approve is what runs, once, and that reading it cannot hurt them. It defends against:

- **An item edited after it was queued.** The sha256 is fixed once: at `desk add`, or the first time a desk sees a hand-dropped item. Every scan re-hashes pending items. An item whose bytes changed moves to `skipped/` with `skip_reason: changed` and cannot be re-armed; the agent queues a new item. On approval the desk reads the bytes once, checks the full hash (never a prefix), and runs exactly those bytes.
- **Approving a different item from the one the agent described.** The agent reports the name and sha256 that `desk add` printed. The dashboard's focus panel shows the first 12 hex characters of the selected item's hash, so the person can match the two before pressing `y`; `a`, which runs several items, asks first and shows the full hash of each. A 12-character prefix is a display aid for a person, not a comparison: the run itself checks the full hash.
- **An item run twice.** A run starts by renaming the item from `pending/` into `running/`. Two desks on one directory race on that rename; the loser sees `ENOENT` and does nothing. A number whose `done/` files already exist is refused: the item moves to `skipped/` as `name-reused`, and nothing of the earlier run's is written. A run creates its `done/<name>.events` and `done/<name>.log` exclusively, so if another run's file appears between the check and the run's start, the run is refused or abandoned without writing into it, and an abandoned run removes the events file it created.
- **Symlink, hard-link and FIFO substitution.** Pending items are opened with `O_NOFOLLOW`, and anything other than a regular file with a link count of 1 is refused. The desk's other readers open with `O_NONBLOCK` and refuse anything but a regular file, so a FIFO cannot hang them; `desk add` and `desk plan` do the same for the file they are given. Every path below the desk is resolved through a directory descriptor pinned when the desk opens, so a swapped directory is refused rather than followed.
- **Hostile text in the terminal.** Item names, `# WHAT:` and `# WHY:` lines, script lines, log tails, event lines and notification text are rendered through `internal/termsafe`, so a script cannot move the cursor, set the window title, or hide text from the person reading it. A meta file's `sha256` must be 64 lowercase hex characters, or the item is refused and the hash is never shown. `--what`, `--why` and `--reason` refuse control and bidi characters, so the header lines written into a script are plain text for `cat` and editors too.

### It does not defend against a same-uid process

A process running as the same user can already press keys in the desk's terminal (through the multiplexer, for example) and edit any file in the desk directory between scans. The desk does not try to stop it. That boundary belongs to the agent's own guardrails, which are what made the agent hand the script over in the first place.

**Touch ID approvals were considered and declined.** Signing each approval with a user-presence check (as `forgectl workflow bless` does, ADR-0006) would close the keypress gap. It was declined to keep the desk simple: the agent's guardrails are the boundary, and the desk is a convenience on the right side of it.

### What the hash covers

The hash covers exactly the item's own bytes: the `.sh` script or the `.manifest` file, header lines included. At run time those verified bytes reach bash through a pipe, never by re-opening the file, so a write to the file after the check cannot change what runs.

The hash does **not** cover anything the script sources, calls, or downloads: a `source ./lib.sh`, a binary on `PATH`, a `curl … | sh`, or the commands a manifest step runs from other files. Reviewing those is the approver's job. "Unchanged since queued" means this file, not the world it touches.

### Redaction limits

A batch manifest step marked `private` keeps its output out of the combined log, and the values it writes to `STEP_OUT` are replaced in later output with `<redacted:OUT_<step>_<key>>`. In the words of the batch runner's own comment (`internal/desk/redact.go`): "Redaction reduces exposure; it is not a guarantee".

`private` redaction replaces the run's own known values of 6 or more characters only: the `STEP_OUT` values of private steps in the same run. It does not find secrets by pattern, it does not redact a value shorter than 6 characters, and a value that a later step transforms (encodes, splits, reverses) passes through unchanged. A private step's raw log is kept under `done/<name>.d/steps/` for the operator, and is not redacted.

### Supervisor and liveness

A detached run is owned by a hidden `forgectl desk _supervise --sha <sha256> --kind <script|batch> <name>`, started in a session of its own so it outlives the desk's pane. The desk passes the full hash and the kind it claimed, and `Claim` itself refuses anything but a full 64-hex-digit hash and a file of another kind than the one queued. The supervisor runs the item only when that hash is the one recorded at queue time, the item in `running/` is still that kind, and its bytes still hash to it. A supervisor whose hash or kind does not name the claim refuses without touching the item, which reads as lost once the claim grace passes; one that finds changed bytes moves the item to `skipped/` as `changed`. So a rewrite of both the `running/` record and its meta runs nothing. Every item starts in the home directory, with bash's startup hooks (`BASH_ENV`, `PS4`, exported functions and the rest listed in [docs/commands/desk.md](../commands/desk.md#items)) removed from its environment, so it never inherits the desk's cwd and bash runs none of its own startup code ahead of the approved bytes. This is not an environment sandbox: the rest of the operator's environment, `PATH` and loader variables such as `LD_PRELOAD` included, passes through, as it would to any command the operator runs. The supervisor records its `pid` and `pid_start` (the process start time) in the item's `.meta.json`; liveness checks compare both, so a reused pid never reads as alive.

The owner also holds a lock on the item (`running/<name>.lock`) from the start of the run to its end, and the kernel drops it when the owner dies. Skip checks that the lock it takes is still the file at that path and removes the file only after it has moved the item, so a live run cannot be skipped. A run whose owner is gone with no `RUN-END` event, or a claimed item that recorded no owner within 60 seconds, is **lost**. The queue shows it as `lost`, and `desk watch` prints `RUN-LOST` and exits 1. Nothing clears it automatically: `forgectl desk skip <name> --reason …` (or `s` on the dashboard) moves it out of `running/`, and a lost run cannot be re-armed. Every skip by a person or an agent records who skipped (`skipped_by`: `dashboard` or `cli`) and when (`skipped_at`), so the history shows an agent's skip as an agent's.

### Nothing is deleted on its own

- `forgectl desk prune` is the only command that deletes an item's files, and only when run (the desk removes only its own owner locks and temporary files). It removes the protocol files of `done/` and `skipped/` items older than `--days`, and never touches `pending/`, `running/`, unknown files, or symlinks.
- A pending item that waits more than 24 hours shows as `stale`. It stays until the person skips it.
- Anything at the desk root other than the four protocol directories is ignored and never touched.

## Consequences

- An agent can hand over work that needs a person, and the person can trust that the bytes they read on the dashboard are the bytes that run, once.
- A same-uid process that wants to approve its own item can: the desk does not stand in its way. Anyone who needs that boundary needs a different tool, or the declined Touch ID approval.
- An approver who skips reading what a script sources or downloads approves that too. The dashboard shows the script's own lines and its hash; the rest is outside the hash.
- Lost runs and stale items pile up until a person clears them. That is deliberate: nothing the desk deletes on its own can surprise the person who queued it.
- `private` is a courtesy for logs, not a secret store. A secret that must never reach disk does not belong in a desk batch.
