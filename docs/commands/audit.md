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
| `symlink` | The entry is a symlink at a repo root whose name starts a multi-segment carrier (`.gemini` for `.*/mcp.json`, `.github` for `.github/instructions/`). The carrier, if any, lives behind the link. The scan never walks a link, so it reports the link itself, whether or not a carrier exists behind it. It checks the link's target only with a stat confined to the projects root: a link to a file or to nothing (`.env`, `.eslintrc`, a dangling link) is skipped. A link it cannot check stays reported: one that leaves the projects root, or one with an absolute target, which the confined stat refuses to resolve. `quarantine` lists a carrier behind an in-root link and refuses one behind an escaping link. The check runs only at a repo root: a symlinked `.gemini` below a repo root, or outside any git working tree, is neither reported nor walked, while a real `.gemini/mcp.json` in the same place is reported with `off-root`. |

### Confinement and caps

The projects root is opened once with `os.OpenRoot`. Below it, every
filesystem call goes through that root:

- Each directory is opened with `O_DIRECTORY|O_NONBLOCK` (where the platform
  has them) and listed with `Readdirnames` (names only). A directory swapped
  for a FIFO mid-scan fails at once rather than hanging the scan.
- Each entry's type and mtime come from the root's `Lstat`.
- The one symlink-following check (is a `symlink`-flagged link a directory?)
  is the root's `Stat`, which refuses a target outside the root.

No listing or stat can resolve outside the root, even if a symlink is swapped
in mid-scan. A symlink is reported as type `symlink` when its name matches a
class (or starts one, see the `symlink` flag). It is never walked.

Paths are reported in your spelling of the root: `$PROJECTS_DIR` or
`~/Projects`, made absolute but not symlink-resolved. `root`, every `path`,
and every `repo` share that one prefix, even when the root sits under a
symlinked directory (on macOS, anything under `/var` or `/tmp`). The root is
opened once, so if it is a symlink repointed mid-scan, the scan finishes in the
directory it opened, but every reported path uses your spelling, which by then
names the new target.

The scan has three caps:

- **Entries (1,000,000) and carriers (10,000).** Hitting either stops the scan.
- **Depth (32 levels, the root being level 0).** Directories below the cap are
  skipped and the scan carries on.

Any cap sets `truncated` and adds its name (`entries`, `findings`, `depth`) to
`capped_by`. The text form ends with one note per cap that names it and says
whether the scan stopped, using the caps it ran with. `depth_skipped` counts
the directories the depth cap left unscanned. A directory the walk cannot
list, or an entry it cannot `Lstat`, is counted in `unreadable_dirs` and
skipped.

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
  "depth_skipped": 0,
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
- `capped_by` lists the caps hit (`entries`, `findings`, `depth`), and
  `depth_skipped` counts directories left unscanned at the depth cap.
- `carriers`, `anomalies`, and `capped_by` are always arrays, never `null`.

### Exit codes

`0` when the scan ran, whatever it found (a hit or a truncated scan does not
fail it). `1` when the projects root cannot be resolved or opened. Under
`--json`, that failure writes one `{"error","code","path"}` object to stderr
(see [json-contract.md](../json-contract.md)).
