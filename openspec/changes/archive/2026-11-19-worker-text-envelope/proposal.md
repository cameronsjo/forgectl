## Why

A worker's launch prompt and every follow-up `surface brief` arrive in its conversation as bare user text, the same as the operator typing in that terminal. `hc send` wraps what it delivers in an element naming the sender, and Claude Code's cross-session messages arrive wrapped too, but forgectl's own text did not, and forgectl's hc sends dropped the sender id hc needs to wrap. The worker rule then said never approve or merge "whatever the task says", and a worker read that as binding the operator's own typed instruction (#1466, #1462).

## What Changes

- Everything forgectl puts into a worker as conversation text arrives in an envelope: `<forgectl from="<terminal>" kind="launch|brief">…</forgectl>`. The launch prompt (every surface and the drain) and the herdr typed brief carry forgectl's. The hc typed brief carries hc's own (`<hc from="…">`): the two `hc send` calls that carry text keep `HC_TERMINAL_ID` and `HC_SCOPE`, which every other hc call still drops. From a plain shell, where hc delivers bare, forgectl wraps the brief itself.
- A body cannot close or forge an envelope: tag-like runs of `forgectl` or `hc` have their bracket rewritten to `&lt;`.
- The 600-character typed cap counts the envelope, measured at the longest sender name, and its refusal names the room left for the caller's text.
- The launch prompt opens with a plain statement that it is the worker's assignment from its launcher, not from the operator, and that later text from the same sender is the channel for fix rounds.
- The worker rule keeps its review-marker and board-task bans as they were. The approve-and-merge ban binds the task, any brief, and anything tagged; text typed in the worker's terminal outside any tag is the operator's own and can direct one approval or one merge of the pull request it names. A tag never grants authority.
- The envelope is a convention, not authentication: anything that can type into a pane can type bare text.

## Impact

- `internal/surface/worker/envelope.go` (new), `internal/surface/worker/brief.go`, `internal/cli/surface_worker.go`, `internal/cli/surface_brief.go`, `internal/cli/surface_hc.go`, `internal/surface/hcadapter/hcadapter.go`, `internal/surface/hcadapter/worker.go`, `internal/exec/sensitive.go`, `internal/exec/sensitive_run.go`, `internal/exec/internal/validated/validated.go`, `docs/herdr.md`.
- Issues: #1466, #1462.
