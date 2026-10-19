# Operator runbook: repository goals

This runbook covers one bounded change in an existing local Git checkout. It
does not install, commit, push, merge, deploy, or clean up the result.

## Before starting

Use a dedicated checkout or worktree and build from its repository root. Keep
the source revision visible for the approval record:

```sh
cd /path/to/forgectl
git status --short                 # a clean build has no output
git rev-parse HEAD                 # record the full object ID
go build .                         # build the repository root
go version -m ./forgectl           # inspect vcs.revision and vcs.modified
/path/to/forgectl/forgectl --help
```

Use the resulting absolute executable path when another `forgectl` may be on
`PATH`; a clean committed build reports the intended `vcs.revision` and
`vcs.modified=false`. If those values do not match, stop: build from a clean
standalone clone checked out at the recorded commit and inspect provenance
again. Another checkout's revision is not evidence for the intended build.
The operator needs a terminal, Git, and a locally authenticated native
harness for every selected role. Confirm native logins with each harness's
normal local status flow; never put credentials, auth files, or secret values
in the goal JSON, command arguments, or diagnostic output. A native team is
experimental: inspect its retained review, checks, and cleanup evidence.

Use only a repository whose Git configuration and attributes you trust.
Preparation and source snapshots use host Git, outside the tool container.
The local Git profile disables transports, prompts, fsmonitor and standard
hook directories, but it does not disable every repository filter driver or
newer configured hook. Container isolation is not a sandbox for that host
Git preparation.

Keep the source checkout separate from the owner-private goal registry. With
no `--root`, the registry is `$XDG_STATE_HOME/forgectl/projects`, falling back
to `$HOME/.local/state/forgectl/projects`;
for portable operation, choose an absolute private root explicitly:

```sh
/path/to/forgectl/forgectl projects goals \
  --root /path/to/private/goal-registry
```

The generated linked worktree is a sibling of the registry under
`/path/to/private/goal-registry-worktrees/<goal-id>`. The retained run
directory is under the registry at `goals/<goal-id>`. They are separate: the
worktree is the implementation checkout, while the run directory holds state,
receipts, logs, and artifacts.

## CLI contract

The validated binary exposes these commands and flags:

```text
forgectl projects goals [--model MODEL] [--root PATH]
forgectl projects goal prepare --file PATH [--root PATH] [--json]
forgectl projects goal start --file PATH --expect-head SHA --approve \
  [--root PATH] [--json]
forgectl projects goal list [--root PATH] [--json]
forgectl projects goal status GOAL_ID [--root PATH] [--json]
forgectl projects goal respond GOAL_ID --question QUESTION_ID --answer-file PATH \
  [--root PATH] [--json]
forgectl projects goal cancel GOAL_ID [--root PATH] [--json]
forgectl projects goal artifact GOAL_ID \
  [--kind KIND] [--index N] [--root PATH] [--json]
```

Use `--root` after `goal` for the singular CLI tree. `--answer-file` accepts a
bounded regular file or `-` for stdin. `--index` is zero-based and applies to
`--kind check`. State-producing commands should use `--json`; the stable
machine-readable shapes are:

- `prepare --json`: `{"version":1,"prepared":{...}}`, where `prepared.spec.base_revision` is the exact source HEAD.
- `list --json`: `{"version":1,"states":[...]}`.
- `start` and `status --json`: one version-1 state object containing `id`, `phase`, `run_dir`, `worktree`, `branch`, `supervisor_active`, `cleanup_complete`, `question`, `checks`, and optional `result`/`error` fields.
- `respond` and `cancel --json`: `{"version":1,"goal_id":"goal-...","sequence":N,"action":"respond|cancel",...}`. A response also carries `question_id` and the bounded answer.
- `artifact --json`: `{"version":1,"kind":"patch|check|json","goal_id":"goal-...","content":"..."}`. `patch` and `check` verify retained bytes against their recorded SHA-256; `json` serializes state and has no separate artifact-blob digest.

`list`, `status`, and `artifact` only read retained state. They do not start,
repair, replay, or resume a goal.

## TUI quickstart

Run `projects goals` from the repository you intend to change. The one-form
flow asks for:

1. A task preset: **Bug fix**, **Feature**, **Docs**, or **Custom**. Presets
   suggest scope, acceptance, and checks; they do not run detection commands or
   install dependencies.
2. A team: **Codex trio**, **Mixed native team**, or **Custom team**. Codex trio is the
   legacy all-Codex path. Mixed native assigns Codex to coordination, Pi to
   implementation, and Claude Code to independent review. Custom exposes
   `harness`, `provider`, and exact `model` for each role.
3. **Advanced** values when needed: title, literal allowed paths, acceptance
   criteria, verification argv, exact model, role assignments, duration, and
   container socket/image when isolation uses a Linux container. **Isolation**
   is a top-level field: choose **Process-group
   cleanup** or **Linux container**; the latter requires **Advanced** →
   **Container socket** and **Container image**.

The form's approval summary shows the repository, exact base revision, dirty
source warning, model/team, allowed scope, acceptance, and commands. Preparing
does not start a model. Start is a separate explicit confirmation; changing a
preset, role, model, provider, or other prepared field invalidates the approval.
The duration is 1–30 minutes and is a deadline, not a token or spending cap.

Checks are JSON argument arrays, never shell strings:

```json
[["/usr/bin/git", "diff", "--check"]]
```

