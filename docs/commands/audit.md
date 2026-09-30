# audit

Read-only security posture scans across the projects root (`$PROJECTS_DIR`,
else `~/Projects`, the same root `forgectl projects` walks). No `audit` verb
writes anything. Bare `forgectl audit` lists its scans.

```bash
forgectl audit injection          # every agent-instruction carrier, grouped by repo
forgectl audit injection --json   # the same, machine-readable
```

## `audit injection`

This scan maps the prompt-injection surface: every file or directory an agent
reads instructions from. It finds exactly the classes `quarantine` hides and the
clean-room `strip` step removes, because it classifies against the same list
(`quarantine.DefaultTargets`). When that list gains a class, this inventory
reports it with no change of its own. The classes today are:

- `CLAUDE.md`, `CLAUDE.local.md`, and `AGENTS.md`, found in any directory.
- `.claude/`, `.mcp.json`, `.cursor/`, `.cursorrules`, and
  `.github/instructions/`, which belong at a repo root.
- MCP configuration directly inside a dot-directory (`.*/mcp.json`,
  `.*/.mcp.json`).

Matching ignores ASCII case, as quarantine's does.

It reports paths, types, and modification times. It never reads a file's
contents. A matched directory (`.claude/`, `.cursor/`) is reported as one
carrier and not descended into. The scan skips `.git`.

### Anomaly flags

| Flag | Meaning |
| --- | --- |
| `vendored` | The carrier sits inside a dependency directory (`node_modules`, `bower_components`, `vendor`, `third_party`, `.venv`, `venv`, `site-packages`), so a package author wrote it: the supply-chain vector. |
| `off-root` | A carrier that belongs at a repo root (anything but the three nestable basenames) sits somewhere else: under a subdirectory, or outside any git working tree. |
| `recent` | The carrier's own mtime is within the last 7 days. A directory's mtime changes only when entries are added or removed directly inside it. |

### Confinement and caps

The walk runs through an `os.Root` opened on the resolved projects root. A
symlink is reported as type `symlink` when its name matches a class, and it is
never followed, so the scan cannot leave the root. The walk stops at 1,000,000
directory entries, 10,000 carriers, or 32 directory levels, and sets
`truncated`. The text form then ends with a note that the list is incomplete. A
directory the walk cannot list is counted in `unreadable_dirs` and skipped.

Every path the text form prints is shown bare when it is ordinary, and
escaped and capped when it holds a control or format character. `--json`
escapes through forgectl's terminal-safe encoder.

### JSON

```json
{
  "root": "/Users/me/Projects",
  "repos_scanned": 42,
  "entries_scanned": 183021,
  "unreadable_dirs": 0,
  "truncated": false,
  "carriers": [
    {
      "path": "/Users/me/Projects/github.com/me/app/CLAUDE.md",
      "repo": "/Users/me/Projects/github.com/me/app",
      "target": "CLAUDE.md",
      "type": "file",
      "modified": "2026-09-28T14:03:11Z",
      "anomalies": ["recent"]
    }
  ]
}
```

- `target` is the quarantine list entry the carrier matched, spelled as the
  list spells it (`.claude/`, `.*/mcp.json`).
- `type` is `file`, `dir`, `symlink`, or `other`.
- `repo` is the nearest enclosing git working tree, or `""` outside one.
- `modified` is RFC 3339 UTC, or `""` when the entry could not be stat'd.
- `carriers` and `anomalies` are always arrays, never `null`.

### Exit codes

`0` when the scan ran, whatever it found (a hit or a truncated scan does not
fail it). `1` when the projects root cannot be resolved or opened. Under
`--json`, that failure writes one `{"error","code","path"}` object to stderr
(see [json-contract.md](../json-contract.md)).
