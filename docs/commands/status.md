# forgectl status — read-only overview across the workbench

> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).

```sh
forgectl status                          # one glyph-led line per section, a few detail rows
forgectl status --json                   # every section and row, for scripts
forgectl status --json --strict          # same, but exit 1 when any section degraded or failed
forgectl status --json --limit 20        # each list cut to 20 rows; a top-level "bound" says what was cut
forgectl status --limit 30               # the text view lists up to 30 rows per list instead of 10 and 5
forgectl status --timeout 5s             # give each section five seconds (default 20s)
forgectl status --tui                    # the cockpit: the same sections on one screen, refreshing in place
```

`forgectl status` puts four read paths that already ship into one view. It changes nothing.

| Section | Source | Same data as |
| --- | --- | --- |
| `git` | Working-tree state of every local project under the projects root (`$PROJECTS_DIR`, else `~/Projects`). Local only: it runs `git status` per project and never queries a forge. | the local clones in `projects list` |
| `prs` | Your active clean-room reviews, the PRs awaiting your review, and your own open PRs. The last two are `gh search prs` calls on the configured `[github]` host. | `pr dash --json` |
| `clean` | The dry-run reclaim total for dep/build directories under the `[clean]` root. | `clean --json`, totals only |
| `bench` | The hearth and chronicle health card. | `bench status --json` |

`status` itself makes no network calls. The only network traffic comes from the sources it reuses: the `prs` section's `gh` searches, and the `bench` section's probes, which are the same checks `bench status` makes: HTTP to `hearth.localhost` and `grafana.localhost`, and a dial to the configured `[bench] otlp_endpoint` (loopback by default, but it can name any host).

## Failure containment

The sections run concurrently, and each has its own deadline, set with `--timeout` (default 20s). A section fails when its source returns an error, panics on the goroutine that runs it, or misses the deadline. A source that returns after the deadline counts as missing it, even when it returned data, because sources read under a cancelled context can report ordinary-looking data such as an `unknown` tree. A source that returned before the deadline is judged on what it returned, however late the report reads it. A failed section is reported as failed, and the other sections still report. A failed section never fails the command.

`status` exits 0 whatever the sections report, like `bench status` and `pr dash`. With `--strict`, it writes the full report and then exits 1 when any section is not `ok`, like `projects list --strict`. Under `--json`, that exit adds nothing to stderr, because the report on stdout is the verdict ([json-contract.md](../json-contract.md)). A `--timeout` of zero or less is refused before any source runs (exit 1).

## Bounding the output with `--limit`

`--json` carries every row by default: `git.projects` lists every local clone and the three `prs` lists every PR (65 KB on a 138-clone machine). `--limit N` keeps the first N rows of each of those four lists and adds one top-level key, present only under `--limit` and written first, so a head-only read sees it:

```json
"bound": {
  "limit": 20,
  "truncated": true,
  "cut": [{"list": "git.projects", "total": 138, "shown": 20}],
  "hint": "cut to --limit rows; raise --limit (0 = every row), or read one list at a time: projects list --json, pr dash --json"
}
```

`cut` names only the lists that had more than N rows, so `"truncated": false` and an empty `cut` mean nothing was dropped. The `git` totals (`total`, `clean`, `dirty`, ...) still count every project.

**One rule for `--limit 0`:** it means every row, in the same shape as a bounded call. `status --json --limit 0` writes `"bound": {"limit": 0, "truncated": false, "cut": []}` and cuts nothing. A caller that passes `--limit` always gets one shape. Without `--limit` there is no `bound` key.

**What a cut keeps.** When `--limit` is above 0, `git.projects` is sorted attention-first (dirty, ahead or unreadable trees, then clean trees, then plain directories; discovery order within each group) before the cut. The text view lists exactly the first group, so both views keep the same projects. Without `--limit`, the order is unchanged. The three `prs` lists keep their own order.

Without `--json`, `--limit N` replaces the text view's caps (10 projects, 5 PRs) with N for each list; `--limit 0` lists every row. `--fields` exists only on `review` and `projects list`. `--limit` with `--tui` is refused (exit 1): the cockpit has its own layout. A negative `--limit` is a usage error (exit 1, code `usage_error` under `--json`). `--limit` does not shorten the run, since every section still runs under its `--timeout`.

## `--json` shape

```json
{
  "git":   {"state": "ok",       "error": "", "notes": [], "data": {…}},
  "prs":   {"state": "degraded", "error": "", "notes": ["awaiting-you: query failed (gh is not signed in to github.com; run gh auth login)"], "data": {…}},
  "clean": {"state": "failed",   "error": "timed out after 20s", "notes": [], "data": null},
  "bench": {"state": "ok",       "error": "", "notes": [], "data": {…}}
}
```

All four keys are always present. Each section has the same four fields, and every field is always present:

- **`state`** says whether the source could be read. It does not say whether what the source found is healthy.
  - `ok`: the source answered in full.
  - `degraded`: the source answered, but part of it could not be read. `notes` names the part.
  - `failed`: the source produced no data. `error` says why.

  A bench component that is down is still an `ok` section, and its `data` says `unavailable`.
- **`error`** is `""` unless `state` is `failed`. The text is escaped and capped at 200 characters.
- **`notes`** is `[]` unless `state` is `degraded`. Each note is a short categorical line, escaped and capped at 200 characters.
- **`data`** is `null` only when `state` is `failed`.

Each section's `data`:

