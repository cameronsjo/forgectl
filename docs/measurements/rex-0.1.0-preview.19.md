# Rex CLI measurement

Measured on 2026-11-14 by a scripted run of Rex Beta `0.1.0-preview.19` (commit `ad5a836c`, `api_revision` 14): one detached scratch session, created and killed.
`[RAN]` means observed; `[HELP]` means inferred from help text, `api describe` or docs. The tags mark sections and bullets where the source run distinguished them; a table row or bullet without a tag was not tagged by the run, so treat it as unverified until re-run.
Raw captures were not kept.
The contract mapping in section 7 is inferred from the adapter task, not checked against `internal/surface/hcadapter` and the `backend.Adapter` contract, so check it there before building.

The CLI is the `rex` binary inside the Rex app bundle's helper (`Contents/Helpers/Rex Server.rex/Contents/MacOS/rex`); it is not on `PATH`.

## 1. Identity [RAN]

`whoami --json` returns `principal`, `account`, `endpoint`, `pid`, `instance_id`, `host_id`, `started_at`, `rex_version`, `api_revision`, `exit_empty`, `session_count` and `tailscale`.
`instance_id` is stable per server process.
`rex version` is plain text only.

## 2. IDs and labels

- IDs are typed strings: `session:<uuidv7>`, `window:<uuidv7>`, `block:<uuidv7>` [RAN].
- A target accepts a full ID, a label, or an ID suffix [RAN].
- A duplicated label fails with rc 3 (`block target "x" is ambiguous`) plus candidate lines [RAN]. Labels are not unique, so always pass full IDs.
- A block's default label is `shell`. Block labels are capped at 128 characters, no control characters [HELP].

## 3. Verbs

| Verb | Argv | Result | rc |
| --- | --- | --- | --- |
| `new` | `new LABEL -c CWD --window main --json -- sleep 120` | `{session_id, initial_windows:[{window_id, block_ids[], revision}]}`; detached, no app window opened | 0 |
| `ls` | `ls --json` | `{sessions:[{session_id,label}]}`; empty is `{"sessions": []}` | 0 |
| `session inspect` | `session inspect S --json` | `{revision, session_id, label, active_window_id, windows[], clients[]}` | 0 |
| `window ls` | `window ls -s S --json` | as session inspect without `clients` | 0 |
| `block ls` | `block ls -s S --json` | `{blocks:[{block_id,label,creator_name,flavor,host_id,placement,window_id,owner?}]}` | 0 |
| `block inspect` | `block inspect -s S B --json` | `{session_id, block, api:{events,methods}}`, about 75 KB of schemas | 0 |
| `block call` | `block call -s S -b B METHOD` | one compact JSON line | 0; unknown method 1 |
| `run` | `run -s S -c CWD --label L --focus=false --keep-open --json -- CMD` | `{window_id, block_ids[], revision}` | 0 |
| `send` / `send-key` | `send -s S -b B 'text'` | no output | 0 |
| `capture` | `capture -s S -b B [--trim] [--format text\|html\|vt]` | plain text | 0 |
| `wait` | `wait -s S --for 5s --json B` | `{"exit_code":N,"error":"exit status N"}` | the child's exit code |
| `block rename` | `block rename -s S <block-id> LABEL` | no output (block is positional; `-b` gives rc 2) | 0 |
| `block close` | `block close -s S B` | no output | 0; repeat 3 |
| `kill` | `kill SESSION` | no output | 0; repeat 3 |

Notes:

- `session inspect` `revision` increases only on layout or label changes, not on status changes: it is not a heartbeat. `clients[]` includes the person's own Rex Beta app client.
- `block inspect` is a capability probe only. Terminal methods include `process`, `program_status`, `title`, `pwd`, `modes`, `write`; events include `child_exited`, `process_changed`, `program_status_changed`, `program_status_removed`, `progress_report`, `desktop_notification`.
- `block call process` while running: `{last_exit:null, child:{pid,user,name,path,argv0,cwd}, foreground:{...}, home}`. After exit: `{last_exit:{exit_code:N,error:"exit status N"}, home}` with no `child`; exit 0 has no `error`.
- `run`: `.block_ids[0]` is the full block ID; `--label` shows in `block ls` at once; `-c` sets the cwd; **a nonexistent `-c` is not an error** (rc 0, the block starts in `$HOME`); **there is no env flag** (`FOO=x rex run -- sh -c 'echo $FOO'` printed an empty value); `run` with no command is rc 2. With `--wait` the JSON is printed, then the CLI exits with the child's code.
- `capture` returns screen and scrollback in one stream; `--trim` did not strip the final whitespace-only line, so strip when comparing.
- `send` passes ESC bytes through: an OSC 7501 sequence typed into a `cat` block was parsed as OSC.

Errors (stderr `Error: <text>`):

| Case | rc | Message |
| --- | --- | --- |
| Missing session | 3 | `session target "x" not found` |
| Missing or closed block | 3 | `block target "..." not found` |
| `wait` on a missing target | 3 | `block or window target "..." not found` |
| Ambiguous label | 3 | `... is ambiguous` plus candidates |
| `wait` timed out | 1 | `wait timed out after 3s: context deadline exceeded` |
| Usage error | 2 | `unknown shorthand flag` and similar |

