# recipe — run small built-in workbench recipes (alias: `r`)

A recipe is a fixed sequence of other tools' calls, wired together because the
sequence itself is the fiddly part. `afk` is the only recipe today.

```bash
forgectl recipe afk                       # run /go:afk on the current Herdr agent, then /compact it
forgectl recipe afk --target w1:p2        # override the Herdr target; r afk is the same command group alias
forgectl recipe afk --compact-only        # skip the prompt step — for a caller that already journaled
forgectl recipe afk --rename parked       # /rename the session before compacting
forgectl recipe afk --skip-receipt        # do not read the pane back to confirm /compact ran
forgectl recipe afk --prompt /cadence:journaling  # submit a different slash command instead of /go:afk
```

## `afk` — journal and compact the current Herdr agent pane

`afk` exists so "wrap up before the usage block resets" is one command instead
of three manual steps through a terminal multiplexer.

**Target resolution.** `--target` wins if given. Otherwise `HERDR_PANE_ID`,
then `HERDR_ACTIVE_PANE_ID`. `HERDR_ACTIVE_PANE_ID` is herdr's *focused* pane
for the workspace, not necessarily the caller's own — it is a reasonable
default target, and a wrong answer to "is this me". No target from any source
exits `2`.

**Preflight.** Before any side effect, `afk` runs `herdr agent get <target>`.
That single call proves herdr is installed, the server answers, and the
target resolves — three things a bare `--help` probe cannot catch, and that
would otherwise surface only after `/journal` has already spent a model call.

**Steps, in order:**

1. `--prompt` — defaults to `/go:afk` (the `go` plugin), which pushes,
   opens a draft PR, updates the journal, and files loose ends — a superset
   of the bare `/journal` this recipe used to hardcode. Pass `--prompt` to
   submit a different slash command instead; skipped when `--compact-only`
   is set, for a caller that already journaled.
2. `/rename <name>` — only when `--rename` is given.
3. `/compact` — always runs.

Each step goes through `herdr agent prompt <target> <command>`. Every step but
the last waits (`--wait`) for the target to settle before the next one fires —
except on a self-target, below.

**Self-targeting.** `afk` can compact the very pane it is running in — the
main use case, a session wrapping itself up before its usage block expires.
Self-ness is decided from the preflight response, never from string-comparing
the two target values: only `HERDR_PANE_ID` can answer it (`HERDR_ACTIVE_PANE_ID`
answers "what's focused," not "is this me"), and a target given by agent name
is resolved to its canonical pane id first, so `--target reviewer` naming your
own pane is still recognized. A false negative here is the safe failure mode —
it keeps `--wait` and the receipt check on, at the cost of a deadlock the
operator can see, rather than submitting an unverified `/compact` into a
stranger's session.

On a self-target, `--wait` is dropped from every step: `agent prompt --wait`
waits for the agent to reach a settled state, and a process running this very
command is `working` and cannot settle until the command returns, so a
self-targeted `--wait` deadlocks the caller against itself. The cost: on a
self-target the steps are submitted back to back with nothing sequencing
them, so their order rests on the pane's own prompt queue, not on anything
`afk` observes. The receipt check (below) is skipped for the same reason —
`/compact` cannot run until this process's own turn ends, so any read taken
before that would necessarily predate it.

**Receipt check.** herdr's CLI exits `0` for any accepted request, which is
not the same claim as "the text ran." Before submitting, `afk` reads the pane
and remembers what's already on screen. After the steps, it polls the pane
(10 attempts, 2s apart) for one of `Compacting`, `Compacted`, or `Compact
summary` — but only a marker **absent from the pre-submission read**: a pane
compacted ten minutes ago still shows `Compacted` in its visible window, so
matching raw text would pass on every second run regardless of what this one
did. `--skip-receipt` turns this check off.

**`--rename` reaches the target pane as raw terminal bytes**, unescaped — so
it is allowlisted (ASCII letters, digits, space, `._-`, ≤128 runes), never
just length-checked. An unescaped control byte in a pane's input isn't a
label, it's a keystroke: an embedded `ESC` cancels the input box, `\x03`
interrupts the agent, and a literal end-of-paste marker closes the paste
region early so every following byte lands as keystrokes instead of text. A
`--rename` value is also refused if it contains one of the receipt markers
above — matching them would satisfy the receipt check without `/compact`
doing anything, breaking the two checks' independence.

**`--target` is allowlisted too** (letters, digits, `._-:/@`), and a leading
`-` is refused outright — not for injection (herdr never parses the target as
a flag), but so a malformed target fails with a legible error here instead of
a confusing herdr-side one after the first step has already run.

**`--prompt` is allowlisted the same way as `--rename`**, including the same
128-rune limit: it must start with `/`, and after that only ASCII letters,
digits, `:`, and `-` are allowed — covering plugin-namespaced commands like
`/go:afk` and `/cadence:journaling` — and it is refused if it contains one of
the receipt markers above, for the same independence reason. That check only
sees the flag's literal text, though: it cannot see what the named command
*prints*. A `--prompt` naming a command whose own output happens to render
one of the receipt markers can satisfy the receipt check without `/compact`
having run — the allowlist keeps the pane's input safe, not the correctness
of an arbitrary command's output.

`--prompt ""` is rejected, not silently replaced with the default: cobra
already fills the default when the flag is omitted, so an explicit empty
value only reaches validation when a caller typed `--prompt ""` on purpose,
and treating that as "use the default" would turn a caller's mistake into
`/go:afk`'s push-and-open-a-PR running on the resolved target pane.

### Flags

| Flag | Meaning |
| --- | --- |
| `--target` | Herdr agent name or pane id. Defaults to `HERDR_PANE_ID`, then `HERDR_ACTIVE_PANE_ID` |
| `--rename` | Run `/rename <name>` before `/compact` |
| `--prompt` | Slash command to submit as the first step. Defaults to `/go:afk` |
| `--compact-only` | Skip the prompt step, run `/compact` alone — for a caller that already journaled |
| `--skip-receipt` | Do not read the pane back to confirm `/compact` ran |

### Exit codes

`2` — no target resolves, an invalid `--target`, an invalid `--rename`, or an
invalid `--prompt`. Any other non-zero exit names the failing step (preflight,
a herdr call, or the receipt read) in its error text.
