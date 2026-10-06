# forgectl menu — the hub's contents, without a TTY

> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).

```sh
forgectl menu            # the hub as plain text: status line, sections, every command and subverb
forgectl menu --json     # the same content as one JSON document
```

Bare `forgectl` opens the hub on a TTY. `forgectl menu` prints what that hub holds, so an agent or a script can read it without a terminal ([ADR-0008](../adr/0008-agent-contract.md)). It reads the same sources the hub reads and it lays out the same rows:

- **The status line.** The current project and branch come from reading the checkout's `.git` files. The live tmux session count comes from `tmux list-sessions`. Running and queued PR reviews come from the review store. All of these are local reads under the hub's short time budget, and none of them uses the network.
- **The sections.** First the pinned commands (`docs`, `pr`, `projects`, `tmux`, `sessions`). Then up to three recent commands, ranked by your shell history. Then every other command, sorted into four areas: `agents`, `repos`, `shell`, and `setup`.

On a TTY the hub shows the pinned rows, the recent rows, and one row per area, so the whole first screen fits an 80×24 terminal. Enter on an area lists its commands. The keys:

- **`1`–`9`** open the pinned rows, then the areas, in order: `1`–`5` and `6`–`9` with the commands forgectl ships. A keyed row only opens a list or the argument picker; it never runs a command. Recent rows and the first-run `init` row have no key, so a key always means the same row. Inside an area, a command's subcommands, or a search, `1`–`9` number the rows in order and move the cursor there; `enter` acts on the row.
- **`/`** searches every command and every runnable subcommand by name, including those inside areas; a subcommand shows under its full path (`pr findings list`). Inside an area it filters that area. It matches names, not descriptions: `forgectl menu | grep <word>` searches those. When nothing matches, the line under the list says so. `esc` clears the search before it backs out of a screen.
- **`enter`** opens the selected row. **`q`** or **`esc`** goes back one screen, and quits from the top.

The line under the list shows the exact `$ forgectl …` the selected row runs. When a description is too long for its row, the row cuts it at a word with `…` and the full text appears under that line when the terminal has rows to spare (20 or more). On a terminal too short for every row, the recent rows go first, then the section dividers, so at 16 rows or more (17 on a first run, when the `init` row shows) the pinned rows and the areas stay on screen. Shorter terminals page (the page number shows under the list), and `1`–`9` still reach every keyed row. The footer's enter hint says what enter does to the selected row: `enter open` opens its subcommands, an area, or the argument picker; `enter run` runs it (a row whose placeholders are all optional runs without them); `enter print command` leaves the hub and prints the command with its placeholders when a required argument is one the picker cannot take. Below 20×8 the hub says the terminal is too small instead of drawing, and takes no key but `q` or `esc`, which quit.

`menu` changes nothing and never runs a row. It needs no TTY and opens no screen. It exits 0.

## `--json` shape

```json
{
  "header": {
    "project": "forgectl",
    "branch": "main",
    "tmux_sessions": 3,
    "reviews": {"running": 1, "queued": 2}
  },
  "first_run": false,
  "pinned":   [row, …],
  "recent":   [row, …],
  "commands": [row, …]
}
```

Every key is always present. Fields may be added later, but existing fields do not change.

- **`header`.** These are the hub's status-line fields. A field whose source was unavailable or did not answer in time is `null`, never `0`. The review store is read only when it already exists.
  - **`project`** is the name of the checkout's directory. It is `null` outside a checkout. The value is escaped and capped at 200 characters.
  - **`branch`** is the checked-out branch, or `(detached)`. It is `null` when there is no project, and also when the name fails the ref-name check. That check is the one the hub header uses.
  - **`tmux_sessions`** is the number of live tmux sessions.
  - **`reviews`** is `{"running", "queued"}` from the PR review store.
  - The doctor result the hub header can show is not included. `forgectl doctor` does not record its result yet.
- **`first_run`** is `true` when there is no `config.toml` yet. In that case the hub shows a `forgectl init` row first.
- **`pinned`, `recent`, `commands`** are the hub's three sections, in the order the hub shows them. Each is an array and is never `null`. `commands` lists the areas' commands area by area; each row's `group` names its area.
  - `recent` holds command paths only. Each one is resolved against the registered commands, and no text from the history file is ever included.

Each `row` has this shape:

```json
{
  "command": "pr findings",
  "group": "",
  "argv": ["pr", "findings"],
  "description": "List or reclaim durable findings from local clean-room reviews",
  "usage": "forgectl pr findings",
  "needs_args": false,
  "leaves": [row, …]
}
```

- **`command`** is the command path, joined with spaces.
- **`group`** is the hub area a `commands` row sits under (`agents`, `repos`, `shell`, or `setup`). It is `""` on pinned, recent, and leaf rows.
- **`argv`** is what to run after `forgectl`.
- **`description`** is the command's one-line help text.
- **`usage`** is the full invocation, with its argument placeholders, such as `forgectl pr <ref>` or `forgectl docs list [dir|file ...]`.
- **`needs_args`** is `true` when running `argv` alone is a usage error, so you must append the argument yourself. That is the case when `usage` names a required positional (`<…>` outside any `[…]`), and also when the command's own argument check refuses no arguments. When it is `false`, the bare `argv` passes the argument check. Optional positionals appear in `usage` but do not set `needs_args`.
- **`leaves`** holds the subverbs the hub's drill-down shows, nested to any depth. A leaf is an array and is never `null`.

Some rows behave differently in the TTY hub: the `tmux` row opens the session jumper there, and the `status` row opens the cockpit. Those are screens, not commands, so `argv` here is just the command path. Arguments that the hub's picker would offer, such as project names, are not listed. `forgectl projects list --json` lists the projects.

A bad flag or a stray argument follows the [stderr contract](../json-contract.md): stdout stays empty, and stderr gets one `{"error","code","path"}` object.

## Text form

Without `--json`, `menu` prints the hub's status line, then one line per row under each section heading: `pinned`, `recent`, then one heading per area. Each line holds the row's usage and then its description, and each subverb is indented under its row. The text is plain, with no colour and no escapes, so it is safe to grep.

```text
forgectl · forgectl @ main · 3 tmux · 1 review running, 2 queued

pinned
  forgectl docs  Local markdown reader — render + serve an indexed doc set over loopback HTTP
    forgectl docs list [dir|file ...]  List the indexed docs, most-recently-modified first
  forgectl pr <ref>  Clean-room review of a pull request
    forgectl pr prs  List open PRs across your repos (authored, assigned, review-requested)
…

recent
  forgectl pr prs  List open PRs across your repos (authored, assigned, review-requested)

agents
  forgectl launch [harness args…]  Per-project launcher for Claude Code, Codex CLI, or Pi
…

setup
  forgectl doctor  …
```

## The picker opt-out

For a command maintainer, the annotation below matters for the TTY hub only. A command whose one positional fills **another CLI's subcommand slot** sets this cobra annotation:

```go
Annotations: map[string]string{"forgectl:hub-no-picker": "<why>"}
```

The hub then never offers its inline argument picker for that command. Its row prints the invocation for you to finish by hand, and the hub's argv builder refuses the command as a backstop. The annotation is needed because a typed value such as `update` would otherwise reach the other CLI as one of its subcommands. Commands that take a whole argv, such as `launch [harness args…]`, are already kept off the picker by their variadic placeholder.
