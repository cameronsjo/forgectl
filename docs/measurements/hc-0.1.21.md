# hc 0.1.21 measurement

Measured on 2026-11-14 against `hc` 0.1.21 with a scripted run, to check the JSON shapes `internal/surface/hcadapter` parses (first measured on 0.1.15).
The run created one scratch terminal, then started, probed, read, keyed, killed and removed it.
No other terminal and no server was touched.

**Result: no breaking drift.** Every difference is additive, and the adapter's parsers ignore unknown fields.
`TestHc0121GetRecordWithUnknownFieldsParses` pins that with a 0.1.21-shaped `get --json` record.

## Verbs the adapter calls

| Verb | Argv measured | 0.1.21 result | Adapter reads |
| --- | --- | --- | --- |
| `server status` | `server status --json` | `core.id`, `core.name`, `version`, plus `finished`, `holder`, `holders`, `installed_by`, `keeps_running_after_logout`, `live`, `pid`, `uptime_ms` | `core.id` (the incarnation pin); the rest is ignored |
| `run` | `run --bg --json --outlive-parent --no-notify --name N --cwd C --label K=V -- CMD` | `{address, agent, cwd, name, parent, state, turn}` | `address` |
| `get` | `get --json -- ADDR` (also by name) | the full record, below | `address`, `created_ms`, `state`, `labels`, `status`, `turn` |
| `get`, missing | `get --json -- t99999` | rc 3, `{"error":{"kind":"not_found","hint","message"}}` | the error `kind` |
| `ls` | `ls --all --json --label K=V` | `{hidden_finished, terminals[]}`; a label with no match gives an empty list, rc 0 | `terminals[]` |
| `read` | `read --json --lines 200 -- ADDR` | `{complete, how, text}` | `complete`, `text` |
| `label` | `label --json -- ADDR K=V` | the full `get` record with the new label | not parsed |
| `send --if idle` | `send --json --if idle -- ADDR TEXT` | rc 10, `{"error":{"kind":"status_mismatch","hint":"nothing was typed",...}}` on a non-idle terminal | the error `kind` |
| `keys` | `keys --json -- ADDR esc` | `{status, turn}` | not parsed |
| `kill` | `kill --json -- ADDR` | the full record, `state` `finished`, `reason` `killed`, `signal` 1 | not parsed |
| `rm` | `rm --json -- ADDR` | `{"removed":"ADDR"}`; on a live terminal rc 1, kind `live`, hint "kill it first"; a second `rm` is rc 3 `not_found` | not parsed |
| `events` | `events --json` (5 s sample) | a snapshot line, then notifications; includes `heartbeat` | ignored by the scanner |

## `run --parent` (measured 2026-11-19)

Measured against hc 0.1.21 for `surface launch --parent` (forgectl#1455), with one scratch terminal started from a live terminal and removed afterwards.

| Argv measured | Result |
| --- | --- |
| `run --bg --json --no-notify --outlive-parent --parent <live addr> --name N --cwd C -- CMD` | rc 0; the record's `parent` is the given address, `get` shows `die_with_parent: false`, and herdr-app `ls` lists the child under the parent's `sub_agents` |
| `run --bg --json --no-notify --outlive-parent --parent t999zz --name N --cwd C -- true` | rc 3, `{"error":{"kind":"not_found","message":"no terminal \"t999zz\""}}`; no terminal named `N` exists afterwards |
| `run --bg --json --no-notify --outlive-parent --parent <live addr> --name N --cwd C -- "claude -n N"` (Claude Code 2.1.328), then `get` once idle | `title` is `N`. The same launch with plain `claude` reads `Claude Code`. herdr-app labels a terminal from this title (its `src/terminal.rs` `set_title`), so a worker started with `--name` shows its name |

## Additive fields

- Terminal record (`get`, `kill`, `label`): `reported`, `status_ms`, `screen`, `identity`, `foreground`, `parent`, `keep`, `reason`, `signal`, `title`.
- The `status` object gains `kind` (`permission`, `question` or `auth`, on `blocked` only), `progress` and `title`.
- `read` gains `how` and `why`; `ls` rows gain `agent` and `where`.
- herdr-app writes the labels `where`, `herdr.seen` and `herdr.tab` next to `forgectl.surface`. The adapter compares only `forgectl.surface`, so they do not matter.
- `events` has a `heartbeat` notification, and there are new error kinds that fall to the adapter's generic path.

## Still true

hc has no expected-identity guard on `kill`, `rm` or `send` (the debt noted in `close.go`), and 0.1.21's help offers none either (read from help output, not exercised).

## Not run

- The adapter and its tests against a real hc: the tests use 0.1.15-shaped fakes and cannot detect drift by themselves.
- `send` on a real agent, and `read` against a working full-screen agent.
- `status.kind` on a live blocked agent (the fixture is built from the documented shape).
- Whether adapter-launched workers get `parent: null`: the hand run did not use the adapter's `HC_TERMINAL_*` scrub.
- Terminal-name rules, core socket paths and `hc server restart` behavior, which the adapter's comments cite from 0.1.15.

## Open decision

Whether the adapter should parse `status.kind` and `read.why`. Nothing reads them today.
