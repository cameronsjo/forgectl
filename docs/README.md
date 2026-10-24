# docs

Index of forgectl's doc tree. The top-level `README.md` is the user-facing
entry point (install, the command roster, the usage summary); this directory
holds the reference pages it links to, plus the supporting record — why
decisions were made, what was explored, and how the running system is
operated.

- **[`commands/`](commands/)** — per-command deep dives (env, launch, k8s,
  docs, [recipe](commands/recipe.md), [audit](commands/audit.md), …) split out of the README; the README keeps the full roster and
  usage summary.
- **[`json-contract.md`](json-contract.md)** — what a `--json` verb writes to
  stderr when it exits non-zero, and the failure object's `code` strings.
- **[`status-vocabulary.md`](status-vocabulary.md)** — the shared `work`
  object (`status`, `on`, `reason`, `evidence`) that queue, drain and desk
  `--json` rows carry beside their native state, and how each state maps.
- **[`configuration.md`](configuration.md)** — every `config.toml` section,
  key by key, plus logging.
- **[`prompts.md`](prompts.md)** — how a cancelled picker or confirm ends: the line it prints and exit code 130.
- **[`herdr.md`](herdr.md)** — the `internal/herdr` client: session and fork requirements, failure shapes, why ids move, worker readiness for `surface ready`, how `surface brief`, `wait` and `read --report` work, and how `surface list` and `close` decide.
- **[`artificer-adaptations.md`](artificer-adaptations.md)** — where this repo
  bends the vendored Artificer design system, and why; each entry names the
  upstream issue and the condition that retires it.
- **[`adr/`](adr/README.md)** — Architecture Decision Records, with an index.
  Durable design decisions (the workflow DSL's shape, the module registry,
  checkpoint/resume semantics) with the reasoning behind them. Read these
  when a "why does it work this way" question comes up; they don't get
  rewritten after the fact.
- **[`discovery/`](discovery/)** — dated research and audits (e.g. an
  audience/README review). A point-in-time snapshot, not a living document.
- **[`operations/`](operations/)** — live runbooks for operating forgectl
  itself day to day. Unlike `adr/` and `discovery/`, these are meant to be
  kept current.
- **[`../openspec/`](../openspec/)** — the specs and the change proposals.
  `openspec/specs/` says what each command group does today; new work is
  proposed under `openspec/changes/` and archived when it ships. See
  AGENTS.md for the workflow.
- **[`plans/`](plans/)** — planning documents for features and refactors as
  they were built, up to the OpenSpec move on 2026-10-20; no new plans land
  here. Read a *feature* plan as history, not as a spec still in force — the
  shipped behavior, its spec and its ADR are the current truth. The
  exception is a file named `*-roadmap.md`: those are forward-looking and
  current until a later one supersedes them, which the superseded file says
  at its top.
- **[`RELEASING.md`](RELEASING.md)** — the release process for cutting a new
  forgectl version.
