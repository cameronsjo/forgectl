# Exit codes

One table for every forgectl verb ([ADR-0014](https://github.com/cameronsjo/forgectl/pull/1146), [#1085](https://github.com/cameronsjo/forgectl/issues/1085)). Branch on the exit status first. The JSON `code` string on stderr is detail, and several strings share one exit status.

| Code | Class | Meaning | Default action |
| ---: | --- | --- | --- |
| 0 | `ok` | The verb did what its name says. Includes "already done", and an empty list or search. | Continue. |
| 1 | `failed` | The verb ran and the result did not hold: a failed step, drift, a partial result, a refusal that carries its reason, a degraded `--strict` report. The default for an error with no class. | Read the output. Re-run once if it names a transient cause; otherwise stop and report. |
| 2 | `usage` | Nothing was attempted, and the caller or operator can fix it: a bad flag or argument, an unknown verb, a malformed name, something absent or unconfigured (no terminal, no herdr pane, no backend on `PATH`), a config that does not parse. | Stop. Fix the call or setup. Do not retry it unchanged. |
| 3 | `unauthorized` | A credential is missing, rejected, or may not do this. | Stop. Escalate to whoever owns the credential. |
| 4 | `refused` | A safety rule said no, and the same inputs will not pass. Today only `tasks` host refusal. | Stop. Escalate. |
| 5 | `unreachable` | Reserved. No verb emits it. | |
| 6 | `not_found` | Reserved. No verb emits it. | |

5 and 6 are reserved for a later phase, which is built only when a caller needs to tell "not found" or "unreachable" apart from "failed". A new verb must not use them for anything else.

## Outside the table

- **75** (`desk watch` reached `--deadline`; the last line holds the resume command), **130** (interrupted) and **141** (stdout closed). `resume` also uses 130 for a cancelled pick.
- **Pass-through verbs** return a child's code: `launch`, `surface _exec`, `docs read` (the reader's status), `k8s` (kubectl's, including a remote command's), and `desk watch` for its run's rc. Their help says so. 126, 127 and 128 and above are never assigned for forgectl's own errors, apart from 130 and 141.

## Where usage keeps exit 1

A verb whose 2 already means something else keeps usage errors at 1, so a 2 never means two things in one verb.

| Verb | Usage stays at | Why |
| --- | --- | --- |
| `tasks` (all subverbs, `mcp --ping`) | 1 | 2 is "instance unreachable, retry", and an external probe depends on it. |
| `env check` | 1 (`check_failed`) | 2 is "file absent", part of its documented contract. |
| `resume snapshot` | 1 | Wired as a Claude Code `Stop` hook. A `Stop` hook that exits 2 blocks the session from stopping, so `resume snapshot` never exits 2: not on a bad flag, an unresolvable `$HOME`, or a config that does not parse. |
| `k8s` | kubectl's code, else 1 | Pass-through. |

`docs` keeps every code it has (2 is its "could not run", timeout included). `resume` keeps 1 for "no session matched" and "ambiguous filter", and 2 for a running target. Existing setup 2s in `preflight`, `doctor` and `update` stay as they are.

## What changed from the previous release

| Path | Old | New |
| --- | ---: | ---: |
| Cobra flag error on every leaf outside the exceptions and `docs *` | 1 | 2 |
| Cobra argument-count error (`desk add`, `surface close`, `workflow run`, `env get`, `y file`, `proxy use`, `version x`, and the rest) | 1 | 2 |
| Unknown verb or subverb (`forgectl zzbogus`, `desk zzbogus`), and the `docs` group's own flag error | 1 | 2 |
| `config zzbogus`, `completion nonesuch` | 0 | 2 |
| Unresolvable `$HOME` or relative `$XDG_CONFIG_HOME` (not `resume snapshot`, which exits 0) | 1 | 2 |
| `resume` flag and argument errors (not `resume snapshot`) | 1 | 2 |

`desk show a/b --json` and `tasks show abc --json` now report `code: "usage_error"` instead of `failed`. Their exit status is unchanged by the code string (2 and 1).

A script that tests `$? -eq 1` for "bad call" breaks. A script that tests non-zero is unaffected.
