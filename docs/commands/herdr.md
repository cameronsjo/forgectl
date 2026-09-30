# herdr

forgectl's native helpers for the [herdr](https://github.com/cameronsjo/herdr) terminal multiplexer. Run them from a herdr pane. The client they share is described in [../herdr.md](../herdr.md).

```bash
forgectl herdr organize             # report how tabs would be grouped and ordered (changes nothing)
forgectl herdr organize --explain   # also show which rule caught each tab, and why unmatched tabs matched nothing
forgectl herdr organize --json      # the plan as one JSON object on stdout; the report goes to stderr
```

## Requirements

- **A herdr pane.** The command needs `HERDR_ENV=1` and a `HERDR_SOCKET_PATH` naming a live socket, which herdr sets in every pane it hosts.
- **Rules in `config.toml`.** `forgectl herdr organize` refuses to run with none (exit 2) and says what it found. `forgectl init` adds a commented `[herdr.organize]` section.
- **The `cameronsjo/herdr` fork, to move tabs.** Upstream herdr has no `tab move`. The report only lists, so it works on stock herdr.

## organize

Each tab goes to the workspace of the first rule whose glob matches `"<cwd> :: <title>"` for one of its panes (first pane first). A tab no rule matches goes to `default`. A tab's identity is the terminal id of its first pane, which stays stable when herdr renumbers tab ids on a move.

Within a workspace, tabs are ordered by wing, then repo, then cwd, then tab id. Wing and repo are the first two path parts under the projects root (`$PROJECTS_DIR`, else `~/Projects`). A worktree path (`.../.claude/worktrees/...`) sorts with its repo. A cwd outside the root sorts last. A repo filed as `<root>/<host>/<owner>/<name>` sorts by host and owner.

Nothing is closed or renamed.

### Configuration

```toml
[herdr.organize]
default = "misc"                     # workspace for tabs no rule matches; required once a rule exists
workspace_order = ["forge", "misc"]  # left-to-right order

[[herdr.organize.rule]]
glob      = "*/Projects/forge/* :: *"
workspace = "forge"
```

- `glob` follows shell rules: `*` matches any run of characters including `/`, `?` matches one, `[abc]`, `[a-c]` and `[!x]` match a class, and everything else is literal. Matching is case-sensitive.
- `workspace` names a workspace by label. An existing workspace with that label is used (the lowest-numbered one when labels repeat, with a warning). A label with no workspace is created.
- `workspace_order` lists labels left to right. A label with no workspace is skipped. A workspace it does not list keeps its relative order after the listed ones. A label listed twice is a config error.

### Reading the report

| Line | Meaning |
|---|---|
| `move` | this tab would move to another workspace |
| `blocked` | the move would leave its workspace with no tabs, and herdr refuses that; open another tab there or move it by hand |
| `order` | this tab would change position, or the workspaces would reorder |
| `unmatched` | tabs no rule matched, filed under `default` (`--explain` shows each one's match key) |
| `organized: N tabs in M workspaces; nothing to do` | nothing is pending |

When moves are pending the report says `tab order will be rechecked after the moves`: the order within a workspace can only be known once the moves have run.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | done, or a report, including moves it predicts herdr will block |
| 1 | herdr failed or declined a call |
| 2 | not in a herdr pane, no rules configured, an invalid config, or a usage error |

### Moving from the `forgectl-herdr` script

forgectl does not read `~/.config/herdr-organize/rules.toml` or `HERDR_ORGANIZE_RULES`. Move the script's keys into `config.toml`: `default` and `workspace_order` go under `[herdr.organize]`, and each `[[rule]]` becomes `[[herdr.organize.rule]]`. Once this command ships, `forgectl herdr` shadows the script for every verb; the script only had `organize`.
