## 1. Domain

- [x] 1.1 `internal/workerwatch`: workers-file parser, hc client (get, ls, read, events), report scan, GitHub read with viewer cache, collector with TTL cache
- [x] 1.2 Unit tests with fakes for hc, git and gh; no live hc or claude

## 2. View

- [x] 2.1 `internal/tui/workers.go`: rows, needs-you banner, screen tail of the selected row, event and poll scheduling
- [x] 2.2 Model/update tests, a golden render, and the hostile-screen escape test

## 3. Verb and docs

- [x] 3.1 `forgectl surface watch` with `--workers`, `--cos`, `--json`, `--interval`, `--gh-ttl`; plain snapshot off a terminal
- [x] 3.2 `docs/herdr.md` (Watching chief-of-staff workers) and README
- [x] 3.3 build, vet, test, lint, `openspec validate --strict`
