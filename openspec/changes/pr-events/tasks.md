## 1. Store and poller

- [x] 1.1 `internal/events`: refs, events, snapshots, `Diff`, validation on write and read.
- [x] 1.2 Store: privdir-pinned directory, flock, atomic JSON writes, append-only feeds cut at 1 MiB.
- [x] 1.3 GitHub fetcher: one GraphQL query per PR; viewer's markers only, summed by sweep's rule.
- [x] 1.4 `Poll`: reads unlocked, one locked `Record`.
- [x] 1.5 Tests: ref parsing, every diff case, validation, store round trip, empty store, watch cap, query parse.

## 2. CLI

- [x] 2.1 `events watch|unwatch|poll|feed|state`, each with `--json`.
- [x] 2.2 Module registry, hub area, README rows.

## 3. Docs

- [x] 3.1 `docs/commands/events.md`.

## 4. Later

- [ ] 4.1 `forgectl mcp`: `wait_for_checks` and `merge_when_green` as MCP tasks, once the Go SDK has the tasks extension.
- [ ] 4.2 A launchd plist for `events poll`.
