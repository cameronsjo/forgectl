# 0014. Log lenses: teaching the run view to read an app's log

**Status: Proposed**

Date: 2026-10-07

## Context

ADR-0013 gave the desk a run view and a log source, and left the run spec out: a log reads as a flat timeline with no step state, until "a tool in use starts writing multi-step logs". It set the condition for a return: a real consumer, and a change to that decision.

The consumer is the operator. They want to point forgectl at an app's own log, which is usually plain text and never in forgectl's shape, and see what the process is doing (which phase it is in, what failed and why, whether it finished) instead of reading the log to trace it. Decoding a log is the attention the run view exists to save. The translation from one app's lines to that picture is per app and will mostly be written by Claude; the view must not be.

## Decision

### A lens is a TOML file per app

A lens says, for one app:

- **How a line splits**: `format = "json"` with the keys for event, step and time, or `format = "text"` with one RE2 pattern whose named groups fill them (any other group is a field). A text line the pattern does not match is an event named by the whole line, so a stack trace's continuation is kept, not lost.
- **What a line does**: ordered `[[rule]]`s, first match wins, each matching the event name or a field. A rule's action starts, closes, fails or skips a step, ends the run, `note`s the line (no step change), or `ignore`s it as noise. Its named groups fill the step, the exit and fields.
- **How a line reads**: a rule's `say` rewrites the event's name into plain words (`querying orders for customer {cid}`); the line as read stays in `@line`, one key away in the run view (`o`) and in `--events`.
- **Which steps exist**, optionally: `[[step]]` with `after` edges draws a graph. With none, a step is discovered the first time a rule acts on it, with no edges, because order of appearance is not dependency.

Lenses live in `<config dir>/forgectl/lenses/NAME.toml`, or any path `--lens` names. Parsing is strict: an unknown key, an action outside the list, a step rule with no step source, a `say` referring to nothing parseable, or an `after` naming no step is an error that names the rule to fix.

### The fold learns two things, the view none

`Spec` gains `ActionField` (the action is read from an event field the lens wrote at ingest, so classification stays in the source and the reducer stays pure) and `Discover` (an unknown step is added on first mention). Replay checkpoints clone the step map when discovering, so a replay before a step appeared does not show it.

Nothing about the view is per app. A lens produces the same `Event`s and `RunState` a desk run does, and the run view, `desk show` and `desk runs` draw it with no lens-specific code beyond counting ignored lines and hiding the lens's `@` bookkeeping in the timeline.

### A vocabulary for translators

When a pattern is not enough, a translator in any language turns the log into JSON lines carrying `event`, `step`, `time`, `action` and `exit`. The built-in lens `events` reads them: a JSON lens whose `[json] action` and `exit` keys let a line name its own action, deciding before any rule. An app can write the vocabulary itself. forgectl never runs a translator: it is piped into a file the log source reads, so forgectl stays a reader of files the operator names, as ADR-0013 set.

A line's own key starting with `@` is dropped at ingest, so a log cannot forge the action a lens assigns.

### The view leads with the answer

The run view's second line, and `desk show`'s, is the run's gist: the first failed step, when, and why (the failing line, in its `say` words), else how the run ended, else what is running and for how long. The flow and timeline are detail under it. The live view (`desk show --live`, for a desk item or a log) is the dashboard's run view alone, full screen, through the same code: a host model holds a desk model with no desk, so its keys, loads and replay are not a copy.

### Writing a lens is a loop

`desk lens check LENS --log FILE` reports each rule's hits, the rules that never matched, the steps found, and the most common unmatched events: what a new rule could claim. It is the loop a person or an agent writes a lens by. `desk lens list` names the directory and parses every lens in it.

## Alternatives considered

- **Port the parallel design's run spec.** Declined again, as in ADR-0013: it described one tool's run directory layout, not how to read a log.
- **Have forgectl run a translator command named in the lens.** Declined: a lens file would become code execution, and a config file that runs a program is a different threat (ADR-0012) for little gain over a pipe.
- **A plugin API (Go plugins, WASM) for parsers.** Declined: a lens covers the regular case in a file Claude can write and check, and a translator covers the rest in any language without a build.
- **Chain discovered steps in order of appearance.** Declined: arrows would claim a dependency the log never stated.

## Consequences

- `desk show` and `desk runs` take `--lens`; `desk show` takes `--live`; `desk lens list` and `desk lens check` are new verbs. JSON changes are additive (`counts.ignored`).
- `live` gains two values for a log: a lens that can end its run reports `live` until an end and `ended` after it, where every log was `unknown` before. Under ADR-0008 that is a new value in an existing field; a reader that treats an unknown `live` as "not a desk run" is unaffected. A lens with no end rule (and no `action` key) still reads `unknown`.
- A start after an end reopens a lens's run (the app restarted into the same log), and the live view keeps following a log after it ends. An ended desk run is still final.
- An end with no exit, from a lens, ends the run with its exit unknown rather than counting a bad exit; `exit = N` on an end rule records a fixed one.
- `say` text and every captured value pass through `termsafe` at ingest, like every other string a source reads.
- ADR-0013's "No run spec file" section is superseded by this ADR; the rest of 0013 stands.
