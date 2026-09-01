# forgectl resume — get back into a Claude Code session after a terminal restart

> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).

```sh
forgectl resume                    # pick from recent sessions across every repo, then resume in place
forgectl resume forgectl           # filter by repo, name, cwd, or id; one hit resumes it, several list the candidates
forgectl resume --fork             # branch a new session off the transcript — the only way into a still-running one
forgectl resume --dry-run forge    # resolve and print the cwd + claude argv, exec nothing (never prompts)
forgectl resume ls                 # list without acting (the only subcommand that returns)
forgectl resume ls --json          # machine-readable JSON (safe to pipe; counts go to stderr; see `resume ls --help` for the field table)
forgectl resume snapshot           # capture what a live session's exit would destroy
forgectl resume snapshot --quiet   # same, silent — the form a Stop hook uses
```

A terminal restart costs three steps otherwise: find the folder, run `claude --resume`, then recognize the session in a picker that shows neither repo nor branch. `forgectl resume` collapses that to one command from a cold terminal — it lists recent sessions across *every* repo with name, repo, branch, and last activity, and lands you back inside the one you pick, in the right directory, with its task list restored.

Like `launch`, it **execs `claude` in place** (via `syscall.Exec`) and never returns; the resumed session is interactive, so there is no `-p`/`--print` form. From a script or an agent tool call, reach for `resume --dry-run` (prints the resolved cwd and argv, execs nothing) or `resume ls --json`. `resume ls` is the only member of the group that returns.

Against `launch`: `launch` starts or resumes a session in the *current* directory. `resume` is the cross-repo one — it finds a session anywhere on the machine and moves you to it.

**Claude Code stays the source of truth for identity.** Its prompt history and per-project transcripts already carry the session id, cwd, branch, and title indefinitely — so there is no daemon and no poller here, just a reader over artifacts that already exist. Two things genuinely do not survive a session's exit, and `resume snapshot` is what captures them:

- the `/rename` name, which lives only in the live-process registry;
- the task bodies, which Claude Code deletes when a session ends.

The snapshot record it writes does also carry recovery metadata — session id, cwd, version, timestamps, and the task-directory association — but that is a *cache with a known authority above it*, not a second source of truth: everything except the name, the tasks, and the task-directory pairing is re-derivable from Claude Code's own files.

Wire the capture to a `Stop` hook so every turn refreshes it. It is cheap, idempotent, and **always exits 0** — a failed snapshot must never become a failed turn.

**Merge this into the `hooks` object of `~/.claude/settings.json`** — user scope, since the whole point is cross-repo. **Do not replace that file**; it holds unrelated settings, and the fragment below is a fragment, not a document:

```json
"Stop": [
  { "hooks": [{ "type": "command", "command": "forgectl resume snapshot --quiet" }] }
]
```

`forgectl` must be on the hook's `PATH` — hooks do not inherit an interactive shell's environment, so use an absolute path if yours is not in the system `PATH`.

Because `snapshot` always exits 0, a hook that is missing, misspelled, or wired into the wrong settings file looks exactly like one that works — until a session exits and its tasks are gone. `forgectl doctor`'s `resume tasks` check is the detector: it warns when live sessions exist and the snapshot store is empty.

Snapshots live one JSON file per session in forgectl's config directory — `~/Library/Application Support/forgectl/resume-sessions/` on macOS, `~/.config/forgectl/resume-sessions/` on Linux — alongside every other forgectl store. The store self-prunes at most once a day, retiring a record when Claude Code's transcript for that session is gone — transcript existence, not a fixed age, because a snapshot is worth keeping exactly as long as `claude --resume` can still open the session, and transcript retention is operator-configurable. A record whose transcript survives is kept however old it is; the 180-day ceiling applies only to records whose transcript is already gone, so that a machine whose transcript lookup is broken still retires dead records eventually. A pass that would retire most of the store refuses and reports why, since that is the signature of a broken lookup rather than of that many dead sessions — and `FORGECTL_RESUME_NO_PRUNE` (any non-empty value) disables pruning entirely.

**Notes:**

- **An ambiguous filter lists the candidates instead of prompting** — with **exit 1**, the same code as "no session matched", because both recover the same way: change the filter. When more than one session matches and nobody can answer the picker — `--dry-run`, or any run without a terminal — `resume` prints one candidate row per line on stdout and exits 1 rather than opening a selector no one can see. That list is also the answer to "is this filter unique?", which a caller otherwise has no way to know before running the command: candidates on stdout means ambiguous, empty stdout means no match, exit 0 means it resumed.
- **A running session is refused, not continued** — with **exit 2**, distinct from the exit 1 used for "no session matched" or an ambiguous filter, because this is the one refusal a caller can act on. Two Claude Code processes on one transcript corrupts it, so `resume` errors out and names the live pid. `--fork` is the way in anyway: it branches a new session off the transcript, which only reads it. Pre-check with `resume ls --json`, which exposes `live` and `pid`.
- **`--fork` always forks, including on a session that is *not* running.** It is not a no-op safety flag: a fork starts a *new* session rather than continuing the old one, and a new session reads its own empty task list, so snapshotted tasks are reported rather than restored (its task directory is named after a session id that does not exist until after the exec). Pass it in response to the live-session error, not defensively.
- **The task store never shrinks to follow Claude Code.** Snapshots merge by task id and retain what they have already captured, so a later pass seeing fewer tasks never discards the earlier ones — dropping to the live set would throw away exactly what the feature exists to rescue. This is a property of *repeated* snapshots taken while the session is alive: `snapshot` walks the live-process registry, so it cannot discover a session that has already exited. Without a prior snapshot, running it after a crash recovers nothing — which is why the `Stop` hook, not manual invocation, is the intended wiring.
- **Restore never overwrites.** A task file the live session owns always wins, and `.highwatermark` is raised but never lowered, so a resumed session is never handed an id already on disk. Running it repeatedly is a no-op.
- **The resumed session gets its own project's posture.** `[launch]` profile resolution is a pure function of the config and a directory, so resuming into another repo picks up that repo's model, effort, permission mode, and `--add-dir` set for free.
- `forgectl doctor` carries a `resume tasks` check. Task rescue depends on Claude Code naming per-session task directories after the session id — verified behavior, not a guarantee — and the check warns if that ever stops being true, so rescue cannot silently degrade to writing where nothing reads.
