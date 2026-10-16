# forgectl sweep — the chief-of-staff ledger

> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).

```sh
forgectl sweep --json                         # print the ledger (schema 2), and write sweep.json
forgectl sweep                                # one-line summary, and write sweep.json
forgectl sweep --owner LOGIN --since 2026-10-14 --briefs /abs/dir --json   # flags override [sweep]
```

`forgectl sweep` answers one question: of the work the chief-of-staff's workers started, what waits on you? It reads four things and joins them into one ledger.

| Source | What it gives | How it is read |
| --- | --- | --- |
| GitHub | the owner's open PRs, and PRs merged since `since` | `gh api graphql` and `gh search prs`, pinned to the `[github]` host |
| Brief directories | each worker's phase and repositories | files under `briefs`, read without following symlinks |
| `herdr agent list` | each worker's pane and status | the same call `attention` makes |
| The desk | which pane has a waiting item | a read-only scan |

The ledger goes to stdout (with `--json`) and to `$XDG_STATE_HOME/cadence/sweep.json` (`~/.local/state/cadence/sweep.json`), which [`forgectl attention list`](attention.md#the-sweep) reads. Unix only.

## Config

Nothing is defaulted. No owner, path or reviewer name is built into the binary.

```toml
[sweep]
owner = "your-github-login"      # GitHub account whose repos the sweep covers
since = "2026-10-14"             # first day (UTC) whose merges and worker briefs count
briefs = "/abs/path/to/briefs"   # one subdirectory per worker
reviewers = ["pr-reviewer"]      # cadence-review marker names that count
```

`--owner`, `--since` and `--briefs` override the first three. A key set by neither exits 2 and names it (`sweep.owner`, `sweep.since`, `sweep.briefs`, `sweep.reviewers`). A value that is set but unusable (an owner that is not a GitHub name, a `since` that is not `YYYY-MM-DD`, a relative `briefs`, a reviewer name outside `[a-z0-9-]`) also exits 2 and names the key without echoing the value.

Workers are the directories under `briefs` whose names start with `[attention] worker_prefix` (default `cos-`) and that hold a `brief.md`, `picks.md`, `go.md` or `report.md` modified at or after `since`. The next section says what else comes from them.

## What the ledger holds

| Field | Meaning |
| --- | --- |
| `schemaVersion` | `2` |
| `generatedAt` | RFC 3339 UTC |
| `merged`, `dependencyBumps`, `reposTouched`, `sweepRepos`, `openPrs`, `filed` | counts. `sweepRepos` is the repositories the briefs name; merges and PRs outside them are not counted. `filed` is the number of lines with a tab in `filed.txt`, next to the briefs directory |
| `topRepos`, `recent` | up to 8 `[repo, count]` pairs, and the 8 latest merges (`repo` is the short name) |
| `workers[]` | `{name, status, phase, repos, pane?, prs[], blockedBy?}` |
| `prs[]` | `{repo, number, title, state, reason, worker}`, `repo` is `owner/repo` |
| `opinions[]` | `{id, tone, text, action, target?}` |
| `sources` | `github` (`ok` or `truncated`), `herdr` (`ok` or `unavailable`), `desk` (`ok`, `absent` or `error`), `briefs` |

Version 1 (the steward mod's earlier `dashboard.json`) carried `generatedAt`, the counts, `opinions` (tone and text), `workers` (name, status, phase, repos), `topRepos` and `recent`. Version 2 keeps all of them and adds the rest, so a v1 reader still works.

### Worker phase and status

A worker's `phase` is the furthest of:

| Phase | Evidence |
| --- | --- |
| `launched` | `brief.md` exists |
| `plan` | `picks.md` exists and its last line is `PICKS-COMPLETE` |
| `go` | `go.md` exists |
| `report` | `report.md` ends in `REPORT-COMPLETE` and was written at or after `go.md` |

A symlinked directory or file counts for nothing; a symlinked worker directory is skipped and reported on stderr. `repos` come from the bullets (`` - `owner/repo`: … ``) under the brief's `## Where to work` heading.

`status` is the herdr `agent_status` of the agent working the brief, or `unknown` when none is. An agent belongs to a brief when one of its names (its worktree, the basenames of its working directories) equals the brief's name or starts with that name and a `-`; the longest brief name wins, so `cos-bosun668-12` is `cos-bosun668`, not `cos-bosun`.

`blockedBy` is `desk` when a waiting desk item's `signal_pane` equals the worker's pane, else `prompt` when its status is `blocked`. With herdr down no worker has a pane or a `blockedBy`, and `sources.herdr` says `unavailable`.

### PR state

A PR is the first of these:

| State | When |
| --- | --- |
| `draft` | it is a draft |
| `blocked` | it conflicts with its base, its checks are `FAILURE` or `ERROR`, or a marker at its current head has `crit>0` |
| `marker-clean` | a marker at its current head has `crit=0`, none at head has `crit>0`, its checks are green, and it is mergeable |
| `waiting` | any other non-draft PR |

`reason` says which test decided it. A PR with no checks is not green, and neither is one whose mergeability GitHub has not computed.

A **marker** is the first line of a review body: `<!-- cadence-review: <name> head=<40 hex> crit=N imp=N -->` (the comment wrapper is optional). It counts only if it was posted by the authenticated `gh` login, its `head=` equals the PR's current head commit, **and** `<name>` is in `[sweep] reviewers`. Reviews by other accounts are not read (anyone who can review a public repo's PR could otherwise post a marker); a marker for an older head, or from another name, is ignored. The latest 20 of your own reviews are scanned. `crit` is summed over the markers at head, so a PR with any `crit>0` marker at its current head stays `blocked` until the head moves.

**This is a claim filter, not an authenticity check.** Any session that can post under your `gh` identity can post any reviewer name with `crit=0`. `marker-clean` therefore means "someone with your login said so", and the ledger never says `ready`. Run your own reviewers at the head before you merge. `imp` is parsed and does not change the state; read the review.

A PR in a repository no brief names is left out. A PR's `worker` is the one worker whose brief names its repository; it is empty when two or more do. The link is by repository, not authorship: a PR opened by someone else in that repository is attributed to the worker too.

### Opinions

Each opinion has a stable `id`: `pr:<owner/repo>#<n>`, `worker:<name>:<state>` (`blocked`, `plan` or `report`), or `sweep:<slug>` for the aggregates (`busy`, `pace`, `quiet`, `bots`, `churn`, `untouched`, `drafts`, `security`). Opinions are sorted by tone: `act`, `warn`, `note`, `good`.

| Situation | `action` | `target` |
| --- | --- | --- |
| a worker is blocked (`prompt` or `desk`) | `unblock` | `{worker}` |
| a worker's report is in (its agent is idle, `done` or gone) and an open `waiting` PR sits in its repositories | `review` | `{repo, number}` of the lowest-numbered such PR |
| a worker's plan has been ready 10 minutes or more with no GO, and its agent is up and idle or `done` | `none` | none |
| anything else | `none` | none |

A plan with no GO never yields an action. A GO is an approval, so the row offers Jump only, and no field in the ledger can stand in for it. A worker with no live agent counts as idle for the report opinion; a plan opinion needs a live agent, since a dead worker is not waiting on a GO.

## Safety

- **Text is untrusted.** PR titles and brief text are stripped of control, bidi and invisible characters and cut before they enter the ledger, with the same cleaner `attention` uses. Opinion text names workers and repositories (validated names), never a title. GitHub's own error text is never echoed.
- **The briefs directory is workers' territory.** Symlinks are refused, files are opened without blocking (a FIFO is refused), reads are capped (256 KiB of `brief.md`, the last 4 KiB of each phase file, 500 worker directories, 50 repositories per brief), and repository names must be GitHub-shaped.
- **The desk is read, never written.** `sweep` opens it with the same read-only scan as `attention list`: nothing is pinned, moved, run or skipped, and a test pins the directory byte for byte.
- **The write is atomic.** `sweep` writes a fresh `O_EXCL` temporary file (`0600`) next to `sweep.json` and renames it over the target, so a reader sees the old file or the new one. A symlink at `sweep.json` is replaced, never followed. `$XDG_STATE_HOME/cadence` is shared with the attention spool, so it is not narrowed; `sweep` creates it at `0700` when absent and refuses one that is a symlink, another user's, or writable by group or others.
- **A failed read leaves the last ledger alone.** If GitHub cannot be read, `sweep` exits 1 before writing, so a half-empty ledger never replaces a good one. A herdr or desk failure degrades the ledger (see `sources`) and does not stop it.

## Exit codes

| Code | Meaning |
| ---: | --- |
| 0 | the ledger was written |
| 1 | GitHub or the briefs directory could not be read, or the write was refused |
| 2 | a key or flag is missing or unusable; nothing was read or written |

## Fixture

`internal/sweep/testdata/ledger_v2.json` is one run on a real host with every owner, repository, worker and PR title replaced by a synthetic one. A test holds the queue reader to accept it whole.
