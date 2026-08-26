# docs

Index of forgectl's doc tree. The top-level `README.md` is the user-facing
reference (install, commands, configuration); this directory holds the
supporting record — why decisions were made, what was explored, and how the
running system is operated.

- **[`adr/`](adr/)** — Architecture Decision Records. Durable design
  decisions (the workflow DSL's shape, the module registry, checkpoint/resume
  semantics) with the reasoning behind them. Read these when a "why does it
  work this way" question comes up; they don't get rewritten after the fact.
- **[`discovery/`](discovery/)** — dated research and audits (e.g. an
  audience/README review). A point-in-time snapshot, not a living document.
- **[`operations/`](operations/)** — live runbooks for operating forgectl
  itself day to day. Unlike `adr/` and `discovery/`, these are meant to be
  kept current.
- **[`plans/`](plans/)** — historical planning documents for features and
  refactors as they were built. Read as history, not as a spec still in
  force — the shipped behavior and its ADR are the current truth.
- **[`RELEASING.md`](RELEASING.md)** — the release process for cutting a new
  forgectl version.
