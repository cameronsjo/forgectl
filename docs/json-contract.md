# The `--json` stderr contract

[ADR-0008](adr/0008-agent-contract.md) rule 2 puts `--json` on every verb that reports state, and says what goes to stdout. This page covers the other stream: what a `--json` verb writes to stderr when it exits non-zero ([#862](https://github.com/cameronsjo/forgectl/issues/862)).

Under `--json`, forgectl never writes its human error frame (the styled `ERROR` block) to stderr. A non-zero exit takes one of two forms:

- **The verdict is already on stdout.** A verb that reports a problem as its result puts the full JSON verdict on stdout and exits with its documented code. It writes no error object to stderr, because the verdict already says what went wrong. Examples: `doctor` finding a failed check, `docs check` with error findings, `preflight` finding a misaligned project, `pr repair` finding unsettled sessions, `update` with a failed step, `projects list --strict` on a partial inventory, and `launch stats` with skipped rows (the `skipped_rows` field).
- **The verb failed before it emitted anything.** stdout stays empty, and stderr gets exactly one JSON object.

A verb that streams several verdicts can hit both forms in one run. `pr drain --watch --json` writes one report per pass to stdout. If a later pass then fails before it can report, that pass's failure object goes to stderr, and the earlier reports stay on stdout.

`--json` never changes an exit code. The same failure exits with the same code with or without it.

Some failures happen before any verb starts: a `config.toml` that does not parse or cannot be read (exit 2), and an environment forgectl cannot resolve its directories from, such as an unset `$HOME` or a relative `$XDG_CONFIG_HOME` (exit 1). These follow the same rule. When the verb you ran declares `--json` and you passed it, stderr gets that verb's one failure object in place of the plain `forgectl: …` line: code `failed` for most verbs, and the family shapes below for `env check` and the docs verbs. `launch` declares no `--json`, because everything after `launch` goes to the harness, so a `launch` failure stays a plain line.

Some verbs also write documented progress or notes to stderr under `--json`: the `update` transcript, `herdr organize`'s human report, `projects list`'s per-host degradation notes, and `sessions why`'s match count. Those lines are part of the verb's normal output. The contract only rules out the human error frame on top of them.

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

Two verb families shipped their own shapes before this contract existed. Callers already parse them, so they are unchanged (ADR-0008: JSON shapes change only additively).

- **`env check`** uses the same `{"error","code","path"}` object with its own codes: `file_not_found` (exit 2) and `check_failed` (exit 1, used for usage errors too). See [commands/env.md](commands/env.md).
- **The `docs` verbs** (`list`, `check`, `search`) write `{"error","code","root"}`, where `code` is the integer exit code, not a string, and `root` names the docs root a failure stopped on. One case differs from the rule above: a partial `docs search` writes its full response to stdout **and** a code-1 object to stderr. See [commands/docs.md](commands/docs.md#exit-codes-and-errors).

## Enforcement

`newRoot` installs the contract on every command that declares `--json` (`installJSONErrorContract` in `internal/cli/json_errors.go`), so a new verb inherits it without extra wiring. A verb that writes a verdict and then exits non-zero calls `jsonVerdict` to exit silently.

`TestJSONStderr_BadFlag_EveryVerb` walks the whole command tree and sends each `--json` verb through fang with a bad flag. `TestJSONStderr_NoUnwrappedErrorSites` fails when a `--json` verb uses a cobra hook or check the contract cannot wrap: a required flag, a flag group, a `PreRun`, `PostRun`, `PersistentPreRun` or `PersistentPostRun` hook, or a parent command with no argument validator.
