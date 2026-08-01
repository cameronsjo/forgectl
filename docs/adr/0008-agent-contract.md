# 0008 — Agent contract: every verb must be drivable without a TTY

- **Status:** Proposed (2026-07-31)
- **Context:** forgectl serves two audiences — humans at an interactive terminal and
  agents driving it via tool calls. A 2026-07-31 usage measurement showed agents probing
  the CLI rather than driving it, and a dogfood batch (#182–#202) filed four agent traps:
  an interactive picker reachable in `--dry-run` (#202), a swallowed escape (#200), a
  silent exit-0 on failure (#188, since fixed by #206), and invisible effective config
  (#189). Related: 0005 (module architecture); the 2026-07-31 active-use pass plan.

## Context

The launch-chain probe that grounds this contract (measured 2026-07-31 on the
maintainer's primary machine):

- Every cmux terminal pane launches Claude Code through a launcher script wired as the
  terminal's Claude binary path, and that script `exec`s **`forgectl launch`** with the
  real binary pinned via `FORGECTL_CLAUDE_BIN`. cmux appends its own `--session-id` and
  `--settings`. No shell function participates — the chain is bash, headless from
  forgectl's point of view.
- The interactive zsh wrapper survives only for hand-typed launches, and its
  args-passthrough branch launches without recording telemetry. The wrapper "going dark"
  in the measurement was therefore *unlogged passthrough plus the cmux chain*, not
  disuse.

Conclusion: forgectl already runs primarily in automated contexts — cmux automation,
agent tool calls, scripts. In those contexts an interactive prompt is not a UX choice;
it is a hang, a swallowed keystroke, or a phantom success. The measurement's traps are
all instances of one missing rule: **behavior keyed on assumptions about a human being
present**.

`--json` already ships in 12 CLI files and `resume --help` already documents an
agent/human split. This ADR codifies the practice as normative so new verbs inherit it
and the filed fixes (#188, #189, #200, #202) have a contract to conform to.

## Decision

Every forgectl verb MUST meet all of the following. These rules bind existing verbs
(the stabilization slate brings violators into conformance) and gate new ones at review.

1. **No TTY interaction without a TTY.** A verb MUST NOT open a picker, prompt, or
   confirmation unless stdin *and* stdout are TTYs. When input would be required and no
   TTY is present, the verb MUST print the candidate set (one per line, or `--json`)
   and exit non-zero. Flags that declare a non-interactive intent (`--dry-run`,
   `--json`, `--yes`, an explicit target argument) MUST suppress interaction even on a
   live TTY.
2. **`--json` on every list/status verb.** Any verb whose output enumerates or reports
   state MUST accept `--json` and emit a stable, documented shape. Human-format output
   MAY change freely; JSON shapes MUST only change additively.
3. **Honest exit codes.** Exit 0 MUST mean the verb did what its name says. Partial
   completion, a no-op where work was requested, or a swallowed cancel MUST exit
   non-zero with the reason on stderr. A user cancel (escape, ctrl-c, declined confirm)
   MUST be distinguishable from success.
4. **No hidden mode switches.** A verb MUST NOT silently change behavior based on
   environment sniffing beyond the TTY test above. Any mode a verb infers (profile,
   posture, workspace) MUST be printable — the effective configuration MUST be
   inspectable via a verb or flag (`config`-class visibility, #189), never
   reconstruction-from-source.
5. **Stable parseable output.** Diagnostics go to stderr; payload goes to stdout.
   stdout in non-JSON mode SHOULD still be line-oriented and grep-safe (no spinners,
   no ANSI when stdout is not a TTY, honoring `NO_COLOR`).

## Consequences

- The remaining agent-traps cluster (#202, #200, #189) implements to this contract; their
  filed fix shapes already agree (e.g. #202's print-candidates-not-picker). #188 landed
  ahead of this ADR and conforms to rule 3 retroactively.
- New-verb review gains a checklist: TTY-gated interaction, `--json`, exit-code
  honesty, config visibility, stream discipline.
- The surface-architecture design (whether the agent surface stays
  CLI-plus-contract or grows an MCP subcommand, #27) builds on this floor either way;
  the probe result above is an input to that design's launch question.
- Existing interactive UX for humans is unchanged — the contract narrows *when*
  interaction may occur, not what it looks like on a real TTY.
