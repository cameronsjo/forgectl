## Context

forgectl#1386 lists the follow-ups from the hc worker surface. This change takes the five that concern a worker after launch. The environment scrub, the shared hc runner and the spec archive are a separate change.

## Decisions

### The send re-checks the worker for a done worker too

`hc send --if idle` makes hc type only while the status is still `idle`. hc's `--if` takes no `done`, so a worker last seen `done` had no guard between brief's checks and the paste. `SendBrief` now takes the checked status and turn and re-reads the terminal, as `PressEscape` does, and refuses unless the turn is unchanged and the status is `idle` or `done`. It accepts either status where the other was checked: the Escape sent just before can make a harness report again, and neither status means a turn is running. When either the checked or the re-read status is `idle`, `--if idle` goes with the send, so hc closes the window to its own call; a worker checked idle and re-read done then gets hc's refusal rather than an unguarded send. For a worker checked and still `done` the window left is one hc round trip, the same as the Escape's. It costs one more `hc get` per brief.

### forget drops a row only on proof

A core keeps its id across `hc server restart` and `hc update` and gets a new one only when its state is wiped, and a terminal does not outlive its core's state. But a new id is that proof only when forgectl is reaching the same core it launched on. hc picks the core by more than `HC_SESSION`: the socket lives under `$XDG_RUNTIME_DIR/herdr-core/<session>.sock`, state and config under the XDG homes or `HOME`, and `HC_SOCKET` names a socket outright. Measured on hc 0.1.15, a fresh `XDG_RUNTIME_DIR` alone reaches a different core with its own id, while the worker's core may still run. The reference's fingerprint covers only the session and the core id.

So a worker launch records a core selection digest on its row (`hc_selection`): a SHA-256 over the resolved `HC_SESSION` and the values of `HC_SOCKET`, `XDG_RUNTIME_DIR`, `XDG_STATE_HOME`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME` and `HOME`. The list is over-inclusive on purpose; a variable that differs only makes forget refuse. `hcadapter.Adapter.Abandoned` runs locate's checks in locate's order and tells the mismatches apart:

- A changed core id proves the terminal gone only when the row's digest is present and equals the current one. A row without a digest (launched before this change), or a digest that differs, refuses with `ErrSelectionUnproven`; the refusal names `surface close` under the launch's environment.
- Nothing at the recorded address on the matching core proves it gone.
- A terminal at the address with another creation time proves it gone: hc handed the address out again after `hc rm`.
- A terminal at the recorded address and creation time is the worker, whatever its `forgectl.surface` label says. Any hc client, the worker included, can rewrite a label (`hc label <t> forgectl.surface=`), so a label change is no proof and refuses.

A reference taken under another `HC_SESSION` selection, an unreadable core, and forgectl's own terminal still there, live or finished, all refuse. Probe calls a finished terminal gone; forget does not, because close removes it.

Every hc read starts a core when none answers its selection: measured on hc 0.1.15 for `hc get`, `hc ls` and `hc server status`, each of which created a socket in a fresh scratch selection. hc has no read that skips this (its docs exempt only Windows' `hc server status`), and resolving the socket path in forgectl would copy hc's selection rules. So forget, like list and close, can leave a core running where none was. That core starts without the launcher's handles (the hc environment scrub) and with no terminals. Under another selection its new id refuses on the digest, so a stray core never turns into a forget by itself. Under the launch's own selection, a core starts fresh only when the old one's state is gone, which is the wipe case, and the operator's next hc call would start that same core anyway.

`surface forget` refuses rows close settles without hc (no recorded terminal, stage `closed`, a launch less than ten minutes old) and removes the row only if it is still the one it read (`worker.SameRow`). It never runs git: the worktree and branch stay, and the result says so. It is hc-only: herdr's close already settles a restarted server by the ownership marker.

Declined: making close treat a changed core as gone. Close also judges and may remove the worktree, and the identity-mismatch refusal is what stops close acting on a terminal it cannot prove is forgectl's. A separate verb that only drops the row keeps that refusal intact.

### The ledger records the posture launch already resolved

`launch.BuildInvocation` returns `WorkerPosture`, each field the harness takes with its source (`default`, `launch.worker`, `repo-profile`, `launcher`). The launch writes it to the row with the harness, before the worker starts, so a launch that fails afterwards still records what it would have run with. `surface list --json` copies it.

### herdr-app visibility is an opt-in label at launch

herdr-app shows an hc terminal only when it carries `herdr.space=<space id>`; `herdr.tab=<id>` picks the tab, and without it herdr-app makes one. Its guide also says unlabeled workers stay hidden and asks callers not to add labels just to run a background worker. `herdr-app do terminal.keep` would also work, but needs the window open, a second command after launch, and a running herdr-app. So the label is set at `hc run`, only when `--herdr-space <id>` is given. The id is checked against herdr-app's shape (1-64 letters, digits, `-`, `_`; herdr-app prints `sp` and 32 hex digits) before anything runs. The label is written once and never compared, because the window may change it. forgectl still never calls herdr-app.

### Escape spacing

A second Escape inside a harness's double-press window opens a menu: rewind in Claude Code, the tree selector in pi (500 ms, documented). The paste and Enter that follow land in it. An hc brief therefore waits until one second has passed since the worker's last typed brief, whose `sent_at` is recorded right after its Escape. The wait comes before the checks, so they still read the worker right before the Escape. A last time in the future waits one second, never longer. Two paths send an Escape and record no time: one hc may have delivered but forgectl could not confirm (`ErrSendFailed`), and one that went out before the ledger write failed. Both hold one second before the refusal returns, so a retry at once cannot double-press. An Escape refused before any key (the re-read saw the worker change) holds nothing. Claude Code's window is not documented; one second is a margin over pi's.

### The ledger names a newer forgectl

New row fields (`posture`, `hc_selection`) come without a ledger version change, as earlier ones did, and the decoder stays strict: a rewrite by an older build would drop them. An unknown field now reads as "it holds field "x", which a newer forgectl wrote; upgrade every forgectl on this machine" instead of a bare JSON error.

### Not taken here

- The default `--surface` stays `herdr` until herdr 0.9 is retired on every working machine.
- A second readiness signal against a forged OSC 7501 `idle`: the Escape before every paste is the guard that holds against a forger; a second forgeable signal adds nothing.
