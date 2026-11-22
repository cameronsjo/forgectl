# forgectl desk UI/UX deep dive

Date: 2026-11-22. Build: `main` at `3278ee27` (0.69.0), built locally. Status: ranking agreed with the operator on 2026-11-22; issues filed (see the table and § Rulings).

## Summary

`forgectl desk` was rendered view by view in an isolated tmux server and read at the default desk-pane width (66 columns) and at 120, with the first view also at 60, 80 and 40 columns, 6 to 40 rows, `NO_COLOR` and `--no-icons`. About 230 frames and 21 plain-text outputs were read.

The safety core holds. The 12-character sha256 survives every width down to 40 columns, the what and why wrap instead of being cut, and `y` refuses to run an item when the window is too short to show them (the fix for #1098 still works).

What costs the operator is steps. The ranking rule, set by the operator on 2026-11-22, is **friction first**: a finding ranks by how many keys, screens and reads it adds between "an item is waiting" and "it ran", and no fix may add a confirm or a second screen. Safety findings rank above friction findings; polish ranks last.

| # | Finding | Kind | Costs today | Issue |
| --- | --- | --- | --- | --- |
| 1 | `a` runs items in a window where `y` refuses | safety | weaker check than `y` | #1479 |
| 2 | After `y` the cursor stays on the item that ran | friction | up to n-1 moves per approval | #1480 |
| 3 | `desk ui` lists scripts but cannot run them | friction | a second program | #1481 |
| 4 | The run-all confirm is 64 hex characters per item, no what | friction | 384 characters for 6 items | #1482 |
| 5 | Skip, clear and done are confirmed though they can be undone | friction | 1 extra key each | #1483 |
| 6 | A failed run shows no reason on the dashboard | friction | 1 extra screen | #1484 |
| 7 | `desk ui` opens with the cursor on a read-only row | friction | 1 move before any answer | #1485 |
| 8 | A changed item says so but shows no difference | friction | a manual compare | #1486 |
| 9 | The queue hides rows with no "more" marker | lost work | items not seen | #1487 |
| 10 | Footer hints: wrong keys dropped, labels differ, a dead Enter hint | friction | guessing | #1488 |
| 11 | `desk ui` cuts lines at the edge with no marker | lost facts | an option and half a hash hidden | #1489 |
| 12 | Overlays break words in the middle | polish | slower reading | #1490 |
| 13 | The header repeats the count and spends width on version and host | polish | the age is cut | #1490 |
| 14 | The timeline panel repeats the queue in the opposite order | polish | 14 rows of repeats | #1490 |
| 15 | The run view's events are raw machine lines | polish | paths cut mid-way | #1490 |
| 16 | Plain output disagrees with the dashboard on words and facts | polish | see #1095 | comment on #1095 |
| 17 | `desk ui` smaller items | polish | - | #1490 |
| 18 | The refusal message is cut at 66 columns | polish | - | #1490 |

## Rulings

The operator ruled on 2026-11-22:

- **Filing shape.** One issue each for findings 1 to 11, one polish batch for 12 to 15, 17 and 18, and comments on #1095, #1202 and #1152.
- **Finding 2.** After `y` the cursor moves to the next waiting item (#1480).
- **Finding 4.** Once #1480 lands, `a` and its prompt are removed (#1482). That also makes finding 1 moot; #1479 stays separate because it can ship first.
- **Finding 5.** Skip acts at once and keeps `u`; confirm only where there is no undo (#1483).
- **Finding 3.** Filed as a design issue without picking the surviving surface (#1481).
- **First view.** The trim direction; the approval card stays as a later option.
- **Notifier notes.** Posted as one comment on #1476.

## How the frames were made

Every frame comes from `tmux capture-pane -p` in a throwaway tmux server, with a fixture `HOME`, a temp desk directory per state, both desk notification settings off, and stub notifier programs first on `PATH` that log any call. The stub log stayed empty for the whole capture. Fixture items are `echo`, `sleep` and `exit 1` scripts. The frames are not committed; each finding carries the rows that show it. The host name and temp paths in the excerpts are replaced.

To reproduce a frame, keep the same isolation. Run it in a scratch directory; on macOS forgectl reads its config from `$HOME/Library/Application Support/forgectl/`, on Linux from `$HOME/.config/forgectl/`.

```bash
S=$(mktemp -d); mkdir -p "$S/stubs" "$S/state" "$S/desk" \
  "$S/home/Library/Application Support/forgectl" "$S/home/.config/forgectl"
# both desk notifications off, in the fixture HOME
printf '[desk]\nnotify_macos = false\nnotify_herdr = false\n' \
  | tee "$S/home/Library/Application Support/forgectl/config.toml" \
  > "$S/home/.config/forgectl/config.toml"
# stub notifiers: a call is logged instead of shown; the log must stay empty
for n in osascript terminal-notifier herdr; do
  printf '#!/bin/sh\necho "%s $*" >> "%s/marker.log"\n' "$n" "$S" > "$S/stubs/$n"
  chmod +x "$S/stubs/$n"
done
f() { env -u HERDR_ENV -u HERDR_PANE_ID -u XDG_CONFIG_HOME HOME="$S/home" XDG_STATE_HOME="$S/state" \
        PATH="$S/stubs:$PATH" forgectl "$@"; }

printf '#!/bin/sh\necho "merging the release branch"\nsleep 1\necho "done"\n' > "$S/merge-1201.sh"
f desk add "$S/merge-1201.sh" --dir "$S/desk" \
  --what "Merge the release branch into main" \
  --why "CI is green and the review is posted at head"

tmux -L deskux -f /dev/null new-session -d -s shot -x 66 -y 40 \
  "env -u HERDR_ENV -u HERDR_PANE_ID -u XDG_CONFIG_HOME HOME='$S/home' XDG_STATE_HOME='$S/state' PATH='$S/stubs:$PATH' forgectl desk --dir '$S/desk'"
sleep 2                                   # wait for the first draw; a capture before it is blank
tmux -L deskux send-keys -t shot a        # the keys a finding names; never Enter
sleep 1
tmux -L deskux capture-pane -t shot -p
tmux -L deskux kill-server
if test -s "$S/marker.log"; then echo "a notifier was called:"; cat "$S/marker.log"
else echo "no notifier was called"; fi
```

The other states come from more fixture items queued the same way:

| State | How it was built |
| --- | --- |
| busy (nine rows) | nine `desk add` calls: short scripts, one with a 100-character `--what` |
| failed | a script that prints an error and ends `exit 1`; `y` on it |
| running | a script with `sleep 45`; `y` on it, captured while it runs |
| held | `desk add … --after 03-fail-step` |
| tty | `desk add … --tty` |
| changed | a line appended to the item's file in `pending/` after `desk add` |
| batch | a `.manifest` of three steps (`lint`, `test`, `publish after=lint,test`); a second one whose `test` step exits 3 |
| lost | `y` on a `sleep 120` script, then `kill -9` on its `forgectl desk _supervise` process |
| mixed days | a copy of the desk with the dates in `done/*.meta.json` rewritten |
| asks | `desk ask --title … --bucket <name>` for each of `now`, `soon`, `optional` and `coming`; one with `--steps-file` and `--link`, one with `--allow reply,snooze` |

`y` on a waiting item runs it at once with no prompt; it was pressed only on these fixture scripts, with notifications off and stubbed. The run-all, clear-lost and mark-done prompts were rendered and then cancelled. One skip prompt was accepted, on a fixture item, and undone with `u`.

Two capture traps, so nobody files them as bugs: the renderer moves the cursor with hard tabs and tmux keeps them (a tab near the right edge stops at the last column, so a plain `expand` pushes the border out), and a capture taken before the first draw is blank.

The first view, for reference (66×40, one item waiting):

```text
desk ● 1 waiting · forgectl dev · laptop.local · …  up <1m · 09:30
◌ 1 waiting · oldest 4m · 0 started today · ✓ 0 ✗ 0
╭ queue ─┤ ● 0 running  ◌ 1 waiting ├────────────────────────────╮
│ ▸ ◌ waiting  01 merge-1201                 ────────  needs you │
╰────────────────────────────────────────────────────────────────╯
╭ focus ─────────────────────────────────────────────────────────╮
│ 01 merge-1201 · sha256 24cf0bd3b113 · unchanged since queued … │
│ what  Merge the release branch into main                       │
│ why   CI is green and the review is posted at head             │
│ ┆ script · the first lines it runs                             │
│ ┆ echo "merging the release branch"                            │
│ ┆ sleep 1                                                      │
│ ┆ echo "done"                                                  │
╰────────────────────────────────────────────────────────────────╯
 y run · s skip · v view · a all · t timeline · ? help · q quit
```

## What holds

- **The short sha256 is whole at every width** from 40 to 120 columns; what and why wrap.
- **`y` is refused when the focus panel does not fit.** At 8 and 9 rows the item stayed in `pending/` after `y`, and the footer stops offering `y`.
- **`NO_COLOR` and `--no-icons` lose no state.** Every state is a word as well as a color and a glyph.
- **The empty state says what to do:** "Claude queues scripts with forgectl desk add; they appear here".
- **A lost run explains itself** in the focus panel: what happened, that it may have partly run, and the two keys that help.
- **The timeline reads in plain words** ("ran ok in 0:45", "failed · exit 1 after 0:00") and groups by day.
- **Piped, the dashboard refuses with a next step** and exit 2, and writes no escape sequences.

## Findings

### 1. `a` runs items in a window where `y` refuses

**Rank:** 1 (safety). Seen at 66×8 and 120×8, and in the code.

`y` runs an item only when its short sha256, what and why are all on screen. `a` has a different test: the full sha256 of each item must fit. So in an 8-row window `y` is refused, but `a` opens a prompt that offers `y run` with the name and hash and no what or why.

```text
desk ● 1 waiting · forgectl dev · laptop.local · …  up <1m · 09:32
too small: make it 10 rows tall
╭ queue ─┤ ● 0 running  ◌ 1 waiting ├────────────────────────────╮
│ ▸ ◌ waiting  01 merge-1201                 ────────  needs you │
╰────────────────────────────────────────────────────────────────╯
 not run: enlarge the window to see 01 merge-1201's sha256, what …
```

```text
desk ● 1 waiting · forgectl dev · laptop.local · …  up <1m · 09:32
dashboard hidden while you confirm
 run these 1?  y run · any other key cancels
  01 merge-1201
    sha256 24cf0bd3b1139096f5bada95e9133ff1
           8594c79b490dd698e814a7a81dc907d1
```

The prompt was cancelled, not accepted; that `y` in it runs the item is read from `askAll` in `internal/tui/desk_model_unix.go`, which checks only that the hashes fit.

**Cost:** the two run keys enforce different checks, and the weaker one is the bulk one.

**Direction:** `a` uses the same test as `y`: unavailable, and absent from the footer, when the focus panel does not fit. This removes a path and adds nothing.

### 2. After `y` the cursor stays on the item that ran

**Rank:** 2 (friction). Seen at 66×40 and 120×40.

A started item moves below the waiting ones, and the cursor follows it there. The next waiting item is now at the top.

```text
╭ queue ─┤ ● 1 running  ◌ 6 waiting  ⌨ 1 tty ├───────────────────╮
│   ◌ waiting  02 bump-tap                   ────────  needs you │
│   ◌ waiting  03 fail-step                  ────────  needs you │
│   ◌ waiting  04 sleeper                    ────────  needs you │
│   ◔ held     05 held                       ────────  held      │
│   ◌ waiting  06 sudo-step                ⌨ ────────  tty       │
│   ◌ waiting  08 nightly                    ────────  needs you │
│   ◌ waiting  09 broken                     ────────  needs you │
│ ▸ ● running  01 merge-1201                 ░░░░░░░░  0:00      │
│   ! changed  07 edited                     ────────  changed   │
╰────────────────────────────────────────────────────────────────╯
╭ focus ─────────────────────────────────────────────────────────╮
```

After a skip the cursor moves to the next waiting item, so the two keys behave differently:

```text
╭ queue ─┤ ● 0 running  ◌ 6 waiting  ⌨ 1 tty ├───────────────────╮
│ ▸ ◌ waiting  02 bump-tap                   ────────  needs you │
│   ◌ waiting  03 fail-step                  ────────  needs you │
│   ◌ waiting  04 sleeper                    ────────  needs you │
 skipped 01 merge-1201 · u to undo
```

**Cost:** approving k items one at a time costs `y` plus up to n-1 presses of `k` each time. With seven waiting, the second approval is 7 moves away.

**Direction:** after `y` the cursor goes to the next waiting item, as after `s`. Ruled: yes (§ Rulings). The "selection moved" guard already has rules for where the cursor lands after an action.

### 3. `desk ui` lists scripts but cannot run them

**Rank:** 3 (friction). Seen at 66×40 and 120×40.

`desk ui` is described as "asks and scripts in one live TUI". It shows a waiting script and its full sha256, then tells the operator to go elsewhere:

```text
◆ desk  q quit · ? help · 2 now · 1 soon · 1 optional · 1 coming ·
Now
  Approve the new deploy key  <1m
  Sign in to the package registry on the bui…  <1m
When you have 10 minutes
  Check that the phone still gets the test n…  <1m
Optional
  Pick a name for the new plugin  <1m
Coming
  Renew the developer certificate next month  <1m
Scripts (run from the desk dashboard)
▸ 01-merge-1201  Merge the release branc…  waiting ae83919dadd5
──────────────────────────────────────────────────────────────────
01-merge-1201
Merge the release branch into main
sha256 ae83919dadd5f8b03e4c3c5ec0d7d3ee98a45dfa9dd7e73571a0c9a136a
```

The dashboard, in turn, shows no asks. An operator with one ask and one script waiting needs both programs.

**Cost:** a second program to approve something already on screen.

**Direction:** one surface. Either the dashboard lists asks, or `desk ui` runs scripts under the same test as the dashboard. Filed as a design issue; the surviving surface is not picked.

### 4. The run-all confirm is 64 hex characters per item, with no what

**Rank:** 4 (friction). Seen at 66×40 and 120×8.

```text
 run these 6?  y run · any other key cancels
  01 merge-1201
    sha256 24cf0bd3b1139096f5bada95e9133ff1
           8594c79b490dd698e814a7a81dc907d1
  02 bump-tap
    sha256 0c8a38ccd21ef1ed8b8a1043016feb53
```

For the six runnable fixture items the prompt lists 384 hex characters and no what. The dashboard's own check is 12 characters next to the what and why.

**Cost:** nobody compares 384 characters, so the prompt is an extra screen that checks nothing.

**Direction:** ruled: remove `a` once the cursor advances after `y` (§ Rulings). The options weighed were dropping `a`, letting it run only items whose focus panel had been on screen, and a shorter prompt; only the first removes a screen.

### 5. Skip, clear and done are confirmed though they can be undone

**Rank:** 5 (friction). Seen at 66×40 and 120×40.

```text
 skip 01 merge-1201?  y skip · any other key cancels
```

```text
 clear lost run 10 long-wait?  y clear · any other key cancels
```

```text
mark done? d or y confirms, any other key cancels
```

A skip can be undone with `u` ("skipped 01 merge-1201 · u to undo", then "returned 01 merge-1201 to the queue"), and it is still confirmed first.

**Cost:** one extra key on a routine action that already has an undo.

**Direction:** skip acts at once and keeps `u`. Clearing a lost run and marking an ask done need the same look: confirm only where there is no undo.

### 6. A failed run shows no reason on the dashboard

**Rank:** 6 (friction). Seen at 66×40 and 120×40.

```text
│ 09 broken · sha256 dd6f22a28d72 · exit 1 · 0:00 · ended 2m ago │
│ what  Checks with a failing test                               │
│ why   To see a failed step in the run view                     │
│ ┆ l to view the log                                            │
╰────────────────────────────────────────────────────────────────╯
```

The fixture's log holds one line that says why ("error: CHANGELOG.md has no 0.69.0 section"). The focus panel has room for it and shows a hint to open another screen. For a failed batch the failed step's name is also only in the run view.

**Cost:** one extra screen for the first question after any failure.

**Direction:** the focus panel of a failed run shows the last lines of its log, and for a batch the failed step, in the room the script preview uses for a waiting item.

### 7. `desk ui` opens with the cursor on a read-only row

**Rank:** 7 (friction). Seen at 66×40 and 120×40.

With five asks and one script, the cursor starts on the script at the bottom. Every answer key then refuses:

```text
Read only here: run or skip it from the desk dashboard (forgectl d
select an ask first (scripts run from the desk dashboard)
↑/↓ select · enter details · d done · r reply · l link · s snooze…
```

**Cost:** one move (`g`) before any ask can be answered, and a refusal if the operator does not notice.

**Direction:** start on the first ask in "Now".

### 8. A changed item says so but shows no difference

**Rank:** 8 (friction). Seen at 66×40 and 120×40.

```text
╭ focus ─────────────────────────────────────────────────────────╮
│ 07 edited · changed                                            │
│ queued 62cf21e49116, now af63ee21c8ae · not run, moved to      │
│ skipped · ask Claude to queue it again                         │
│ what  Rotate the deploy key                                    │
│ why   The old key expires on Friday                            │
╰────────────────────────────────────────────────────────────────╯
```

`v` shows the bytes as they are now, with nothing marking the added line:

```text
script · 07 edited  1-5/5
#!/bin/sh
# WHAT: Rotate the deploy key
# WHY: The old key expires on Friday
echo "rotate the deploy key"
echo "and also print the key"
```

**Cost:** the operator learns that the item changed and has to find out how by hand.

**Direction:** not a diff of the old bytes (they are gone), but the focus panel can say how the size changed ("was 4 lines, now 5"), and the message already names the right next step.

### 9. The queue hides rows with no "more" marker

**Rank:** 9 (lost work). Seen at 66×12 and 66×20.

Nine items, two visible, nothing says seven are hidden:

```text
╭ queue ─┤ ● 0 running  ◌ 7 waiting  ⌨ 1 tty ├───────────────────╮
│ ▸ ◌ waiting  01 merge-1201                 ────────  needs you │
│   ◌ waiting  02 bump-tap                   ────────  needs you │
╰────────────────────────────────────────────────────────────────╯
```

At 20 rows, five of nine:

```text
╭ queue ─┤ ● 0 running  ◌ 7 waiting  ⌨ 1 tty ├───────────────────╮
│ ▸ ◌ waiting  01 merge-1201                 ────────  needs you │
│   ◌ waiting  02 bump-tap                   ────────  needs you │
│   ◌ waiting  03 fail-step                  ────────  needs you │
│   ◌ waiting  04 sleeper                    ────────  needs you │
│   ◔ held     05 held                       ────────  held      │
╰────────────────────────────────────────────────────────────────╯
```

The tab's "7 waiting" is the only clue. The timeline panel does mark its own overflow ("… 3 more · t to open").

**Cost:** a held, changed or lost item below the fold is not seen.

**Direction:** the queue's last visible row becomes "… N more" as the timeline's does.

### 10. Footer hints: the wrong keys are dropped, labels differ, and one hint is dead

**Rank:** 10 (friction). The footer cases were seen at 40, 60 and 66 columns; that Enter does nothing on the dashboard is read from the code (`"enter"` is handled only in the timeline view), since Enter was never pressed.

At 40 columns the footer keeps run-all and drops skip and view:

```text
 y run · a all · ? help · q quit
```

At 60 it drops the timeline. On a busy desk at 66 the same key is "r runs" in the footer and "open the run view" in the key list:

```text
 y run · s skip · v view · a all · r runs · ? help · q quit
```

The dashboard's timeline panel says Enter does something. It does not on the dashboard; the key works only after `t` opens the timeline:

```text
╭ timeline ──────────────────────────────────────────────────────╮
│  Needs you · enter takes you to it ──────────────────────── 1  │
│      4m  ◌ Merge the release branch into main                  │
│            waiting for you · queued 4m ago                     │
╰────────────────────────────────────────────────────────────────╯
```

**Cost:** the narrow footer offers the bulk action over reading the script, and the Enter hint sends the operator to a key that does nothing.

**Direction:** order the hint priorities so `v` and `s` outlive `a`; one label per key; drop the Enter hint from the dashboard panel or make Enter select the item there.

### 11. `desk ui` cuts lines at the right edge with no marker

**Rank:** 11 (lost facts). Seen at 66 and 120 columns.

The full sha256 is cut at both widths, with no sign that characters are missing:

```text
▸ 01-merge-1201  Merge the release branc…  waiting ae83919dadd5
──────────────────────────────────────────────────────────────────
01-merge-1201
Merge the release branch into main
sha256 ae83919dadd5f8b03e4c3c5ec0d7d3ee98a45dfa9dd7e73571a0c9a136a
```

```text
  Approve the new deploy key  <1m                                  │ Merge the release branch into main
  Sign in to the package registry on the bui…  <1m                 │ sha256 ae83919dadd5f8b03e4c3c5ec0d7d3ee98a45dfa9dd7
When you have 10 minutes                                           │ state  waiting
  Check that the phone still gets the test n…  <1m                 │
```

At 66 columns the snooze prompt hides its fourth choice; at 120 it is "4 one week":

```text
snooze for: 1 one hour · 2 four hours · 3 until 9:00 tomorrow · 4
```

```text
snooze for: 1 one hour · 2 four hours · 3 until 9:00 tomorrow · 4 one week · other key cancels
```

**Cost:** a hash that looks whole and is not, and a choice the operator cannot see.

**Direction:** wrap or mark every cut. A hash is wrapped, never cut.

### 12. Overlays break words in the middle

**Rank:** 12 (polish). Seen at 66 columns; no wrap needed at 120.

```text
a    run every waiting item on screen (asks first, listing each fu
↳ ll sha256)
```

```text
# WHY: The release is tagged and the tap still points at 0.68.2; e
↳ very machine that runs brew upgrade tonight stays on the old bui
↳ ld until this lands
```

**Direction:** wrap at spaces in the key list. The script view should keep exact bytes, so there the mark is right and only the key list changes.

### 13. The header repeats the count and spends its width on version and host

**Rank:** 13 (polish). Seen at 60, 66 and 120 columns.

```text
desk ● 7 waiting · forgectl dev · laptop.local · …  up <1m · 09:41
◌ 7 waiting · oldest <1m · 0 started today · ✓ 0 ✗ 0 · 1 changed
╭ queue ─┤ ● 0 running  ◌ 7 waiting  ⌨ 1 tty ├───────────────────╮
```

"7 waiting" appears three times in three rows. The header's `●` means waiting; in the queue tab `●` means running and `◌` means waiting. The version and host name take the room, so the desk path is cut to "…" and, in the focus panel, "unchanged since queued" loses its age at the default width.

**Direction:** one count; one glyph per state; version and host out of the header; the hash on its own row so the age is never cut.

### 14. The timeline panel repeats the queue in the opposite order

**Rank:** 14 (polish). Seen at 66×40 and 120×40.

```text
│ ▸ ◌ waiting  01 merge-1201                 ────────  needs you │
│   ◌ waiting  02 bump-tap                   ────────  needs you │
│   ◌ waiting  03 fail-step                  ────────  needs you │
│   ◌ waiting  04 sleeper                    ────────  needs you │
│   ◔ held     05 held                       ────────  held      │
│   ◌ waiting  06 sudo-step                ⌨ ────────  tty       │
│   ◌ waiting  08 nightly                    ────────  needs you │
│   ◌ waiting  09 broken                     ────────  needs you │
│   ! changed  07 edited                     ────────  changed   │
```

```text
│  Needs you · enter takes you to it ──────────────────────── 7  │
│     <1m  ◌ Checks with a failing test                          │
│          │ waiting for you · queued <1m ago                    │
│     <1m  ◌ Nightly checks, then publish                        │
│          │ waiting for you · queued <1m ago                    │
│     <1m  ◌ Reload the launch agent                             │
│          │ waiting for you · queued <1m ago · runs in the de…  │
```

On a desk with only waiting items the panel lists the same items as the queue, newest first where the queue is oldest first, in two rows each.

**Direction:** the panel earns its room when it shows what the queue cannot (finished and skipped items). With nothing finished it can collapse to its one-line form.

### 15. The run view's events are raw machine lines

**Rank:** 15 (polish). Seen at 66×40; the layout is covered by #1250.

```text
run 09 broken  ✗ exit 1  7 events  run 1 of 6
 test failed · exit 1
 ✓ lint 0s → – publish
 ✗ test 0s
 events
   #1    RUN-START id=09-broken jobs=4 pid=33261 steps=3
   #2    STEP-START lint deps=
   #3    STEP-START test deps=
   #4    STEP-END lint dur=0.0 log=/private/var/folders/xx/xxxxxx…
   #5    STEP-FAIL test dur=0.0 log=/private/var/folders/xx/xxxxxx…
   #6    STEP-SKIP publish reason=dep-failed:test
 ▸ #7    RUN-END failed=1 ok=1 rc=1 reason=failed skipped=1
```

The step lines above are clear. The events below are key=value lines whose log paths are cut mid-way. The `o` key promises plain words only when a lens rewrote the lines.

**Direction:** the timeline's wording ("test failed after 0s", "publish skipped: test failed") for desk runs, raw lines behind `o`.

### 16. Plain output disagrees with the dashboard on words and facts

**Rank:** 16 (polish). Posted as a comment on #1095, not a new issue.

A failed run is "failed" on the dashboard and `state=done` in `desk status`:

```text
name=09-broken
state=done
kind=batch
```

A waiting item's status prints an events path under `done/` that does not exist:

```text
name=02-bump-tap
state=waiting
kind=script
what=Bump the Homebrew tap formula to 0.69.0 and push it to the tap repository so brew upgrade picks up the release today
why=The release is tagged and the tap still points at 0.68.2; every machine that runs brew upgrade tonight stays on the old build until this lands
tty=false
sha256=0c8a38ccd21ef1ed8b8a1043016feb533b4dc38e5d056f709a8786e9929a11eb
added=2026-11-22T15:41:12Z
events=<scratch>/fx/desk-run/done/02-bump-tap.events
```

`desk ui` without a terminal answers with the dashboard's message: it names the dashboard and points at `--frame` and `desk status`, where its own `TERM=dumb` message correctly says `desk ui --frame` and `desk asks`:

```text

   ERROR

  Desk: the dashboard needs a terminal on stdin and stdout; use --frame for one frame, or desk status.

```

### 17. `desk ui` smaller items

**Rank:** 17 (polish). Seen at 66×40.

- The footer offers `d r l s` for an ask that allows only some of them; the refusal comes after the key:

```text
this ask does not take done
↑/↓ select · enter details · d done · r reply · l link · s snooze…
```

- Steps are shown as raw markdown (`**publish**`, backticks):

```text
Approve the new deploy key
ask-21028f39 · now · from fixture
1. Open the registry settings page.
2. Create a token with **publish** scope only.
3. Paste it into the release secret named `NPM_TOKEN`.
The old token expires on Friday.
```

- Titles are cut at about 42 of 66 columns while the rest of the row is empty.
- It names items "01-merge-1201" where the dashboard writes "01 merge-1201", and uses a different header and no panels, so the two programs do not look like one tool.

### 18. The refusal message is cut at 66 columns

**Rank:** 18 (polish). Seen at 66×8 and 66×9.

```text
 not run: enlarge the window to see 01 merge-1201's sha256, what …
```

**Direction:** a shorter sentence that fits 40 columns: "not run: window too short to show the sha256".

## Known issues touched

- **#1202** (open; `--what` truncated, no order between items): the queue row shows the file name and an unlabeled bar, not the what. Commented on 2026-11-22.

```text
│ ▸ ◌ waiting  01 merge-1201                 ────────  needs you │
│   ◌ waiting  02 bump-tap                   ────────  needs you │
```

- **#1152** (open; `TERM=dumb` garbles the dashboard): no longer true for `desk` or `desk ui` on `main`. Both exit 2 with "TERM=dumb cannot draw a full-screen view; use desk --frame for one frame, or desk status." (seen in tmux at 100×20 and with no terminal). Commented on 2026-11-22; the issue also names other TUIs, which this review did not run.
- **#1095** (open): finding 16.
- **#1250** (open): finding 15's layout half.
- **#1098, #1158** (closed): re-checked and still fixed. `?` opens a key list in the run view.

## Checklist walk

### TUI review checklist

The terminal-capability rows come from a probe that ran both programs in a pseudo-terminal under seven conditions plus resize and Ctrl-C. Its two reported oddities were re-run in tmux: one stood (finding 16's wrong `desk ui` message) and one did not (a "`-- frame`" typo that was a line wrap in the probe's capture).

| Item | Verdict | Evidence |
| --- | --- | --- |
| Core task is acting on visible state | pass | the dashboard |
| Every action reachable without the TUI | fail | no CLI verb runs an item; `skip`, `respond` exist |
| A plain mode exists | pass | `--frame`, `status`, `runs`, `show` |
| State, actions, last result on one screen | pass | footer shows "started 01 merge-1201" |
| `?` lists the keys for the view | pass | dashboard, run view, `desk ui` |
| Footer built from the binding table | fail | finding 10 |
| Core keys follow convention | pass | `q`, `j/k`, arrows |
| No key changes meaning in a sub-view | fail | `l` is log, replay step, and link; `s` is skip and snooze |
| Keys can be remapped | not checked | - |
| Current mode on screen | pass | "dashboard hidden while you confirm" |
| Destructive actions undone or confirmed | pass | - |
| Routine reversible actions not confirmed | fail | finding 5 |
| Every key answers within 100 ms | pass | every refused key prints why |
| Empty, waiting, error states say what next | fail | finding 6; empty and lost pass |
| Too-small message at the minimum size | pass | "too small: make it 10 rows tall" |
| Piped stdout: plain or a refusal | pass | exit 2, no escapes |
| No state by color alone | pass | `NO_COLOR` frames |
| Focus always visible | pass | `▸` in every list |
| Quit works on a terminal that answers no queries | pass | exits within 0.01 s of `q`, both programs |
| Modes restored on quit and on Ctrl-C | pass | none left set |
| `NO_COLOR` removes color | pass | 662 color escapes drop to 0 (dashboard), 65 to 0 (`desk ui`) |
| `TERM=dumb` gets a refusal | pass | exit 2 and a next step; see #1152 above |
| Resize redraws cleanly | pass | 120×40 to 30×8 and back |
| No terminal at all | pass | exit 2; `desk ui` uses the dashboard's message (finding 16) |
| First-frame time, light theme, grapheme width, tmux/SSH behavior | not checked | need a real terminal |

### UX review checklist

| Item | Verdict | Evidence |
| --- | --- | --- |
| Surface named, density matches | pass | a tool surface |
| Squint test: primary is obvious | fail | finding 13: three counts, two glyph meanings |
| Keyboard alone completes the task | pass | - |
| Each state present | fail | error state lacks the why (finding 6) |
| Error copy says what to do | pass | refusals name the next step; finding 18 cuts one |
| Input survives an error | pass | a refused `y` changes nothing |
| Notifications tiered | not applicable | not rendered in this review |
| Lists capped before disclosure | fail | finding 9: capped with no disclosure |
| One primary action per view | pass | `y` |
| Verdict scannable in 5 seconds | fail | findings 13 and 14 |
| Reduced motion | not checked | the running bar animates; no switch was looked for |

### Design-system constraints

| Item | Verdict | Evidence |
| --- | --- | --- |
| Two semantic colors for decoration | pass | amber for "needs you", muted teal for labels; status colors are status |
| At most one primary action | pass | - |
| Lists of 5 to 7 before grouping | fail | nine flat queue rows; the timeline groups by day |
| Status without color alone | pass | - |
| Literal, direct copy | pass | "not run", "ask Claude to queue it again" |
| Success is a quiet receipt | pass | "01 merge-1201 ended: ok" |

## Options for the first view

Three directions for the waiting-item screen at 66 columns were sketched for the operator on 2026-11-22: **trim** (same layout; one count, hash on its own row, what in the queue row), **approval card** (what, why, hash, script in reading order, queue below), and **queue-led** (each row carries its own what, why and hash, no focus panel). Trim fixes findings 10, 13 and the #1202 comment without touching the layout the #1098 guard depends on. A fourth sketch, a richer run-all confirm, was withdrawn: it added reading to a second screen.

## Notifier observations

Posted as a comment on #1476 on 2026-11-22, for the session working on clickable notifications.

- With both `[desk]` notify settings off and no herdr session in the environment, `desk add`, `desk ask`, `y`, `s` and `u` called no notifier program in about 30 adds and 10 runs.
- A notification click will land on the first view above. What the operator needs there is the selected item to be the one the notification named; the dashboard opens with the cursor on the first row, which is the oldest waiting item, not the newest.
- `desk ui` opens on the read-only script row (finding 7), so a click that lands there starts with a refusal.

## Not covered

- `desk serve` and `desk layout` were not run. `desk layout` splits the live terminal tab; `desk serve` is a web page.
- No real terminal was used: light themes, a 16-color terminal, and a screen reader are unchecked.
- Enter was never pressed, so the timeline's "enter review to run" path is read from code, not seen.
