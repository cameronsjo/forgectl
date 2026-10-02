# The `--json` stderr contract

[ADR-0008](adr/0008-agent-contract.md) rule 2 puts `--json` on every verb that reports state, and says what goes to stdout. This page covers the other stream: what a `--json` verb writes to stderr when it exits non-zero ([#862](https://github.com/cameronsjo/forgectl/issues/862)).

Under `--json`, forgectl never writes its human error frame (the styled `ERROR` block) to stderr. A non-zero exit takes one of two forms:

- **The verdict is already on stdout.** A verb that reports a problem as its result puts the full JSON verdict on stdout and exits with its documented code. It writes no error object to stderr, because the verdict already says what went wrong. Examples: `doctor` finding a failed check, `docs check` with error findings, `preflight` finding a misaligned project, `pr repair` finding unsettled sessions, `update` with a failed step, `projects list --strict` on a partial inventory, `status --strict` with a section that is not `ok`, and `launch stats` with skipped rows (the `skipped_rows` field).
- **The verb failed before it emitted anything.** stdout stays empty, and stderr gets exactly one JSON object.

A verb that streams several verdicts can hit both forms in one run. `pr drain --watch --json` writes one report per pass to stdout. If a later pass then fails before it can report, that pass's failure object goes to stderr, and the earlier reports stay on stdout.

`--json` never changes an exit code. The same failure exits with the same code with or without it.

Some failures happen before any verb starts: a `config.toml` that does not parse or cannot be read (exit 2), and an environment forgectl cannot resolve its directories from, such as an unset `$HOME` or a relative `$XDG_CONFIG_HOME` (exit 1). These follow the same rule. When the verb you ran declares `--json` and you passed it, stderr gets that verb's one failure object in place of the plain `forgectl: …` line: code `failed` for most verbs, and the family shapes below for `env check` and the docs verbs. `launch` declares no `--json`, because everything after `launch` goes to the harness, so a `launch` failure stays a plain line.

Some verbs also write documented progress or notes to stderr under `--json`: the `update` transcript, `herdr organize`'s human report, `projects list`'s per-host degradation notes, `sessions why`'s match count, and `tasks done`'s close record. Those lines are part of the verb's normal output. The contract only rules out the human error frame on top of them.

## The failure object

Most verbs use this shape:

```json
{"error": "<message>", "code": "<code>", "path": "<file>"}
```

All three keys are always present.

| `code` | When |
| --- | --- |
| `usage_error` | forgectl rejected a flag or positional argument before the verb ran: an unknown flag, a flag missing its value, or a wrong number of arguments. |
| `failed` | The verb ran and failed before it wrote its verdict, for example because a subprocess failed, a flag combination was refused, or the config is invalid. |

`path` is the one resolved file the failure is about, relative to the repository root, when there is one. Today only env-file refusals set it. Everywhere else it is `""`.

A flag error is reported as JSON even when `--json` comes after the bad flag (`--bogus --json`), because forgectl stopped parsing at `--bogus` and never reached `--json`. That scan skips a token that is the value of a flag it knows takes one, including after a bundled shorthand group such as `-vH`, where the last letter takes the next token when it names a flag that takes a value. In `projects list --host --json --bogus`, `--json` is the value of `--host`, so the error comes out as the human message. Once flags have parsed, `--json` counts only if it parsed as the `--json` flag. In `projects list --host --json a b`, `--json` is the value of `--host`, so the argument error comes out as the human message.

## Verbs with their own codes

Two verb families shipped their own shapes before this contract existed. Callers already parse them, so they are unchanged (ADR-0008: JSON shapes change only additively). A third, `tasks done`, uses the standard object with more codes; it has its own section below.

- **`env check`** uses the same `{"error","code","path"}` object with its own codes: `file_not_found` (exit 2) and `check_failed` (exit 1, used for usage errors too). See [commands/env.md](commands/env.md).
- **The `docs` verbs** (`list`, `check`, `search`) write `{"error","code","root"}`, where `code` is the integer exit code, not a string, and `root` names the docs root a failure stopped on. One case differs from the rule above: a partial `docs search` writes its full response to stdout **and** a code-1 object to stderr. If stdout fails partway through a `docs search` response, the part already written stays on stdout, truncated, next to a code-1 object on stderr whose `error` names the write failure rather than roots that could not be searched. `docs list` behaves the same way when stdout fails: part of its JSON array may already be on stdout, truncated, and stderr gets one code-1 object naming the write failure. See [commands/docs.md](commands/docs.md#exit-codes-and-errors).

## `tasks done`

`forgectl tasks done <id> --evidence <text> [--closer NAME] [--write-keychain-service NAME] --json` marks one task done.

On success it exits `0` and writes one object to stdout:

```json
{"id": 42, "project_id": 3, "title": "<title>", "done": true, "already_done": false, "evidence_recorded": true}
```

| Key | Meaning |
| --- | --- |
| `id`, `project_id` | The task that was closed and the project it is in. `id` is the global task id, not the `#N` the web UI shows. |
| `title` | The task's title, as the board holds it. It is board text: escape it before you print it. |
| `done` | `true` when the task is done, whether this call closed it or it already was. |
| `already_done` | `true` when the task was done before the call. Nothing was written. |
| `evidence_recorded` | `true` only when the task's description now ends with the closed-by line this call wrote. Always `false` when `already_done` is `true`. |

On failure it writes the standard `{"error","code","path"}` object to stderr, with `path` always `""` and one of these codes:

| `code` | Exit | When |
| --- | ---: | --- |
| `usage_error` | 1 | A bad argument, refused before any credential is read: no `--evidence`, an `<id>` that is not a positive number, evidence that is not one line of at most 300 characters or that holds something shaped like a token, `--keychain-service` (this verb reads `--write-keychain-service`), or a keychain service name outside 1 to 64 letters, digits, `.`, `_` and `-`. |
| `credential_missing` | 1 | The login keychain has no entry under `--write-keychain-service` (default `vikunja-write`). The verb never falls back to the read entry. Storing the entry is operator setup. |
| `not_found` | 1 | No task has that id, or this credential cannot see it. |
| `repeating_task` | 1 | The task repeats. This verb closes one-off tasks only. |
| `trailer_too_long` | 1 | The description plus the closed-by line is over the size this client sends. |
| `write_refused` | 1 | The update was sent and the server refused it. The task is unchanged. |
| `not_confirmed` | 1 | The update was sent and nothing read afterwards shows the task done. It may have been applied: read the task before retrying. |
| `unauthorized` | 3 | The server rejected the credential, on the first read or on the update. On the update it means the credential can read the task and may not change it; retrying does not help. |
| `failed` | 2, 4, or 1 | Anything else. Exit `2` when the instance could not be reached, `4` when the host is not one a keychain credential may be sent to, and `1` otherwise. |

The host rule covers every `tasks` verb that reads the keychain (`ls`, `show`, `ready`, `done`, and `mcp` over stdio). A keychain credential is sent only to the built-in default host or to a host listed in the user's `config.toml`:

```toml
[tasks]
allowed_hosts = ["<hostname>"]
```

Any other `--host` exits `4` before the keychain is read. An entry must be a plain hostname, with no port, user, path, trailing dot, or IP address; any other entry makes `config.toml` invalid, which every command reports as described above for a file that does not parse.

### The close record

Every `tasks done` call that sends an update writes one line of JSON to stderr and appends the same line to `tasks-closes.jsonl` in the forgectl config directory. It does this with or without `--json`, and whatever `log_level` is. A call that sends no update (an already-done task, or any failure before the update) writes no record.

```json
{"time":"2026-01-02T03:04:05Z","task_id":42,"project_id":3,"surface":"done","closer":"cli","evidence":"merged owner/repo#12","credential":"vikunja-write","host":"<host>","outcome":"closed"}
```

`outcome` is `closed`, `not_confirmed`, `write_refused`, or `unauthorized`. `credential` is the name of the keychain entry the token was read from, never the token. `closer` is the name the caller declared; nothing verifies it.

So under `--json`, a failure after the update was sent puts two things on stderr: the record line first, then the failure object. The record is always exactly one line that starts with `{"time":`, and the failure object is everything after it. A success puts the record on stderr and the result on stdout.

If the record cannot be appended to the file, the close still stands and the exit code does not change. stderr gets one more plain line that says so. The stdio MCP server appends its `complete_task` records to the same file, with `surface` `mcp`; over HTTP the server writes them to stderr only.

The file is created with mode `0600`. forgectl refuses to append to it when the path is a symlink or anything else that is not a regular file, or when its mode lets group or other read or write it. The record then goes to stderr only, with the same plain line.

### The host refusal line

`tasks-closes.jsonl` holds one other kind of line. When a `tasks` verb that would read the keychain (`ls`, `show`, `ready`, `done`, or `mcp` over stdio) is refused by the host rule and exits `4`, forgectl appends one line to the file:

```json
{"time":"2026-01-02T03:04:05Z","event":"host_refused","verb":"ls","host":"<host as given>","credential":"vikunja-readonly"}
```

`event` is always `host_refused`; a close record has no `event` key, which is how a reader tells the two apart. `verb` is the `tasks` subcommand. `host` is the value given to `--host`, cut to 253 characters, with a user and password before an `@` replaced by `[redacted]` and a value holding a token shape replaced whole. `credential` is the name of the keychain entry the verb would have read, or `""` when that name is not a valid service name. No token is in the line: the keychain has not been read when the rule refuses.

This line goes to the file only. stderr carries the refusal itself, as the error text or, under `--json`, as the failure object with code `failed`. If the line cannot be appended, the exit code is still `4` and stderr gets one plain line that says so, before the failure object.

## Enforcement

`newRoot` installs the contract on every command that declares `--json` (`installJSONErrorContract` in `internal/cli/json_errors.go`), so a new verb inherits it without extra wiring. A verb that writes a verdict and then exits non-zero calls `jsonVerdict` to exit silently.

`TestJSONStderr_BadFlag_EveryVerb` walks the whole command tree and sends each `--json` verb through fang with a bad flag. `TestJSONStderr_NoUnwrappedErrorSites` fails when a `--json` verb uses a cobra hook or check the contract cannot wrap: a required flag, a flag group, a `PreRun`, `PostRun`, `PersistentPreRun` or `PersistentPostRun` hook, or a parent command with no argument validator.
