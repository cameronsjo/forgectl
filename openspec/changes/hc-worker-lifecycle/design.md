## Context

forgectl#1386 lists the follow-ups from the hc worker surface. This change takes the five that concern a worker after launch. The environment scrub, the shared hc runner and the spec archive are a separate change.

## Decisions

### The send re-checks the worker for a done worker too

`hc send --if idle` makes hc type only while the status is still `idle`. hc's `--if` takes no `done`, so a worker last seen `done` had no guard between brief's checks and the paste. `SendBrief` now takes the checked status and turn and re-reads the terminal, as `PressEscape` does, and refuses unless the turn is unchanged and the status is `idle` or `done`. It accepts either status where the other was checked: the Escape sent just before can make a harness report again, and neither status means a turn is running. When the re-read says `idle`, `--if idle` goes with the send, so hc closes the window to its own call; for `done` the window left is one hc round trip, the same as the Escape's. It costs one more `hc get` per brief.

### forget drops a row only on proof

A core keeps its id across `hc server restart` and `hc update` and gets a new one only when its state is wiped, and a terminal does not outlive its core's state. So a reference whose core id no longer matches, on the same session, names a terminal that is gone. `hcadapter.Adapter.Abandoned` runs locate's checks in locate's order and tells the mismatches apart. A changed core id, nothing at the recorded address, or an address holding another terminal (another creation time or label) proves the worker's terminal gone. A reference taken under another `HC_SESSION` selection, an unreadable core, and forgectl's own terminal still there, live or finished, all refuse. Probe calls a finished terminal gone; forget does not, because close removes it.

`surface forget` refuses rows close settles without hc (no recorded terminal, stage `closed`, a launch less than ten minutes old) and removes the row only if it is still the one it read (`worker.SameRow`). It never runs git: the worktree and branch stay, and the result says so. It is hc-only: herdr's close already settles a restarted server by the ownership marker.

Declined: making close treat a changed core as gone. Close also judges and may remove the worktree, and the identity-mismatch refusal is what stops close acting on a terminal it cannot prove is forgectl's. A separate verb that only drops the row keeps that refusal intact.

### The ledger records the posture launch already resolved

`launch.BuildInvocation` returns `WorkerPosture`, each field the harness takes with its source (`default`, `launch.worker`, `repo-profile`, `launcher`). The launch writes it to the row with the harness, before the worker starts, so a launch that fails afterwards still records what it would have run with. `surface list --json` copies it.

### herdr-app visibility is an opt-in label at launch

herdr-app shows an hc terminal only when it carries `herdr.space=<space id>`; `herdr.tab=<id>` picks the tab, and without it herdr-app makes one. Its guide also says unlabeled workers stay hidden and asks callers not to add labels just to run a background worker. `herdr-app do terminal.keep` would also work, but needs the window open, a second command after launch, and a running herdr-app. So the label is set at `hc run`, only when `--herdr-space <id>` is given. The id is checked against herdr-app's shape (1-64 letters, digits, `-`, `_`; herdr-app prints `sp` and 32 hex digits) before anything runs. The label is written once and never compared, because the window may change it. forgectl still never calls herdr-app.

### Escape spacing

A second Escape inside a harness's double-press window opens a menu: rewind in Claude Code, the tree selector in pi (500 ms, documented). The paste and Enter that follow land in it. An hc brief therefore waits until one second has passed since the worker's last typed brief, whose `sent_at` is recorded right after its Escape. The wait comes before the checks, so they still read the worker right before the Escape. A last time in the future waits one second, never longer. An Escape that hc may have delivered but forgectl could not confirm (`ErrSendFailed`) has no record, so the refusal holds one second before it returns, and a retry at once cannot double-press. An Escape refused before any key (the re-read saw the worker change) holds nothing. Claude Code's window is not documented; one second is a margin over pi's.

### Not taken here

- The default `--surface` stays `herdr` until herdr 0.9 is retired on every working machine.
- A second readiness signal against a forged OSC 7501 `idle`: the Escape before every paste is the guard that holds against a forger; a second forgeable signal adds nothing.
