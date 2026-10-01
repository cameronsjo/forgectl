# audit

Read-only security posture scans across the projects root (`$PROJECTS_DIR`,
else `~/Projects`, the same root `forgectl projects` walks). No `audit` verb
writes anything. Bare `forgectl audit` lists its scans.

```bash
forgectl audit injection          # every agent-instruction carrier, grouped by repo
forgectl audit injection --json   # the same, machine-readable
forgectl audit secrets            # stray .env files, private keys, gitleaks findings
forgectl audit secrets --json     # the same, machine-readable
```

Both scans walk the tree the same way (see
[Confinement and caps](#confinement-and-caps)): one `os.OpenRoot`, no symlink
followed, `.git` skipped, and the same three caps.

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

## `audit secrets`

This scan looks for secret-hygiene problems. Its native checks always run.
When `gitleaks` is installed, it also scans each repo's working tree with
`gitleaks dir`. The first line of the text output, and the `gitleaks` object
in `--json`, always say what gitleaks did.

```bash
forgectl audit secrets                       # native checks, plus gitleaks when installed
forgectl audit secrets --gitleaks=off        # native checks only
forgectl audit secrets --gitleaks=require    # fail unless gitleaks ran
forgectl audit secrets --gitleaks-timeout=2m # deadline for the whole gitleaks pass (default 10m)
```

### Native checks

These read metadata only, with one exception: a `*.pem` or `*.key` file is
opened through the root and its first 4 KiB checked for `PRIVATE KEY-----`.
The check keeps a yes or no and nothing of the bytes. The open never blocks on
a FIFO, and anything that is not a regular file once opened is refused.

| Kind | What matches (by basename, ASCII case-insensitive) |
| --- | --- |
| `env` | `.env`, `.env.*` and `.envrc`. A name ending `.example`, `.sample`, `.template`, `.tmpl` or `.dist` is a template and is skipped. A directory is never a match (a Python virtualenv is often named `.env`), and it is walked. |
| `key` | `id_rsa`, `id_dsa`, `id_ecdsa`, `id_ed25519`, `id_ecdsa_sk` and `id_ed25519_sk` (not `.pub`); any `*.p12`, `*.pfx` or `*.keystore`; a `*.pem` or `*.key` only when the 4 KiB check finds a private-key header. |
| `scanner-config` | `.gitleaks.toml` and `.gitleaksignore`. A repo can use either to hide findings from a scanner, so their presence is reported. |

A symlink whose name matches is reported as type `symlink` and is never read or
followed, whatever it points at. A FIFO or device named like a `.pem` or `.key`
is not read and not listed.

Whether a file is tracked or ignored comes from one `git ls-files -z -t --cached
--others --exclude-standard` call per repo (batched for long path lists), run
through forgectl's hardened git profile, with every path a `:(literal)`
pathspec. **A `.env` file git ignores is counted (`ignored_env_files`), not
listed**: that is the normal development pattern. A key is listed whether or
not it is ignored.

| Flag | Meaning |
| --- | --- |
| `tracked` | git tracks the file, so it is in the repo's history. |
| `unignored` | An untracked `.env` that no ignore rule covers: `git add .` would commit it. |
| `outside-repo` | A `.env` outside any git working tree. |
| `git-unknown` | git could not report on the repo (not a usable repository, `safe.directory`, no git). The file is listed rather than assumed ignored. |
| `vendored` | Inside a dependency directory (the same list `audit injection` uses). |
| `loose` | Readable or writable by group or others (`mode & 0o077`, the rule ssh applies to a private key). Set for `env` and `key` regular files on unix only; Windows permission bits are synthesized. |
| `foreign-owner` | A key owned by a uid other than the one running the scan (unix). |

A `*.pem` or `*.key` file that cannot be opened is counted in
`unreadable_files`, neither listed nor ruled out.

### gitleaks

`--gitleaks` is `auto` (the default: run it when a usable one is installed),
`off`, or `require` (exit 1 unless it ran). A usable gitleaks is:

- found on `PATH` once, as an absolute path. A hit through a relative `PATH`
  entry (Go's `exec.ErrDot`) is refused, and so is a binary inside the projects
  root, compared both as spelled and with symlinks resolved, since a cloned
  repo could have put it there;
- version 8.19.0 or later, read from `gitleaks version`. 8.19.0 introduced the
  `dir` subcommand. A version forgectl cannot read counts as too old.

It runs once per repo the walk found, in path order, under one deadline for
the whole pass (`--gitleaks-timeout`, default 10m) that kills the process group
when it passes:

```text
gitleaks dir --config <tmp>/cfg.toml --gitleaks-ignore-path <tmp>
  --report-format json --report-path <tmp>/report.json --redact --exit-code 0
  --no-banner --log-level error --max-target-megabytes 5 -- <repo>
```

- `<tmp>` is a fresh 0700 temp directory, removed afterwards. `cfg.toml` holds
  only `[extend] useDefault = true`: gitleaks' built-in rules. gitleaks reads
  `--config` ahead of a scanned repo's own `.gitleaks.toml`, so a repo cannot
  replace or allowlist the rules. `GITLEAKS_CONFIG` and `GITLEAKS_CONFIG_TOML`
  are removed from gitleaks' environment.
- `--gitleaks-ignore-path` points at the temp directory, so the
  `.gitleaksignore` of whatever directory you run forgectl from is not read.
- Only `dir` mode runs. History is not scanned: `gitleaks git` would run git
  without forgectl's hardening. Symlinks are not followed (no
  `--follow-symlinks`). Files over 5 MB are skipped.
- From the JSON report forgectl decodes only `RuleID`, `File`, `StartLine` and
  `Fingerprint`. `Secret`, `Match` and `Line` are never decoded, and every
  text row also passes through forgectl's credential redaction as a
  backstop. A report is read up to 32 MiB, and at most 10,000 findings are kept
  across the pass (`truncated`). A finding whose `File` is not inside the repo
  it was reported for is dropped and counted in `findings_rejected`. A finding
  in a nested repo, which its parent's scan also reports, is listed once,
  under the innermost repo.

**Limits.** gitleaks covers files inside git working trees only, not the rest
of the projects root. A scanned repo can still hide a finding from it: gitleaks
always reads the repo's own `.gitleaksignore`, and honors a `gitleaks:allow`
comment on the line. The native scan reports every `.gitleaksignore` and
`.gitleaks.toml` (`scanner-config`) so that hiding is visible. History is not
scanned.

`forgectl doctor` has a `gitleaks` row from the same resolver: skipped when
gitleaks is absent or found only through a relative `PATH` entry, a warning
when it is too old, inside the projects root, or `gitleaks version` fails, and
OK with its version otherwise.

### JSON

```json
{
  "root": "/Users/me/Projects",
  "repos_scanned": 42,
  "entries_scanned": 183021,
  "unreadable_dirs": 0,
  "unreadable_files": 0,
  "ignored_env_files": 17,
  "git_status_failed_repos": 0,
  "truncated": false,
  "capped_by": [],
  "depth_skipped": 0,
  "findings": [
    {
      "path": "/Users/me/Projects/app/.env",
      "repo": "/Users/me/Projects/app",
      "kind": "env",
      "type": "file",
      "flags": ["tracked", "loose"]
    }
  ],
  "gitleaks": {
    "mode": "auto",
    "status": "ran",
    "reason": "",
    "version": "8.30.1",
    "min_version": "8.19.0",
    "path": "/opt/homebrew/bin/gitleaks",
    "repos_scanned": 42,
    "repos_failed": 0,
    "findings_rejected": 0,
    "truncated": false,
    "findings": [
      {
        "path": "/Users/me/Projects/app/src/config.go",
        "repo": "/Users/me/Projects/app",
        "rule": "github-pat",
        "line": 12,
        "fingerprint": "/Users/me/Projects/app/src/config.go:github-pat:12"
      }
    ]
  }
}
```

- `kind` is `env`, `key`, or `scanner-config`; `type` is `file`, `symlink`, or
  `other`; `flags` are listed in the order of the table above.
- `repos_scanned` counts the git working trees the walk found; the gitleaks
  block's `repos_scanned` counts those gitleaks scanned successfully.
- `gitleaks.status` is `ran`, `failed`, or `timed_out` when gitleaks ran;
  `absent`, `refused`, `too_old`, or `version_failed` when it was found
  wanting; `off` under `--gitleaks=off`. `reason` explains `refused`
  (`relative_path` or `under_scan_root`).
- `capped_by`, `truncated` and `depth_skipped` mean what they do for
  `audit injection`; the findings cap counts native findings before ignored
  `.env` files are dropped.
- Every array is always an array, never `null`.

### Exit codes

`0` when the scan ran, whatever it found. `1` when the projects root cannot be
resolved or opened; when gitleaks was found but failed, timed out, or its
`gitleaks version` failed (under `auto` as well: the scan you asked for did not
complete); or under `--gitleaks=require` when gitleaks did not run. Absent,
refused or too old under `auto` exits `0`: the scan is the native-only one
`auto` promises, and the first line says so. `2` for a bad `--gitleaks` or
`--gitleaks-timeout` value. Under `--json` the report is still written to
stdout when the exit is `1`.
