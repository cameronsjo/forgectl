# The shared status vocabulary

Every `--json` row that tracks a piece of work carries a `work` object beside its native `state`. The native state is what the store records and stays unchanged. `work` says the same thing in words shared by every surface: where the work stands, and while it is open, who owes the next step.

```json
"work": {"status": "blocked", "on": "operator", "reason": "failed: retry or drop"}
```

`work` is computed on read and never stored. `queue.json` and `drain.json` decode strictly, so a renamed state value would make them unreadable to an older binary after a rollback. The mapping lives in one place, `internal/workstate`.

Today `work` is on `surface queue --json`, `surface enqueue --json`, `surface dequeue --json`, `surface drain status --json` (the drain itself and each `attention` row), and each item of `desk status --json`.

## Fields

| Key | Always | Meaning |
| --- | --- | --- |
| `status` | yes | one of the statuses below |
| `on` | for `delivered` and `blocked` | who owes the next step |
| `reason` | no | a few words of why, free text; never parse it |
| `evidence` | for `resolved` | how forgectl knows the goal was met, such as `merged PR #41` or `exit 0` |

## Statuses

Open:

| `status` | Means |
| --- | --- |
| `queued` | accepted and not started; it waits only for capacity |
| `working` | an agent or process is acting on it now |
| `delivered` | the actor says it is finished and nobody has verified it; `on` names the verifier |
| `blocked` | it cannot move until the party `on` names acts |
| `stalled` | nobody is acting on it and nobody is named to: a lost desk run, a stale drain |

Ended:

| `status` | Means |
| --- | --- |
| `resolved` | the goal was met; `evidence` says how that is known |
| `dropped` | it ended on purpose without meeting its goal; `reason` says why |
| `expired` | a timeout ended it and nobody decided |

There is no `done`. A worker that reports, or a script that ran, is not finished until its outcome is known: a report is `delivered`, a script that exited non-zero is `blocked` on the operator.

## Who `on` names

| `on` | Means |
| --- | --- |
| `operator` | the person at the machine. The needs-you inbox is every row `on: operator`, plus every `stalled` row |
| `chief-of-staff` | the chief-of-staff session that queued the row (batch `cos-…`) and verifies its report |
| `system` | forgectl or a service it retries by itself, such as herdr or GitHub |

## How native states map

Queue rows (`queue.json`):

| `state` | `work` |
| --- | --- |
| `queued` | `queued` |
| `claimed`, `launched` | `working` |
| `needs-you` | `blocked` on `operator` |
| `reported` | `delivered` on `chief-of-staff` for a `cos-` batch, else on `operator`; on `operator` once its PR closed unmerged |
| `failed` | `blocked` on `operator`: a failure waits for a retry or a drop |
| `closed` | `resolved` with `merged PR #N` when the closer closed it after a merge; `dropped` otherwise |
| `expired` | `expired` |

A queue row records a merge only in the reason the drain's closer writes when it closes the row. A row closed any other way, by `surface close` included, reads `dropped` with the reason `closed with no merge recorded`, even if its PR merged later.

The drain (`drain.json`):

| `status` | `work` |
| --- | --- |
| `running` | `working` |
| `paused` | `blocked`: on `system` when every pause kind retries by itself (`herdr`, `github`), else on `operator` (`config`, `github-auth`, `launch-config`, or a kind this binary does not know) |
| `stale` | `stalled` |
| `stopped` | `blocked` on `operator` while rows are queued or live; no `work` at all when nothing waits |

Desk items:

| `state` | `work` |
| --- | --- |
| `waiting`, `refused` | `blocked` on `operator` |
| `held` | `queued`: it turns waiting on its own |
| `running` | `working` |
| `lost` | `stalled` |
| `done` | `resolved` with `exit 0` at exit 0; `blocked` on `operator` at any other exit, or with no exit recorded |
| `skipped` | `dropped` |

A done item that failed stays `blocked` until the operator reruns it or `desk prune` removes it.

## Adding a surface

Map the native state in `internal/workstate`, print the result as `work` beside the native field, and add the table here. Keep the native field: callers parse it, and JSON shapes change only additively (ADR-0008).
