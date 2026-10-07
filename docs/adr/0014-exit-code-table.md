# 0014. One exit-code table for every verb

**Status: Proposed**

Date: 2026-10-06

## Context

[ADR-0008](0008-agent-contract.md) rule 3 says exit codes must be honest. It does not say which numbers mean what, so each verb family picked its own. [#1085](https://github.com/cameronsjo/forgectl/issues/1085) found the result: the same failure class exits 1 or 2 depending on which layer caught it, and exit 2 means four different things. An agent that branches on `rc == 2` ("fix the call, do not retry") misreads a `tasks` outage.

This ADR measures today's codes, proposes one table, lists every code that would change, and asks the maintainer for the decisions that break callers. It changes no code.

### What the code does today

Measured on `origin/main` at `2ecae571` with a built binary, stdin on `/dev/null`, a throwaway `HOME`, and the help text of every verb. A repeat of the measurement: build with `go build -o /tmp/forgectl .`, then run `forgectl <verb> --zzbogus; echo $?` for each leaf in `forgectl menu --json`.

The mechanism is `WithExitCode` and `ExitCode` (`internal/cli/exitcode.go:37`, `:48`). An error that never opts in exits 1; `main.go` calls `cli.ExitCode` once on whatever `Execute` returns. There is no shared set of names. Each family declares its own numbers.

| Family | Codes in use | Where | What each one means |
| --- | --- | --- | --- |
| Cobra's own errors (bad flag, wrong argument count, unknown verb) | 1 | default of `ExitCode` | 130 leaves measured exit 1 on `--zzbogus` (a few pass-through verbs such as `k8s exec` fail for another reason, also 1); the six `docs` leaves exit 2 |
| Group verbs with an unknown argument | 0 | `config zzbogus`, `completion nonesuch` print help and exit 0 | no failure reported |
| `desk` | 1, 2, 75, 130, 141 | `internal/cli/desk.go:35-43` | 2 usage or missing precondition; 1 refused, no such item or run, desk unreadable; 75 `desk watch` hit `--deadline`; 130 interrupted; 141 stdout closed. `desk watch` otherwise exits with the run's own rc |
| `surface` | 1, 2 | `surface_close.go:113-114`, `surface_read.go:66-67`, `surface_brief.go:97-99`, `surface_ready.go:77-78`, `surface_wait.go:79-80` | 2 usage or setup, including "no such worker" (`surface_close.go:146`); 1 refused, blocked, gone, not settled |
| `resume` | 0, 1, 2, 130 | `resume.go:103-107`, `:163-165`, `:454`, `:518` | 1 no session matched, or ambiguous with no way to pick; 2 the target is still running, or a bad `[proxy] launch_profile`; 130 the pick was cancelled. `resume restart` and `resume hooks` use 1 for "incomplete, retry" and 2 for bad usage (`resume_restart.go:121-125`) |
| `tasks` | 1, 2, 3, 4 | `internal/cli/tasks.go:36-38`, `tasks_mcp.go:399-402` | 2 instance unreachable; 3 credential rejected; 4 host not allowed for the keychain credential; 1 everything else, including usage, not found, credential missing, write refused. `tasks mcp --ping` uses the same 2 and 3 |
| `docs` | 0, 1, 2 | `docs_errors.go:31-41`; `docs/commands/docs.md` § Exit codes and errors | 2 "could not run" (bad root or config, bad flag, timeout, no backend, bind failure); 1 the verb ran and found errors or a partial search; `docs check` returns 2 for a partial tree. `docs read` passes `mdroll`'s status through |
| `env check` | 0, 1, 2 | `env.go:546-569` | 1 drift, and also every failure that is not "file absent", usage included; 2 the env file or example is absent |
| `env get`, `env set`, `y`, `proxy`, `k8s` and the rest | 1 | default | usage and failure are the same code |
| `update` | 0, 1, 2 | `update.go:94-95` | 1 a step failed; 2 a harness error |
| `audit secrets` | 0, 1, 2 | `audit_secrets.go:101-103` | 1 the scan could not complete; 2 a bad flag value |
| `status`, `projects list`, `review releases` with `--strict` or `--fail-on-stall` | 0, 1 | `status.go:319`, `projects_list.go:163`, `review_releases.go:114` | 1 degraded, by request |
| `doctor`, `preflight`, `pr drain`, `pr repair`, `upgrade` | 0, 1 | `doctor.go:57`, `preflight.go:80`, `pr_drain.go:222-230`, `pr_repair.go:398-409`, `upgrade.go:62` | 1 a check failed, misaligned, a review failed to launch, unsettled sessions, upgrade failed |
| Config that does not parse | 2 | `execute.go:194` | every verb |
| Unresolvable `$HOME` or `$XDG_CONFIG_HOME` | 1 | `docs/json-contract.md` | every verb |
| `launch`, `surface _exec` | the harness's own code | `surface_trampoline.go:310-321` | pass-through; not forgectl's to assign |

Probes that show the split directly:

```text
desk add (no file)          1   cobra's argument check; the verb's help says "2 a usage error"
desk add X --what a         2   the verb's own check (--why missing)
desk show (no name)         2   verb check
desk status a/b             2   verb check (malformed name)
desk status 99-nope         1   real not found; the error lists the waiting items
surface close (no name)     1   cobra; the verb's help says "2 usage or setup"
surface close nosuch        2   not found, folded into "setup"
tasks done abc --evidence x 1   code "usage_error", documented as exit 1
tasks show abc              1   "abc is not a task id"; the usage class
tasks mcp --ping            1   needs --http; the usage class
```

The JSON `code` strings split the same way. `docs/json-contract.md` documents `usage_error` and `failed` for most verbs, with `usage_error` at exit 1 for `tasks done`. A `failed` object appears at exit 1, 2 or 4. `docs` writes an integer `code`, and `env check` writes `check_failed` for usage errors.

### Who depends on today's codes

Measured by searching this repo and the maintainer's other local checkouts.

- **In this repo, scripts test zero or non-zero only.** `scripts/dogfood-drain.sh:72-77` treats any non-zero from `pr drain` as failure. `scripts/verify-v2-list-surfaces-unreadable.sh:43-49` requires exit 0. `scripts/mcp-stdio-smoke.sh` reserves its own 1 and 2 and does not read forgectl's codes except through the server's output.
- **The `tasks` codes 2, 3 and 4 are a public contract.** They are documented in `docs/json-contract.md` and `tasks.go:24-35`, which says 2 and 3 match an external reference probe script (`UNREACHABLE`, `UNAUTHENTICATED/FORBIDDEN`). A caller can alert on 4 because it is a security verdict.
- **The resume watcher retries on exit 1** (`resume_restart.go:121-125`, `docs/commands/resume.md`). A change to 1 for "incomplete" would stop retries.
- **Documented per-verb contracts**: `resume.md:54-56` (1 vs 2), `env.md:68` ("exit codes are part of its contract"), `docs.md` § Exit codes and errors, `desk.md` and `surface` help.
- **Outside this repository, not verified.** I searched the maintainer's other local checkouts for callers of `forgectl desk|surface|tasks|docs|resume|env|review|pr|status` and found none that branch on a code other than zero. The request for this ADR said scripts and a fleet orchestrator ("foreman") depend on today's codes. No script by that name turned up; the string appears in another checkout only as an agent label. Whether a fleet script branches on a specific code is a question only the maintainer can answer. This is decision 1 below.

## Decision

Adopt one table, apply it at the root, and ship it in two phases that differ in risk. Phase 1 is a small break with a clear gain. Phase 2 is the rest of the table behind an explicit opt-in.

### The table

Every forgectl-assigned exit code is one of these. A verb's help lists the codes it can return, with the class name.

| Code | Class | Meaning | Retry? |
| ---: | --- | --- | --- |
| 0 | `ok` | The verb did what its name says. Includes "already done" and a no-op whose goal already holds (`desk skip` of an already-skipped item, `tasks done` of a done task). An empty list or search is `ok`. | n/a |
| 1 | `failed` | The verb ran and the result did not hold: a failed step, a refused action whose reason is in the output, a partial result, drift, a degraded `--strict` report. This is the default for any error with no class. | Maybe |
| 2 | `usage` | Nothing was attempted, and the caller or operator can fix it by changing the call or the setup: a bad flag, a wrong or malformed argument, an unknown verb or subverb, a missing precondition (no terminal, no herdr pane, no backend), a config that does not parse. | No: fix the call |
| 3 | `unauthorized` | A credential is missing, rejected, or may not do this. | No: fix the credential |
| 4 | `refused` | A safety rule refused to send or do something, and no retry with the same inputs will pass. Today only `tasks` host refusal. | No |
| 5 | `unreachable` | A dependency did not answer: the instance, the network, a socket. | Yes, later |
| 6 | `not_found` | The thing the caller named does not exist: a task id, a worker, an item, a run, a session filter with no match, a file the verb was told to read. | No: fix the name |

Outside the table, kept as they are and documented once in the same place:

- **75** (`desk watch` reached `--deadline`; the last line holds the resume command), **130** (interrupted), **141** (stdout closed). These mimic `sysexits.h` and the shell's reporting of signals and already ship in `desk.go:38-43`. `resume` also uses 130 for a cancelled pick.
- **Pass-through** verbs (`launch`, `surface _exec`, `docs read`, `desk watch` for its run's rc) return a child's code. The table does not apply to a child's code. The help of each says so.
- Codes 126, 127 and 128 and above are never assigned for forgectl's own errors, apart from 130 and 141 above.

`usage` is the one class that was never misleading in the help text: `desk`, `surface`, `resume`, `docs`, `update` and `audit` already document 2 as usage or setup. The table keeps that and makes cobra's errors match it.

### Root mapping

One place maps errors to classes, so no verb has to remember.

1. Cobra's flag errors, argument-count errors and unknown-command errors are wrapped as `usage` at the root. `SetFlagErrorFunc` and the argument wrapper already exist in `internal/cli/json_errors.go` and `usage_args.go`; both gain the code.
2. A group verb with an unrecognized argument (`config zzbogus`, `completion nonesuch`) returns `usage` instead of printing help and exiting 0 (ADR-0008 rule 3).
3. `ExitCode` keeps its default of 1 (`failed`). The class constants replace the bare numbers in `WithExitCode` calls, so a reader greps `exitNotFound`, not `6`.
4. A table-driven test walks every leaf and proves that a bad flag exits 2 and, under `--json`, writes `code: "usage_error"`.

### JSON `code` and exit status

Under `--json`, one table ties the string to the exit status. The failure object keeps its shape (`{"error","code","path"}`); only the pairing is fixed.

| `code` | Exit |
| --- | ---: |
| `usage_error` | 2 |
| `unauthorized`, `credential_missing` | 3 |
| `refused` (new; `tasks` host refusal is `failed` today) | 4 |
| `unreachable` (new; `failed` today) | 5 |
| `not_found` | 6 |
| `failed` and every other string (`repeating_task`, `write_refused`, `not_confirmed`, `trailer_too_long`, `check_failed`) | 1 |

`env check` keeps `check_failed` for non-usage failures and `file_not_found` for an absent file. `docs` keeps its integer `code`, which already equals the exit status.

### Every code that would change

**Phase 1: usage becomes 2.** No opt-in. Each row is old to new.

| Verb or path | Old | New | Why it changes |
| --- | ---: | ---: | --- |
| Cobra flag error, every leaf except `docs *` (130 measured) | 1 | 2 | matches verb help that already says 2 |
| Cobra argument-count error: `desk add\|plan\|skip\|watch`, `surface brief\|close\|read\|ready\|wait`, `tasks done\|show`, `workflow bless\|run\|status\|verify`, `review mark\|unmark`, `env get\|set`, `y file\|img`, `proxy use`, `k8s exec\|logs`, `version x`, and the rest | 1 | 2 | same |
| Unknown verb or subverb (`forgectl zzbogus`, `desk zzbogus`) | 1 | 2 | same |
| `config zzbogus`, `completion nonesuch` | 0 | 2 | ADR-0008 rule 3: a failed request must not exit 0 |
| `tasks done` usage refusals (`usage_error`), `tasks show` with a non-numeric id, `tasks mcp --ping` without `--http`, a refused `--keychain-service` | 1 | 2 | `docs/json-contract.md` today documents 1 for `usage_error` |
| `env check` usage cases (unknown flag, flag missing its value, stray argument): code `check_failed` | 1 | 2, code `usage_error` | one string changes; drift and other failures stay `check_failed` at 1 |
| Unresolvable `$HOME` or relative `$XDG_CONFIG_HOME` | 1 | 2 | setup error, like an unparseable config, which is already 2 |
| `resume` ambiguous filter with no way to pick | 1 | 2 | the caller must change the filter; this splits it from "no session matched" |

Unchanged in Phase 1: every `docs` code, every `desk` and `surface` code that is already 2, 75, 130, 141, all `tasks` codes 3 and 4, every verdict exit (1 for a failed check, drift, degraded `--strict`).

**Phase 2: the rest of the table, opt-in.**

| Verb or path | Old | New |
| --- | ---: | ---: |
| `tasks` instance unreachable (`failed`), `tasks mcp --ping` no answer | 2 | 5 (`unreachable`) |
| `tasks` host refusal | 4 | 4 (`failed` becomes `refused`) |
| `tasks` credential missing (`credential_missing`) | 1 | 3 |
| `tasks show`, `tasks done` task not found (`not_found`) | 1 | 6 |
| `desk status\|skip\|watch\|plan` no such item; `desk show`, `desk runs` no such run | 1 | 6 |
| `surface close\|read\|ready\|wait\|brief` no such worker | 2 | 6 |
| `resume` no session matched, no recent sessions | 1 | 6 |
| `env check` env or example file absent (`file_not_found`) | 2 | 6 |
| `docs` timeout (`--timeout` deadline) | 2 | 5 |

Legacy codes, deliberately not moved in either phase, and listed in the table's reference page: the `surface` "refused, blocked, gone, not settled" verdicts and `desk` refusals (1, a `failed` verdict that carries its reason), `resume`'s "target is still running" refusal (documented 2 in `resume.md:55`; moving it to 4 would break a published contract for no gain a caller can use), and the whole `docs` "could not run" family at 2 (it is usage or setup under this table).

Phase 2 also changes JSON `code` strings (`failed` becomes `unreachable`, `refused`; `credential_missing` and `not_found` get new exits). That is a breaking change to a documented object shape and is the strongest reason to keep Phase 2 behind an opt-in.

### Migration, per the integrator contract

The CLI integrator contract (stable surface, deprecation window, a way for CI to find use before removal) asks for an announcement, a stated window, a way for CI to find use before removal, and no flag that is accepted and ignored. Applied here:

1. **Phase 1 ships in one minor release, marked breaking.** The PR carries a `BEGIN_COMMIT_OVERRIDE` block with a `feat!:` line, so the release note names every row above. There is no opt-in: the change moves only the usage class, the verb help already promises 2, and no consumer found in this checkout or its siblings reads the old value. `0.x` bumps the minor.
2. **Phase 2 ships behind `FORGECTL_EXIT_CODES=v2`.** The variable is a declared mode, not environment sniffing. It appears in `forgectl config` output, so ADR-0008 rule 4 holds. Unset, the codes are Phase 1's. Set to `v2`, the Phase 2 rows apply and the new `code` strings are emitted.
3. **The window is two minor releases**, then `v2` becomes the default and `FORGECTL_EXIT_CODES=v1` is the one-release escape hatch. Each release's notes repeat the table. A deprecation line goes to stderr, once per run and only when the variable is unset and a changed code would have been returned, so CI finds the dependency before the default flips.
4. **A snapshot gate protects the release.** The release PR runs the contract snapshot and diff from `cli-integrator-contract` against the previous tag, with exit codes captured for a fixed set of failing invocations (one per row above). A `BREAKING exit A -> B` row outside the lists in this ADR fails the check.

### The reference page

`docs/exit-codes.md` holds the one table. `forgectl --help` links it in the footer. `docs/json-contract.md` points to it instead of repeating numbers, and each verb page and help text names the classes it can return with the number: `2  usage`, not `2  a usage or setup error`.

## Implementation plan

A separate PR, not this one. Steps, in order:

1. Add class constants (`exitOK`, `exitFailed`, `exitUsage`, `exitUnauthorized`, `exitRefused`, `exitUnreachable`, `exitNotFound`) beside `WithExitCode` in `internal/cli/exitcode.go`, and replace the bare 1 and 2 literals listed under "What the code does today". Keep `ExitCode`'s default.
2. Wrap cobra errors as `usage`: the flag-error func and argument wrapper in `json_errors.go` and `usage_args.go`, and the unknown-command path in `execute.go`. Make group verbs reject unknown arguments.
3. Add the code-to-exit table to the JSON failure helper (`jsonFailure`) so each `code` string carries its exit, and fail a test if a string has no row.
4. Apply the Phase 1 rows per verb: `tasks` usage cases, `env check` usage cases, `$HOME` resolution, `resume` ambiguity.
5. Write `docs/exit-codes.md`; update `docs/json-contract.md`, the verb pages, and each help `Exit codes:` block.
6. Phase 2 behind `FORGECTL_EXIT_CODES`: one resolver `classExit(class)` that returns the v1 or v2 number, so a verb names a class and never a number. Show the mode in `forgectl config`.

Tests, each shown red without its change:

- **Leaf walk.** Every leaf from `menu --json` with a bad flag exits 2, and under `--json` writes one `usage_error` object (extends `TestJSONStderr_BadFlag_EveryVerb`).
- **Table pin.** A test lists every old to new row above and asserts the new code.
- **Legacy pin.** The legacy rows still return their numbers, so a later edit cannot move them silently.
- **Docs agreement.** The reference page and each help `Exit codes:` block name the same numbers (the same pattern `desk_names` help tests use).
- **Phase 2.** The same table under `FORGECTL_EXIT_CODES=v2`, and an unset run still returns Phase 1.
- **Contract snapshot.** `cli_contract.py snapshot` and `diff` against the last release, with the failing-invocation set.

The PR states every changed code in its body, which this ADR's two tables supply.

## Decisions for the maintainer

1. **Does anything outside this repo branch on a specific forgectl code?** The request for this ADR names a fleet orchestrator and scripts. Nothing in this checkout or its siblings does. If a fleet script tests `== 1` for usage, Phase 1 changes its behavior. Confirm before the PR merges.
2. **Phase 1 as a direct break, or opt-in first?** Recommended: direct, in one minor, with the `feat!:` note. The alternative is to ship the whole table behind `FORGECTL_EXIT_CODES=v2` and flip the default after the window, which costs two release cycles and leaves the misleading cobra codes in place meanwhile.
3. **Phase 2 at all, and its window.** Recommended: ship it opt-in, two minor releases before the default flips. The cost lands on the `tasks` consumers (2 becomes 5), the resume watcher is untouched, and the JSON `code` strings change. If nothing needs to tell "not found" from "failed", skipping Phase 2 leaves the surface clean and smaller.
4. **`tasks` unreachable moves from 2 to 5.** The reference probe script treats 2 as `UNREACHABLE`. That script is outside this repo. If it must not change, the alternative is to keep `tasks` at 2 and let `usage` stay 1 there, which gives up the single table.
5. **New `code` strings (`refused`, `unreachable`) are a breaking change to the JSON object.** Accept, or keep `failed` and let the exit status carry the class.

## Alternatives declined

- **Minimum fix: only make cobra usage errors exit 2.** This is Phase 1 without the table. It removes the 1-or-2 split and leaves "exit 2 means different things" untouched. It is the right first step, and the decision above keeps it as Phase 1. Declined as the whole answer because `tasks 2` stays the exception.
- **Adopt `sysexits.h` (64 usage, 66 no input, 69 unavailable, 75 temp fail, 77 no permission, 78 config).** It is a standard, and `desk watch` already returns 75. It collides with nothing in the table, but it moves every `usage` code from 2 to 64, where the verb help and most callers already read 2, and it leaves `tasks` 3 and 4 as odd ones. A bigger break for a naming gain.
- **The issue's example numbering: 2 usage, 3 auth, 4 not found, 5 transient.** It conflicts with the live `tasks` meaning of 4 (host refused). This table keeps 3 and 4 where `tasks` already puts them and takes 5 and 6 for the new classes.
- **One-shot break of the whole table in a minor.** Faster, and a loud surprise for `tasks` callers and anything matching `code: "failed"`. Declined for Phase 2; accepted for Phase 1, where the changed class is small and the documentation already promised it.
- **A flag instead of an environment variable for the opt-in.** A flag must be added to every invocation and does not reach wrappers that call forgectl for you. The variable reaches them. A `--exit-codes` root flag can be added later for one-off checks.
- **Leave it and document the per-verb codes better.** The per-verb docs already exist and are accurate. The failure is that no one table lets a caller decide without reading the verb's page.

## Consequences

- An agent can branch on a number: 2 means fix the call, 3 the credential, 5 try later, 6 fix the name. This holds for cobra's errors too.
- Phase 1 changes the exit status of every usage error. A script that tests `$? -eq 1` for "bad call" breaks. A script that tests non-zero is unaffected.
- The reference page becomes a contract. A new verb picks a class instead of a number, and review checks the choice (the ADR-0008 review checklist gains an exit-class line).
- `docs/json-contract.md` stops carrying its own copy of the numbers.
- Phase 2 leaves two modes for two releases, with the cost of keeping both tested.
