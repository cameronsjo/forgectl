# 0014. One exit-code table for every verb

**Status: Accepted**

Date: 2026-10-06 (accepted 2026-10-07)

## Context

[ADR-0008](0008-agent-contract.md) rule 3 says exit codes must be honest. It does not say which numbers mean what, so each verb family picked its own. [#1085](https://github.com/cameronsjo/forgectl/issues/1085) found the result: the same failure class exits 1 or 2 depending on which layer caught it, and exit 2 means several different things. An agent that branches on `rc == 2` ("fix the call, do not retry") misreads a `tasks` outage.

Measured on `origin/main` at `2ecae571`, with the evidence, file and line cites, and the list of dependents in the [appendix](0014-exit-code-table-appendix.md):

- Cobra's own flag, argument and unknown-verb errors exit 1 on every leaf outside `docs`; the six `docs` leaves exit 2. `config zzbogus` and `completion nonesuch` exit 0.
- Verb-level checks use 2 for usage or setup (`desk`, `surface`, `preflight`, `herdr`, `recipe`, `launch which`, `audit`, `update`), but 2 also means "target running" (`resume`), "file absent" (`env check`), "instance unreachable" (`tasks`) and "could not run" (`docs`).
- `tasks` also uses 3 (credential rejected) and 4 (host refused). `desk watch` uses 75, 130, 141. `k8s`, `launch` and `surface _exec` pass a child's code through.
- JSON `code` strings split the same way: `desk show a/b --json` emits `failed` at exit 2, `tasks show abc` emits `failed` at exit 1, `docs` writes an integer, `env check` writes `check_failed` for usage errors.

This ADR adopts one table, lists every code that changes, and records the maintainer's decisions on the breaking parts. It changes no code.

## Decision

Adopt one table, apply it at the root, and ship Phase 1 now. Phase 2 (codes 5 and 6) is deferred; see [Deferred](#deferred-phase-2).

### The table

Every forgectl-assigned exit code is one of these. Branch on the exit status first; the JSON `code` is detail.

| Code | Class | Meaning | Default action |
| ---: | --- | --- | --- |
| 0 | `ok` | The verb did what its name says. Includes "already done" and a no-op whose goal already holds. An empty list or search is `ok`. | Continue. |
| 1 | `failed` | The verb ran and the result did not hold: a failed step, drift, a partial result, a refusal that carries its reason, a degraded `--strict` report. The default for any error with no class. | Read the output. Re-run once if it names a transient cause; otherwise stop and report. |
| 2 | `usage` | Nothing was attempted, and the caller or operator can fix it by changing the call or the setup: a bad flag or argument, an unknown verb, a malformed name, something absent or unconfigured (no terminal, no herdr pane, no backend on `PATH`), a config that does not parse. | Stop. Fix the call or setup. Do not retry it unchanged. |
| 3 | `unauthorized` | A credential is missing, rejected, or may not do this. | Stop. Escalate to whoever owns the credential. |
| 4 | `refused` | A safety rule said no, and the same inputs will not pass. Today only `tasks` host refusal. | Stop. Escalate. |
| 5 | `unreachable` | A configured dependency is present but did not answer: the instance, the network, a socket. | Retry with backoff, then escalate. |
| 6 | `not_found` | The thing the caller named does not exist: a task id, a worker, an item, a run. | Stop. List or inspect to find the right name. |

The boundary between 2 and 5: absent or unconfigured is 2, configured and present but silent is 5.

Phase 1 emits only 0 to 4 (plus the exceptions below). **5 and 6 are reserved: no verb emits them until Phase 2 is built.** A new verb must not use them for anything else.

Outside the table, documented once in the same place:

- **75** (`desk watch` reached `--deadline`; the last line holds the resume command), **130** (interrupted) and **141** (stdout closed) already ship in `desk.go`. `resume` also uses 130 for a cancelled pick.
- **Pass-through** verbs return a child's code, which the table does not govern: `launch`, `surface _exec`, `docs read` (the reader's status), `k8s` (kubectl's, including a remote command's), and `desk watch` for its run's rc. Each one's help says so. 126, 127 and 128 and above are never assigned for forgectl's own errors, apart from 130 and 141; a pass-through verb can return them.

### Root mapping

One place maps errors to classes, so no verb has to remember.

1. Cobra's flag errors, argument-count errors and unknown-command errors are wrapped as `usage` at the root (`SetFlagErrorFunc` and the argument wrapper in `json_errors.go` and `usage_args.go`, and the unknown-command path in `execute.go`).
2. A group verb with an unrecognized argument (`config zzbogus`, `completion nonesuch`, `docs --zzbogus`) returns `usage`, not help at exit 0 or 1 (ADR-0008 rule 3).
3. `ExitCode` keeps its default of 1. Class constants (`exitUsage`, `exitNotFound`, and so on) replace the bare numbers in `WithExitCode` calls.

### Phase 1 exceptions: where 2 already means something else

Phase 1 moves cobra's usage errors to 2 everywhere except verbs whose 2 already carries a different meaning. Those keep usage at 1, so a 2 never means two things in one verb.

| Verb | Stays at | Why |
| --- | --- | --- |
| `tasks` (all subverbs, `mcp --ping`) | usage 1 | 2 is "unreachable, retry" and an external probe depends on it |
| `env check` | usage 1 (`check_failed`) | 2 is "file absent", documented as part of its contract |
| `resume snapshot` | flag errors 1 | wired as a Claude Code `Stop` hook that must not exit 2 (see Consumers) |
| `k8s` | kubectl's code, else 1 | pass-through; a forgectl 2 would collide with kubectl's |

`docs` keeps every code it has (2 is its "could not run", timeout included), apart from the `docs` group's own flag error, 1 to 2. `resume` keeps its documented 1 for "no session matched" and "ambiguous filter" (`resume.md:54`) and 2 for a running target. Setup 2s that already match `usage` (`preflight`, `herdr`, `recipe`, `launch which`, `launch`, `desk`, `surface`, `audit`, `update`) are unchanged. `surface`'s "no such worker" stays 2.

### Every code that changes

**Phase 1** (no opt-in), old to new:

| Path | Old | New |
| --- | ---: | ---: |
| Cobra flag error on every leaf outside the exceptions and `docs *` | 1 | 2 |
| Cobra argument-count error: `desk add\|plan\|skip\|watch`, `surface brief\|close\|read\|ready\|wait`, `workflow bless\|run\|status\|verify`, `review mark\|unmark`, `env get\|set`, `y file\|img`, `proxy use`, `version x`, and the rest | 1 | 2 |
| Unknown verb or subverb (`forgectl zzbogus`, `desk zzbogus`), and the `docs` group's own flag error | 1 | 2 |
| `config zzbogus`, `completion nonesuch` | 0 | 2 |
| Unresolvable `$HOME` or relative `$XDG_CONFIG_HOME` (not `resume snapshot`, which exits 0) | 1 | 2 |
| `resume` flag and argument errors (not `resume snapshot`) | 1 | 2 |

Unchanged in Phase 1: every other `docs` code, 75, 130, 141, `tasks` 3 and 4, every verdict exit 1 (a failed check, drift, a degraded `--strict`, `surface` blocked or refused, `desk` refusals), `resume`'s 1s and its running-target 2, and the pass-through verbs.

### JSON `code` and exit status

The failure object keeps its shape (`{"error","code","path"}`). The pairing is many-to-one, and an agent branches on the exit status first.

| `code` | Exit (Phase 1) |
| --- | --- |
| `usage_error` | 2, or 1 in the exceptions |
| `unauthorized` | 3 |
| `credential_missing` | 1 |
| `not_found` | 1 (`tasks`) |
| `failed`, `repeating_task`, `write_refused`, `not_confirmed`, `trailer_too_long`, `check_failed` | 1, and a few legacy 2 and 4 |

Two further notes. `docs` writes an integer `code` equal to its exit status and keeps it. `desk show a/b --json` and `tasks show abc` carry `failed` today for what is a usage error: both become `usage_error` in Phase 1 (a new `code` string on a failure object, still the same three keys).

Retry guidance by code: only `tasks` `failed` at exit 2 and a `docs` timeout are retryable. `not_confirmed` is not: read the task before retrying. `write_refused`, `repeating_task` and `trailer_too_long` are stops.

### Migration

The CLI integrator contract asks for an announcement, a stated window, a way for CI to find use before removal, and no flag accepted and ignored.

1. **Phase 1 ships in one minor release, marked breaking.** The PR carries a `BEGIN_COMMIT_OVERRIDE` block with a `feat!:` line that names the rows above. There is no opt-in and no environment variable: the change moves only the usage class, the verb help already promises 2 for most of it, and the exceptions keep every verb's 2 unambiguous.
2. **A snapshot gate protects the release.** A table-pin test lists every row above and asserts the new code. If the maintainer's contract-snapshot tooling (`cli_contract.py`, outside this repo) is available, run its `snapshot` and `diff` against the previous tag with one failing invocation per row. The in-repo tests are the gate that does not depend on it.

## Implementation plan

A separate PR, not this one:

1. Add class constants beside `WithExitCode` in `internal/cli/exitcode.go` and replace the bare literals listed in the appendix.
2. Wrap cobra errors as `usage` at the root; make group verbs reject unknown arguments; implement the four exceptions as explicit per-verb overrides.
3. Add the `code`-to-exit pairs to the JSON failure helper (`jsonFailure`). Group verbs do not declare `--json`, so their usage errors stay human text; the leaf walk asserts the `usage_error` object only for leaves that declare it.
4. Make `desk show a/b --json` and `tasks show abc` emit `usage_error`.
5. Write `docs/exit-codes.md`. Add to root `--help` and to each verb's `Exit codes:` block: the seven-row table with the default-action column (5 and 6 marked reserved), and the line that 75, 130, 141 and pass-through verbs sit outside the table. Update `docs/json-contract.md` and the verb pages (`resume.md` keeps its 1s).
6. One resolver, `classExit(class)`, returns the number for a class, so a verb names a class and never a number. It has one mode today; Phase 2 would slot in here.
7. Check Claude Code's hooks reference on exit 2 for a `Stop` hook, and confirm `resume snapshot`'s exemption. A test pins `resume snapshot` to exit 0 or 1, never 2.

Tests, each shown red without its change:

- **Leaf walk.** Every leaf with a bad flag exits 2 (or 1 for an exception), and exits as documented under `--json`.
- **Table pin.** Every row above asserts its new code; the exceptions and the unchanged `docs`, `resume` and pass-through rows assert their old ones.
- **JSON pairs.** Each `(code, exit)` pair in the table holds. A string absent from the table is not an error, because the pairing is many-to-one.
- **Help names the classes.** Root help names every class; each `Exit codes:` block agrees with the reference page.
- **Stop hook.** `resume snapshot` never returns 2, on any flag or environment error.
- **Existing pins.** Update the tests that pin 1 for usage, such as `json_stderr_contract_test.go`.

The PR states every changed code in its body, which the two tables above supply.

## Decisions

Settled by the chief of staff on the maintainer's go-ahead, 2026-10-07 (on [#1146](https://github.com/cameronsjo/forgectl/pull/1146)).

1. **No outside caller branches on a specific code.** Checked on `sjomba`: a Claude Code `Stop` hook runs `forgectl resume snapshot --quiet`, and a `Stop` hook that exits 2 blocks the session from stopping, so `resume snapshot` stays pinned to 0 or 1 and a test asserts it. The cadence-lab `desk` mod runs `forgectl tasks ready` and treats any non-zero exit as failure, so renumbering is safe for it. Nothing found tests `== 1` for usage.
2. **Phase 1 is a direct break.** One minor release, `feat!:`, with the four exceptions.
3. **Phase 2 is deferred.** See below.

## Deferred: Phase 2

Phase 2 would add codes 5 (`unreachable`) and 6 (`not_found`) behind `FORGECTL_EXIT_CODES=v2`, with a two-minor window before the default flips. It is not built. Nothing today needs to tell "not found" or "unreachable" apart from "failed", and two modes for two releases is surface with no current caller. Rows 5 and 6 stay in the table as reserved.

**Trigger:** a caller that needs to tell not-found or unreachable apart from failed. Build it then, and revisit the design below against what that caller needs.

Sketch of what it would change, old to new (under `FORGECTL_EXIT_CODES=v2`):

| Path | Old | New |
| --- | ---: | ---: |
| `tasks` instance unreachable, `tasks mcp --ping` no answer | 2 | 5 |
| `tasks` usage cases | 1 | 2 |
| `tasks` credential missing | 1 | 3 |
| `tasks show\|done` task not found | 1 | 6 |
| `desk status\|skip\|watch\|plan` no such item; `desk show\|runs` no such run | 1 | 6 |
| `surface close\|read\|ready\|wait\|brief` no such worker | 2 | 6 |
| `env check` usage cases (flag error, stray argument, refused `--file` name) | 1 | 2 |
| `env check` file absent | 2 | 6 |
| `resume snapshot` flag errors | 1 | 1 (stays; Stop hook) |
| `tasks` host refusal | 4 | 4 (JSON `code` becomes `refused`) |

It would also add the JSON `code` strings `refused` (exit 4) and `unreachable` (exit 5), move `credential_missing` and `not_found` to 3 and 6, show the mode in `forgectl config` (ADR-0008 rule 4), and send a once-per-run stderr deprecation line naming the variable. The `classExit(class)` resolver in the implementation plan is where the second mode would slot in.

## Alternatives declined

- **Minimum fix: only cobra usage errors exit 2.** This is Phase 1 without the table. It removes the 1-or-2 split and leaves "2 means different things" untouched. It is Phase 1 here; declined as the whole answer.
- **Adopt `sysexits.h` (64 usage, 66 no input, 69 unavailable, 75 tempfail, 77 no permission, 78 config).** It is a standard and `desk watch` already returns 75. It moves every usage code from 2 to 64, where the verb help and most callers already read 2, and leaves `tasks` 3 and 4 as odd ones.
- **The issue's example numbering: 2 usage, 3 auth, 4 not found, 5 transient.** It conflicts with the live `tasks` meaning of 4 (host refused). This table keeps `tasks` 3 and 4 and takes 5 and 6 for the new classes.
- **One-shot break of the whole table in a minor, including 5 and 6.** A loud surprise for `tasks` callers and anything matching `code: "failed"`. Phase 1 is a direct break; the `tasks` and 5/6 parts are deferred.
- **A flag instead of an environment variable for the opt-in.** A flag must be added to every invocation and does not reach wrappers that call forgectl for you. A `--exit-codes` root flag can follow for one-off checks.
- **Move `tasks` unreachable to 5 in Phase 1, or leave `tasks` usage at 2.** Either breaks the documented `tasks` contract and the external probe in the default phase. The exception column keeps `tasks` 2 single-meaning.
- **Leave it and document per-verb codes better.** Those docs exist and are accurate. No single table lets a caller decide without reading each verb's page.

## Consequences

- An agent can branch on a number: 2 means fix the call, 3 the credential, 5 try later, 6 fix the name. Four verb families keep documented exceptions.
- Phase 1 changes the exit status of most usage errors. A script that tests `$? -eq 1` for "bad call" breaks. A script that tests non-zero is unaffected.
- The reference page becomes a contract. A new verb picks a class, and review checks it (the ADR-0008 checklist gains an exit-class line).
- `docs/json-contract.md` stops carrying its own copy of the numbers.
- Codes 5 and 6 are reserved. Adding them later is a new decision with its own migration.
