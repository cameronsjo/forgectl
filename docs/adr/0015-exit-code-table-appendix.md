# ADR-0015 appendix: exit codes as measured on 2026-10-06

Evidence for [ADR-0015](0015-exit-code-table.md). It records what the code does today, so the ADR can stay a decision. Measured on `origin/main` at `2ecae571` with a built binary, stdin on `/dev/null`, a throwaway `HOME`, plus each verb's help text and the code.

To repeat the flag-error measurement: build with `go build -o /tmp/forgectl .`, then run `forgectl <verb> --zzbogus; echo $?` for every leaf in `forgectl menu --json`. That probe sees cobra's errors only. It cannot see the verb-level 2s below, so the second half of the audit is `grep -rn 'WithExitCode(' internal/cli`.

## How a code is chosen

`WithExitCode` and `ExitCode` (`internal/cli/exitcode.go:37`, `:48`). An error that never opts in exits 1. `main.go` calls `cli.ExitCode` once on whatever `Execute` returns. There is no shared set of names; each family declares its own numbers.

## Codes in use, per family

| Family | Codes | Where | What each means |
| --- | --- | --- | --- |
| Cobra's own errors (bad flag, argument count, unknown verb) | 1 | default of `ExitCode` | every leaf outside `docs` exits 1 on `--zzbogus`: 130 probed here, and a review counted 133 childless menu leaves with the same result (a few pass-through verbs such as `k8s exec` fail for another reason, also 1). The six `docs` leaves exit 2. The `docs` group itself exits 1 on `docs --zzbogus` |
| Group verbs with an unknown argument | 0 | `config zzbogus`, `completion nonesuch` | help is printed, exit 0 |
| `desk` | 1, 2, 75, 130, 141 | `internal/cli/desk.go:35-43` | 2 usage or missing precondition; 1 refused, no such item or run, desk unreadable; 75 `desk watch` reached `--deadline`; 130 interrupted; 141 stdout closed. `desk watch` otherwise exits with the run's own rc |
| `surface` | 1, 2 | `surface_close.go:113-114`, `surface_read.go:66-67`, `surface_brief.go:97-99`, `surface_ready.go:77-78`, `surface_wait.go:79-80`, `surface.go:212-246` | 2 usage or setup, including "no such worker" (`surface_close.go:146`); 1 refused, blocked, gone, not settled |
| `resume` | 0, 1, 2, 130 | `resume.go:103-107`, `:163-165`, `:454`, `:518` | 1 no session matched, or ambiguous with no way to pick (deliberately one code, `docs/commands/resume.md:54`); 2 the target is still running, or a bad `[proxy] launch_profile`; 130 the pick was cancelled. `resume restart` and `resume hooks`: 1 incomplete, 2 bad usage (`resume_restart.go:121-125`) |
| `tasks` | 1, 2, 3, 4 | `tasks.go:36-38`, `tasks_mcp.go:399-402` | 2 instance unreachable; 3 credential rejected; 4 host not allowed for the keychain credential; 1 everything else, including usage, not found, credential missing, write refused. `tasks mcp --ping` uses the same 2 and 3 |
| `docs` | 0, 1, 2 | `docs_errors.go:31-41`; `docs/commands/docs.md` § Exit codes and errors | 2 "could not run" (bad root or config, bad flag, timeout, no backend, bind failure); 1 the verb ran and found errors or a partial search; `docs check` returns 2 for a partial tree. `docs read` passes `mdroll`'s status through |
| `env check` | 0, 1, 2 | `env.go:546-569` | 1 drift, and every failure that is not "file absent": a refused `--file` or `--example` name, a flag error, a stray argument; 2 the env file or example is absent |
| `preflight` | 0, 1, 2 | `preflight.go:79-80`, `:86-109` | 1 misaligned; 2 an error (cannot resolve home or project directory, apply failed) |
| `doctor` | 0, 1, 2 | `doctor.go:57`, `:71-76` | 1 a check failed; 2 the report could not be written (not in the help) |
| `herdr organize`, `recipe`, `launch which`, `launch` (own failures) | 2 | `herdr.go:139-161`, `recipe.go:114-135`, `launch_which.go:35-45`, `launch.go:181` | setup or resolution refusals: no herdr target, an invalid rule set, a lock unavailable, a refused fork. Everything after `launch` goes to the harness |
| `k8s` | kubectl's | `k8s.go:108-125` | every subcommand returns kubectl's real exit code, including the remote command's status for `k8s exec` |
| `update` | 0, 1, 2 | `update.go:94-95` | 1 a step failed; 2 a harness error |
| `audit secrets` | 0, 1, 2 | `audit_secrets.go:101-103` | 1 the scan could not complete; 2 a bad flag value |
| `status`, `projects list`, `review releases` | 0, 1 | `status.go:319`, `projects_list.go:163`, `review_releases.go:114` | 1 a section or host degraded, only with `--strict` or `--fail-on-stall` |
| `pr drain`, `pr repair`, `upgrade` | 0, 1 | `pr_drain.go:222-230`, `pr_repair.go:398-409`, `upgrade.go:62` | 1 a review failed to launch, sessions unsettled, upgrade failed |
| `env get`, `env set`, `y`, `proxy`, and the rest | 1 | default | usage and failure share one code |
| Config that does not parse | 2 | `execute.go:194` | every verb |
| Unresolvable `$HOME` or `$XDG_CONFIG_HOME` | 1 | `docs/json-contract.md` | every verb except `resume snapshot`, which exits 0 and prints "snapshot skipped: $HOME is not defined" |
| `launch`, `surface _exec` | the harness's code | `surface_trampoline.go:310-321` | pass-through |

## Probes that show the split

```text
desk add (no file)           1   cobra's argument check; the verb's help says "2 a usage error"
desk add X --what a          2   the verb's own check (--why missing)
desk show (no name)          2   verb check
desk status a/b              2   verb check (malformed name)
desk status 99-nope          1   real not found; the error lists the waiting items
desk show a/b --json         2   code "failed"
surface close (no name)      1   cobra; the verb's help says "2 usage or setup"
surface close nosuch         2   not found, folded into "setup"
tasks done abc --evidence x  1   code "usage_error", documented as exit 1
tasks show abc               1   "abc is not a task id"; code "failed"
tasks mcp --ping             1   needs --http
env check --file nope.env    1   refused: outside the repository
resume snapshot --quiet --zzbogus   1   a Stop-hook command
```

The JSON `code` strings split the same way. `docs/json-contract.md` documents `usage_error` and `failed` for most verbs. A `failed` object appears at exit 1, 2 or 4. `docs` writes an integer `code` (the exit status). `env check` writes `check_failed` for usage errors. Group verbs do not accept `--json` today, so `config zzbogus --json` fails with "Unknown flag" at exit 1.

## Who depends on today's codes

Searched in this repo and the maintainer's other local checkouts.

- **Scripts in this repo test zero or non-zero only.** `scripts/dogfood-drain.sh:72-77` treats any non-zero from `pr drain` as failure. `scripts/verify-v2-list-surfaces-unreadable.sh:43-49` requires exit 0. `scripts/mcp-stdio-smoke.sh` reserves its own 1 and 2. `internal/bless/bless.go:207` reads a helper's codes, not forgectl's. CI workflows test zero or non-zero. Tests that pin exit 1 for usage (for example `json_stderr_contract_test.go`) will change with Phase 1.
- **The `tasks` codes 2, 3 and 4 are a public contract.** They are documented in `docs/json-contract.md`, and `tasks.go:24-35` says 2 and 3 match an external reference probe script (`UNREACHABLE`, `UNAUTHENTICATED/FORBIDDEN`). A caller can alert on 4 because it is a security verdict.
- **The resume watcher does not read the CLI's exit code.** The built-in restart runs in process: `internal/resume/hooks.go:288-296` maps `res.Incomplete()` to `OutcomeIncomplete`, `NextState` (`hooks.go:175-189`) keeps any outcome other than `ok` pending, and the retry pass re-runs only hooks with an action (`hooks.go:482`). A user `command` hook that shells out to `forgectl resume restart` is never retried. Changing a CLI exit code cannot change retries. The one coupling is the `exit` field the hook writes to its audit log (`resume hooks status --json`), which the watcher stamps itself (`Exit = 1` for incomplete).
- **Claude Code hooks.** `docs/commands/resume.md:35-41` and `README.md:161` tell users to wire `forgectl resume snapshot --quiet` as a `Stop` hook that "always exits 0". A mistyped flag there exits 1 today. In Claude Code's hooks reference, a hook that exits 2 is a blocking error (a `Stop` hook that exits 2 keeps the turn going) and other non-zero codes are not blocking. I could not confirm this from this checkout, so the implementation PR checks it against the hooks reference. A one-line audit found no other forgectl command documented as a hook in `README.md` or `docs/`; the launchd watcher runs `forgectl resume hooks run`, and launchd's last exit code is not used (`resume.md:105`).
- **Documented per-verb contracts:** `resume.md:54-56` (1 vs 2), `env.md:68` ("exit codes are part of its contract"), `docs.md` § Exit codes and errors, and the `desk` and `surface` help.
- **Not verified:** a fleet script or orchestrator outside these checkouts. The request for the ADR said scripts and a fleet orchestrator ("foreman") depend on today's codes. No script by that name turned up; the word appears in another checkout only as an agent label.
