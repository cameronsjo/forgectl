# forgectl pr — the clean-room reviewer's own posture

> Part of [forgectl](../../README.md) — see the [command roster](../../README.md#command-groups).

```sh
forgectl pr <ref>                        # prepare + launch an isolated, deny-by-default review (owner/repo#N, a PR URL, or a bare N)
forgectl pr <ref> --dry-run              # resolve + print the plan, create nothing
forgectl pr prs                          # cross-repo open PRs (authored, assigned, review-requested); reviewed rows dimmed
forgectl pr prs --json                   # machine-readable JSON (safe to pipe; notes go to stderr)
forgectl pr dash                         # dashboard: active reviews, PRs awaiting you, your open PRs
forgectl pr pick                         # multiselect with both descriptors TTY; otherwise sanitized owner/repo#N rows on stdout + exit 1
forgectl pr reviewed mark <ref>          # mark a PR reviewed (dims it until the PR sees new activity)
forgectl pr reviewed unmark <ref>        # clear a PR's reviewed mark
forgectl pr reviewed sync                # prune reviewed marks for PRs that are no longer open
forgectl pr list                         # list active clean-room review sessions
forgectl pr attach <breadcrumb>          # jump to a review window (also: open <b>, teardown <b>)
                                          #   <breadcrumb> is the session path `pr list` prints
forgectl pr keys                         # tmux cheatsheet for driving a review
```

When both stdin and stdout are terminals, `pr pick` keeps its existing picker.
Headless `pr pick` emits sanitized `owner/repo#N` rows and exits 1; each
printed ref is directly usable with `forgectl pr <ref>`, while `pr prs --json`
remains the stable inventory.

The `[pr]` section configures `forgectl pr` independently of whatever repo a review happens to land in:

```toml
[pr]
max_concurrent = 4         # live "pr-*" tmux windows allowed at once; <= 0 = default (4)
model  = "opus"            # reviewer model; unset = the ambient launch profile's
effort = "xhigh"           # reviewer effort; unset = derived from the reviewer model
```

`model` and `effort` exist because a review's posture should follow *the review*, not the checkout it is pointed at. Setting `model` **discards** the ambient repo's effort and re-derives from the new model — otherwise reviewing a sonnet-pinned repo with `[pr] model = "opus"` would ship `--model opus --effort high`, the exact mispairing the knob exists to prevent. Setting `effort` overrides everything.

**These two keys are the whole surface, by design.** The clean-room review always runs with `--permission-mode plan`, never `--allow-dangerously-skip-permissions`, always `--strict-mcp-config`, and with your ambient `add_dir` dropped — because the workspace under review may hold a third party's checkout. That posture is forgectl's control, not an operator preference, and no `[pr]` key can reach it. A value starting with `-` is refused before dispatch, since both land next to those flags in the argv.

**`model` is deliberately not checked against a list of known models.** `effort` is an enum of five levels, so a typo there is caught before dispatch; `model` is not, because the value space is effectively open — the aliases (`opus`, `sonnet`, `fable`), their undocumented context-window variants (`opus[1m]`), and arbitrary full ids (`claude-opus-5`) all have to keep working. An allowlist would buy little and would break legitimate pins the day a new model ships.

A mistyped `model` therefore reaches the agent, which rejects it a few seconds after dispatch and exits — and tmux destroys the review window along with the error. `forgectl pr list` is where that surfaces: it cross-checks each breadcrumb against the live tmux window list and renders a status column. The output is tab-separated — aligned below for readability.

```
cameronsjo/forgectl#42   2026-08-04T09:12:31Z  …/forgectl/pr-sessions/cameronsjo-forgectl-42-….json   live
cameronsjo/forgectl#41   2026-08-04T09:10:02Z  …/forgectl/pr-sessions/cameronsjo-forgectl-41-….json   window gone
cameronsjo/forgectl#39   2026-08-03T16:41:55Z  …/forgectl/pr-sessions/cameronsjo-forgectl-39-….json   workspace missing
```

The status is the last field, appended rather than inserted, so the breadcrumb stays field 3 for anything already parsing this output.

A breadcrumb's filename is the one field here that is read off disk rather than parsed, so a name carrying terminal control or bidi characters is escaped as a Go-quoted literal instead of being printed raw. The escaping is conditional: an ordinary path prints verbatim, so field 3 remains exactly the argument `pr teardown` takes. `pr dash` quotes the path unconditionally — it is a human view, not a parsing target.

`live` means the review window still exists. `window gone` means the agent is no longer running — a rejected `model` is one cause, but so is a finished review or a window you closed yourself; either way the session is stale and `pr teardown <breadcrumb>` reclaims it. `?` means tmux itself could not be read, which says nothing about any individual window; the command still succeeds.

`workspace missing` means the clean-room directory itself is gone — usually because you deleted it by hand, or the OS reclaimed its temp root. The breadcrumb outlived what it described. `pr teardown <breadcrumb>` removes the leftover record: on this branch it unlinks the breadcrumb and **nothing else** — no workspace removal, no quarantine restore, no tmux, no git — because there is nothing left to tear down. `pr cleanup <YYYY-MM-DD>` sweeps these up by date alongside live sessions.

These rows are also why a stale-only `pr list` makes no tmux calls at all: a missing workspace's window is not a question worth asking, and only live rows are batched into the liveness read.

Teardown refuses rather than guesses. It re-checks the breadcrumb's identity and exact contents, and re-confirms the workspace is still absent, immediately before unlinking; anything that changed underneath it — the file rewritten, replaced, or swapped for a symlink, the workspace reappearing — is a refusal that leaves the record in place. A record whose workspace exists but is not a forgectl sandbox is neither live nor cleanly missing, so it is left alone entirely: `pr list` skips it and teardown refuses it. That state means something unexpected wrote to the session-state dir, and deleting it on a guess would destroy the evidence.

**`pr list` is the after-the-fact view; the launch commands check the same thing at dispatch time.** Once every window is open, forgectl waits **eight seconds**, lists windows exactly once, and reports any review that has already vanished. Eight seconds is the observed window in which a rejected `model` gets rejected — long enough to catch it, short enough not to stall the command. It is a bounded observation, not a guarantee: an agent that dies at nine seconds still dispatches "successfully", and `pr list` remains the way to find it later.

Matching a dispatch back to a window has to survive two things that ordinary lookups do not — a second, older window carrying the same name, and a tmux server restart that reissues native window ids from `@0`. So forgectl captures the server PID, the server start time, and the native window id together, in the same `new-window` call that creates the window. Those three format fields are the reason for the floor: tmux documents all of them from **2.2** onward. `tmux -V` is checked before any review workspace exists, so an old, missing, or unreadable binary refuses up front:

```text
tmux 2.2 or newer is required to launch PR reviews with exact dispatch identity (found "tmux 2.1"); upgrade tmux and retry: unsupported tmux version
```

The stable sentence and the quoted version are the contract; the text after the final colon is the wrapped cause and varies with what failed. The quoted value is `"unknown"` only when `tmux -V` itself could not be run or parsed — a missing binary, or output in a shape forgectl refuses to guess at. A supported binary whose server state is unreadable still reports its real version there.

This capability check runs before any clean room or breadcrumb exists, so a refusal there leaves neither behind, and it creates no tmux server, session, or window — both probes (`tmux -V` and one `display-message -p`) only read. Nothing an earlier invocation left running is touched: a review dispatched from another terminal or another repository keeps its window, and `forgectl pr list` is how to see what is still live. What this does **not** cover is a failure after preparation — an invalid reviewer profile, a refused provenance, or tmux going away between this check and the launch: those deliberately leave the workspace and breadcrumb in place, because `pr attach` and `pr teardown` still need them. The one exception is tmux's own doing: a read-only probe against a machine with no server running can create the standard `tmux-<uid>` socket parent directory. That is a tmux artifact, not a review artifact.

When verification reports a review gone, recovery is the ordinary stale-session path — `forgectl pr list` to see which breadcrumbs lost their window, then `forgectl pr teardown <breadcrumb>` on each. When tmux could not be read at all, the command says exactly that instead of naming any review gone; an unreadable server is not evidence of a dead window.

`--no-verify` skips the eight-second wait and the window check, and nothing else. It is not an escape from the tmux floor, the capability check, the concurrency cap, or identity capture — it only trades post-dispatch confirmation for an immediate return.
