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
carrier and not descended into. That includes a repo's `.claude/worktrees/`:
worktrees parked there are part of the `.claude` carrier and are not scanned
separately. The scan skips `.git`.

### Anomaly flags

| Flag | Meaning |
| --- | --- |
| `vendored` | The carrier sits inside a dependency directory (`node_modules`, `bower_components`, `vendor`, `third_party`, `.venv`, `venv`, `site-packages`), so a package author wrote it: the supply-chain vector. |
| `off-root` | A carrier that belongs at a repo root (anything but the three nestable basenames) sits somewhere else: under a subdirectory, or outside any git working tree. |
| `recent` | The carrier's own mtime is within the last 7 days. A directory's mtime changes only when entries are added or removed directly inside it, and a fresh clone or checkout sets every file's mtime to that moment. |
| `symlink` | The entry is a symlinked directory whose name starts a multi-segment carrier (`.gemini` for `.*/mcp.json`, `.github` for `.github/instructions/`). The carrier, if any, lives behind the link. The scan never follows a link, so it reports the link itself, whether or not a carrier exists behind it. `quarantine` lists a carrier behind an in-root link and refuses one behind an escaping link. |

### Confinement and caps

The projects root itself is resolved once (`filepath.EvalSymlinks`) and opened
with `os.OpenRoot`. Below it, every filesystem call goes through that root:
each directory is listed with the root's `Open` plus `Readdirnames` (names
only), and each entry's type and mtime come from the root's `Lstat`. No
listing or stat can resolve outside the root, even if a symlink is swapped in
mid-scan. A symlink is reported as type `symlink` when its name matches a
class (or starts one, see the `symlink` flag). It is never followed.

The scan has three caps:

- **Entries (1,000,000) and carriers (10,000).** Hitting either stops the scan.
- **Depth (32 levels, the root being level 0).** Directories below the cap are
  skipped and the scan carries on.

Any cap sets `truncated` and adds its name (`entries`, `findings`, `depth`) to
`capped_by`. The text form ends with one note per cap that names it and says
whether the scan stopped. A directory the walk cannot list is counted in
`unreadable_dirs` and skipped.

Every path the text form prints is shown bare when it is ordinary. It is
escaped when it holds a control or format character, and cut in the middle
when it runs past 512 runes. `--json` escapes through forgectl's terminal-safe
encoder, which also escapes C1 controls and bidi overrides.

### JSON

```json
{
  "root": "/Users/me/Projects",
  "repos_scanned": 42,
  "entries_scanned": 183021,
  "unreadable_dirs": 0,
  "truncated": false,
  "capped_by": [],
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
- `capped_by` lists the caps hit (`entries`, `findings`, `depth`).
- `carriers`, `anomalies`, and `capped_by` are always arrays, never `null`.

### Exit codes

`0` when the scan ran, whatever it found (a hit or a truncated scan does not
fail it). `1` when the projects root cannot be resolved or opened. Under
`--json`, that failure writes one `{"error","code","path"}` object to stderr
(see [json-contract.md](../json-contract.md)).