Each inner array is executed directly. Pipes, redirects, expansion, and shell
fallbacks are not supported. Allowed paths are literal repository-relative
files or directory prefixes; `.` explicitly permits repository-wide source
changes, while Git metadata, agent metadata, and `.env` files remain excluded.

### Two bounded examples

Documentation task, limited to one file and a whitespace check:

```json
{
  "version": 1,
  "title": "Document widget setup",
  "objective": "Add the approved setup walkthrough to the widget guide.",
  "repository": "/path/to/repository",
  "base_revision": "",
  "allowed_paths": ["docs/widget.md"],
  "acceptance": ["The guide contains the setup steps and rollback note."],
  "checks": [["/usr/bin/git", "diff", "--check"]],
  "model": "EXACT_OPENAI_MODEL_ID",
  "limit_minutes": 10
}
```

Narrow code task, limited to an implementation file and its regression test:

```json
{
  "version": 1,
  "title": "Reject malformed parser delimiters",
  "objective": "Fix the delimiter regression and add the focused regression test.",
  "repository": "/path/to/repository",
  "base_revision": "",
  "allowed_paths": ["internal/parser/parse.go", "internal/parser/parse_test.go"],
  "acceptance": ["Malformed delimiters are rejected", "The focused regression test passes"],
  "checks": [["/path/to/go", "test", "./internal/parser"]],
  "model": "EXACT_OPENAI_MODEL_ID",
  "limit_minutes": 15
}
```

Replace `/path/to/go` with the actual absolute Go executable. These examples
illustrate the contract; prepare them against the real checkout and review the
returned source revision before starting.

## Optional Linux tool isolation

Container isolation is an opt-in tool/check boundary and requires an explicit
native team, even when all three roles use Codex. Build and inspect the helper
image through the same explicit Docker Unix socket used by the goal; do not
use an ambient Docker context or a tag as the approval identity:

```sh
cd /path/to/forgectl
docker --host unix:///path/to/docker.sock \
  build -f Dockerfile.goal-tools -t forgectl-goal-tools:local .
docker --host unix:///path/to/docker.sock \
  image inspect --format '{{.Id}}' forgectl-goal-tools:local
```

Put the resulting immutable lowercase `sha256:` image ID in the goal. Because
container isolation requires an explicit native team, add a top-level `team`
object beside `isolation`; do not add `isolation` to a legacy goal that has no
`team`:

```json
"team": {
  "coordinator": {"harness": "codex", "provider": "openai", "model": "EXACT_CODEX_MODEL_ID"},
  "implementation": {"harness": "pi", "provider": "openai-codex", "model": "EXACT_PI_MODEL_ID"},
  "reviewer": {"harness": "claude", "provider": "anthropic", "model": "EXACT_CLAUDE_MODEL_ID"}
}
```

Replace each model token with the exact locally supported ID before preparing.
The native harnesses must be installed and authenticated locally.

```json
"isolation": {
  "mode": "container",
  "socket": "/path/to/docker.sock",
  "image": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}
```

The engine must already be a trusted local Linux Docker Engine with API 1.51,
cgroup v2, memory/PID limits, and built-in seccomp. Preparation checks socket,
engine, image identity/architecture, declared volumes, and environment
defaults; it does not run the helper. Start then runs the fixed
`/usr/local/bin/forgectl` helper and a mount/UID boundary probe for each role
before any native model work. It never pulls or builds an image, starts a VM,
or falls back to host commands.

The recipe provides Linux `forgectl`, Git, Python 3, and a POSIX shell. A check
requiring another toolchain needs a separately trusted image build; the host
toolchain is never substituted. On macOS with a Linux VM, every mounted
worktree, role-runtime, Git metadata, and result-patch path must be in a
directory shared with that VM. The registry/control directory, broker sockets,
and Docker socket are not mounted into the container. Do not assume `/tmp` is
shared merely because the host can `stat` it.

The boundary distinction matters: native **MODEL** processes remain outside
containers and receive process-group cleanup only. Each **TOOL** and approved
check runs in a fresh Linux container whose PID namespace contains all of that
operation's tool descendants. Each operation is limited to 120 seconds or the
remaining goal deadline, whichever is sooner, and container networking is
disabled. Approved checks therefore need Linux-compatible argv, the helper
image's available toolchain, and no network access. Container cleanup does not
contain daemonized native-model descendants.

## Approval and result

For the scripted path, prepare and inspect the revision, then bind the exact
full SHA to an explicit start:

```sh
/path/to/forgectl/forgectl projects goal prepare \
  --file /path/to/goal.json --root /path/to/private/goal-registry --json
/path/to/forgectl/forgectl projects goal start \
  --file /path/to/goal.json \
  --expect-head FULL_SHA_FROM_PREPARE --approve \
  --root /path/to/private/goal-registry --json
```

`prepare` is the no-model boundary. `start` is the explicit native model and
potential provider-cost boundary; `limit_minutes` is not a spending limit.
If the source HEAD changes, prepare again and review the new value. No native
provider, harness, or toolchain fallback is used.

`ready` means the supervisor froze a patch, an independent native review passed,
every approved check passed, the reviewed patch stayed unchanged, and the
original source checkout stayed unchanged. It does **not** mean human accepted,
committed, merged, or deployed; `human_accepted` remains false. Review the
patch, check output, source snapshot, native responses, and cleanup receipts
before deciding whether to apply anything manually.

There is no accept, apply, or merge command in this lifecycle. For automation,
read patch content through `artifact --kind patch --json` and decode the
`content` string without adding or changing bytes. The plain-text artifact
view is terminal-safe display output, not a byte-preserving patch export.