- **`git`**: `{"root", "total", "clean", "dirty", "ahead", "unknown", "not_a_repo", "projects": [{"name", "path", "status"}]}`.
  - `status` is the object `projects list --json` carries per clone: `{"state", "modified", "untracked", "ahead"}`, where `state` is `ok`, `not-a-repo` or `unknown`.
  - `dirty` and `ahead` can count the same project.
  - `unknown` counts projects whose `git status` failed or could not be parsed. They are never counted as clean.
- **`prs`**: the `pr dash --json` document, `{"active_reviews", "awaiting_you", "your_open"}`. `notes` carries the degradation notes `pr dash` writes to stderr.
- **`clean`**: `{"root", "total_reclaimable_bytes", "reclaimable", "skipped"}`. `total_reclaimable_bytes` counts non-skipped targets only, as in `clean --json`. `clean --json` lists the targets.
- **`bench`**: the `bench status --json` report, `{"hearth", "chronicle"}`.

Under `--json`, a failure before any section runs (a bad flag, or a bad `--timeout`) follows the [stderr contract](../json-contract.md): stdout stays empty, and stderr gets one `{"error","code","path"}` object.

## Human view

```text
✓ git    42 project(s) under "/Users/me/Projects": 38 clean, 3 dirty, 1 ahead, 0 unknown
    "forgectl"  [3 modified]
    "hearth"  [status unknown]
! prs    1 active review(s), 3 awaiting you, 5 open by you
    cameronsjo/forgectl#42  fix(cli): cap the status rows
    note: your-open: query failed (gh is not signed in to github.com; run gh auth login)
✗ clean  failed: timed out after 20s
✓ bench  hearth ok, chronicle unavailable
    ✗ chronicle — chronicle status failed
```

The glyph is the section's `state`: `✓` ok, `!` degraded, `✗` failed. Colour follows the usual rules: none off a TTY or under `NO_COLOR`. The human view is an overview, so it caps its lists:

- `git` lists up to 10 projects that are dirty, ahead or unknown. Clean trees and plain directories are counted but not listed.
- `prs` lists up to 5 PRs awaiting your review.
- `bench` gives a reason line only for a component that is not ok.

Each list says how many rows it hid. Every name, title, path and reason is escaped for the terminal and capped. The JSON report carries every row in full.

The human view can change between releases. Scripts should read `--json`, whose shape only grows (ADR-0008).

## Cockpit (`--tui`)

`forgectl status --tui` shows the same four sections on one screen and refreshes them in place. The hub's `status` row opens it. The cockpit itself only reads. The one key that leads to an action is `enter` on a PR row: it quits the cockpit and hands off to `forgectl pr <ref>`, which starts a review.

```text
#  forgectl status  ·  git 2m ago · prs 2m ago · clean 2m ago · bench 2m ago
✓ git    3 project(s) under "/p": 1 clean, 1 dirty, 1 ahead, 0 unknown
    alpha                     [clean]
  > beta                      [2 modified]
    gamma                     [1 ahead]
! prs    0 active review(s), 2 awaiting you, 1 open by you
    note: your-open: query failed (gh is not signed in to github.com; run gh auth login)
    o/r#1                     awaiting you · fix the thing
    o/r#2                     awaiting you · add the other
    … 1 more (tab to this section)
✗ clean  failed: timed out after 20s
✓ bench  hearth ok, chronicle unavailable
    hearth                    ok — up
    chronicle                 unavailable — down

↑↓/jk move · tab section · enter open · r refresh · R all · / filter · ? help · q quit
```

- **Header.** Each section's age since its last result, `refreshing…` while a refresh runs, or `loading…` before the first result.
- **Sections.** Each section has the same glyph and headline as the text view, which come from the same functions. The focused section lists all of its rows and scrolls. The others show their first two rows.
  - `git` lists every local clone with its state. Plain directories are counted, not listed.
  - `prs` lists the PRs awaiting you, then your open PRs, then your active reviews.
  - `bench` lists each component with its reason.
  - `clean` has only its headline. `clean --json` lists the targets.
- **Keys.**

  | Key | Does |
  | --- | --- |
  | `↑` `↓` `j` `k` | Move in the focused section. |
  | `tab` `shift+tab` | Next or previous section. |
  | `enter` | On a PR row, quits and runs `forgectl pr <ref>`, which starts a review. The argv is built and checked the way the hub's picker builds it, and it runs after the cockpit has exited, as a hub-chosen command does. On a project row, it says focus is not available yet. |
  | `/` | Filters every section's rows on the text they show (name, state, title). `enter` keeps the filter, and `esc` clears it. |
  | `r` | Refreshes the focused section. |
  | `R` | Refreshes every section. |
  | `?` | Shows the key help. |
  | `q` `esc` `ctrl+c` | Quit. `esc` clears an active filter first. |

- **Refresh.** Opening the cockpit runs one full collection. After that, only `git` refreshes itself, every 60s; it reads local clones and makes no network call. `prs` (which calls `gh`), `clean` and `bench` refresh only when you press `r` or `R`. A section refreshes at most once every 15s, and only one refresh per section runs at a time. A section whose source is still running after its `--timeout` shows as failed, but stays `refreshing…`, and refuses another refresh, until that source has actually returned.

`--tui` needs a terminal on both stdin and stdout. Without one, it exits 1 and points at `--json`. It can't be combined with `--json` or `--strict`. `--timeout` still bounds each section. Every name, title, path, note and error is escaped for the terminal and capped.

Unlike the hub, the cockpit has no `1`–`9` keys. Its rows carry no visible numbers, and a digit on a PR row would start a review on a row nobody saw numbered.