rc 3 collides with a child that exits 3, and rc 1 with a timeout: disambiguate with `--json` stdout, stderr text, or `block call process`.

`events` prints newline-delimited JSON `{type:"session_event", payload:{session_id, event:{type,payload}}}`.
Types seen: `client_connected`, `client_disconnected`, `block_event`, `block_closed`, `window_closed`, `active_window_changed`, `session_view_changed`.
Every CLI call appears as a connect/disconnect pair, so the stream is noisy.
It ignores SIGALRM: stop it with SIGTERM.

## 4. Block lifecycle [RAN]

- **Natural exit without `--keep-open`:** the block is gone from `block ls`, and `inspect`, `capture`, `send` and `wait` on it return rc 3. Its exit status is readable only by a `wait` already running, or by using `--keep-open`.
- **Quick failure** (`sh -c 'exit 7'`) stays open without `--keep-open`, with `last_exit` 7, matching the documented `quick_exit_threshold_ms` of 250 ms.

## 5. Owner marker

- The best candidate is the **block label** (`run --label`, or `block rename`), read back from `block ls`. `:` and `=` are accepted. It is not unique, a person can rename it, and it is capped at 128 characters.
- The session label is also readable in `ls --json`; duplicate session labels were not tested.
- The `owner` field is **not** a marker: it is a client ID tied to terminal-size ownership, absent on `run` blocks, and CLI client IDs change on every call.
- No per-block metadata API was found in `api list`.

## 6. OSC 7501 program status [RAN]

Sending `state=working:id=measure:app=measure:msg=aGk=` into a block, then `block call program_status`:

```json
{"records":[{"id":"measure","state":"working","app":"measure","effective_app":"measure","msg":"hi","updated_at":"<rfc3339>","owner":{"pid":62546,"name":"cat"}}]}
```

- It shows up **only** in `block call program_status` and in the `events` stream (`block_event` `program_status_changed`, payload `{id, record, reason:"report"}`). It is not in `block inspect`, `session inspect`, `window ls`, `block ls` or `block call title`.
- `msg` and `title` come back base64-decoded. Each report replaces its record entirely.
- `clear` with an id gave `{"records":[]}` and a `program_status_removed` event. `clear` with no id, parent ids, process-exit and shell-prompt rules are documented but not run.
- Any program in the pane can set a record: a status signal, not an ownership signal.

## 7. Candidate mapping onto the Adapter contract

| Contract piece | Rex candidate | Gap or risk |
| --- | --- | --- |
| Identity | `whoami --json`: require `rex_version`, `api_revision` in range, pin `instance_id` | a server restart changes `instance_id` |
| Start | find or create a session (`new`, detached), then `run -s <session_id> -c <cwd> --label <marker> --focus=false --keep-open --json -- <argv>`; keep `.block_ids[0]` | **no env flag**; a bad cwd silently becomes `$HOME`, so stat it first |
| Probe | `block ls` by full `block_id`, plus `block call process` (`last_exit` null means running) | a closed block vanishes: absence means closed or never existed |
| Exit status | `process.last_exit`, or `wait --json` | only with `--keep-open` or a quick-fail block; `wait`'s rc collides with not-found |
| Readiness | weak: poll `capture` for a marker, watch `events` for `child_exited`, or read OSC 7501 `idle`/`working` via `program_status` | program status is optional and forgeable |
| Owner marker | block label `forgectl:owner=<id>` | renameable, not unique, 128 characters |
| Close | `block close -s S <full-id>`; `kill SESSION` only for a session the adapter created | closing the last block closes its window |
| Parsers | objects are flat and stable; ignore unknown fields | `block inspect` is huge: do not parse it for state |

## 8. Not measured

The 250 ms quick-exit threshold timing, a `working` record cleared by process exit, raw `api call` error codes, non-ASCII output, very large scrollback, alternate-screen capture, and what forgectl's own `rex` calls do with an inherited `$REX_SERVER`.

Measured since: `$REX_SESSION`, `$REX_BLOCK`, `$REX_SERVER` and `$REX_BIN_DIR` inside a Rex terminal. Rex sets all four afresh in each block, including a block in a session created by `rex new` from inside another block (see `docs/hosts.md`).

## Design problems for a Rex backend

Two problems the adapter's plan has to solve, both from this measurement:

1. **Worker environment.** forgectl hands a worker its environment (`FORGECTL_DRAIN_WORKER` and the rest) and keeps the sealed bootstrap out of argv. `rex run` has no env flag, so the options are an `env K=V prog` wrapper (values then appear in argv and in `block call process`), `session.new_window` through `rex api call` with `initial_input` (reachable only that way), or a wrapper script file. The choice interacts with the sensitive-runner design and needs a decision before the adapter is built.
2. **Finished versus gone.** A closed block vanishes, so `Probe` cannot tell finished from never-existed unless the adapter always starts with `--keep-open`. The adapter's identity check also has only a renameable label to lean on; the block ID is a UUIDv7 (it carries a creation time) and `instance_id` plays the part of hc's core id, which is the same shape of defence `hcadapter` uses.
