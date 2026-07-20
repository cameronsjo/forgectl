# Audience Review: forgectl README + CLI surface (Artifact/UX mode)

Panel: six cast personas, degraded fallback path (cast generated this session —
read-only instructed not enforced, no persona memory accrued). Dossier:
`docs/discovery/2026-07-17-audience-dossier.md`.

**Filed 2026-07-18:** the actionable findings became forgectl#101 (README
surface), #102 (fx alias), #103 (TUI trap), #104 (exit codes), #105
(env check --json), #106 (cold-reader gaps), #107 (gateway-auth scope record).

## Chasm verdict

forgectl dies at the Pragmatist, and it dies on one bar four seats measured
independently: **the README documents roughly half the shipped surface** (9-11
of ~19-20 command groups; `branch`, `clean`, `docker`, `docs`, `net`, `pip`,
`quarantine`, `review`, `y` are invisible to a cold reader, two of them
carrying destructive `--apply` flags). The enthusiast side of the curve adopts
— the Innovator traced every load-bearing claim to source and found them all
true; the Visionary judged the platform spine real and the README
*underselling* its own trajectory — but the mainstream this project actually
has (future-Cameron cold, a second homelab operator, the agents) refuses at
the front door. The drift is live, not historical: the undocumented modules
landed July 8-17 and the README was edited July 17 without sweeping them in.
The deliberate bowling-alley walls (license, personal tap, estate coupling)
are working as intended and are not the blocker.

**Dies at:** Pragmatist — the documented-surface bar. One fix (README covers
every shipped command group, or marks the rest internal/unstable) moves four
verdicts.

## Agent-operability verdict

**Not yet.** The declared second user class hits a trap on its first probe:
bare `forgectl` — the README's *lead* usage example — and any mistyped verb
route to a Bubble Tea alt-screen TUI with no TTY guard, while `--help` (which
works, styled) is never documented. Headless behavior conflicts across seats
— the Agent inferred error+exit 1 from source; the Conservative empirically
observed *silence with exit 0*, which is worse (reads as "did nothing") and
wins on evidence. Exit-code discipline is absent at the boundary (every
failure exits 1; README's `env check` "exit 1 iff missing" is false — missing
keys, missing file, and missing example are indistinguishable by code; the
bless helper's typed codes get flattened). The env module's *threat model*
verified sound when operated blind — the contract is right; the ergonomics
around it assume a human.

## Findings by curve position

### [Innovator] — adopt · bar: claims trace to source

- Blessing ceremony is real cryptographic engineering (SE P-256, per-signature
  `userPresence`, domain-separated pre-images, split Swift-signer/Go-verifier);
  `AllowAllVerifier` deleted outright as claimed.
- Workflow DSL is a typed composition layer with a field-level security
  boundary (`GuardedFields` hard-error), not TOML-wrapped shell.
- Deny-by-default `pr` review is literal (`allowlist.go`), including the
  `rg --pre` exclusion — real threat-modeling detail.
- Open ceiling, disclosed honestly: ADR-0006's residual — the ceremony assumes
  a non-agent-writable Homebrew prefix, which `/opt/homebrew` is not.

### [Visionary] — adopt · bar: the README lets a reader judge the trajectory accurately (it doesn't, in both directions)

- Platform spine (DSL, blessing, launch absorption, env) is infrastructure,
  not paint.
- Fleet epic (#81) is an explicitly empty placeholder — the multi-agent-estate
  mechanism doesn't exist yet.
- The claunch absorption's self-declared "load-bearing" gateway-auth scope was
  silently dropped when #2 closed — no code, no follow-up issue naming it.
- `review` and `quarantine` — the two most on-thesis modules for an
  agent-driven estate — are the two most absent from the README.

—— the chasm ——

### [Pragmatist] — not yet · bar: README documents the whole shipped surface

- ~9 undocumented command groups; the documented surface feels complete, which
  makes the omission disqualifying — the cold reader trusts the map.
- No CHANGELOG (#34) and no quickstart across six releases.
- #57 confirmed live on the installed binary: `config` and `launch which`
  disagree about which config governs.
- Second-operator surfaces are half-configurable: `projects` GitHub owner is a
  compile-time const, the Gitea host has no config key — and nothing documents
  which groups are estate-bound.

### [Conservative] — not yet · bar: the first thing the README says to try must exist

- **`fx` does not exist after the documented install** — the published cask
  stages only `forgectl`; the alias lives in the author's dotfiles.
- Load-bearing nouns never defined: mart, hearth, chronicle, flux,
  `<breadcrumb>` (a filesystem path discoverable only by reading Go source).
- Companion dependencies (`sesh`, `gh`, `tea`, `docker`) undeclared in
  Install; `sesh` lacks the `LookPath` guard `claude` gets.
- Bare `forgectl` with no TTY: silence, exit 0 (observed) — on the tool's own
  flaky-SSH use case.

### [Skeptic] — not yet · bar: "read the README" and "know what forgectl does" are different activities

- Independently measured the 42% doc gap and proved the drift is ongoing.
- The pitch sentence doesn't match the scope: undisclosed destructive verbs,
  a docker wrapper, a pip.conf editor.
- A literal internal IP ships as the example `sessions.dsn`
  (`config.go:51`).
- Risk-disclosure discipline is real but tracks the most recently
  security-passed subsystem (env), not the surface (`clean --apply`,
  `branch --apply` carry no equivalent note).
- No written exit story for a 40k-LOC, 56-dependency, bus-factor-1 tool.

### [Agent] — off-curve — not yet · bar: the first natural probe must not trap

- The TUI trap (headline, above); cobra's did-you-mean suggestions dead for
  top-level typos because the TUI intercepts unknown verbs first.
- Universal exit 1; `env check` README claim false; helper's typed codes
  flattened at the boundary.
- No `env check --json` on the agent-primary surface (its two-section indented
  format is bespoke); `--json` exists on six other commands.
- Positive: env's never-argv / no-print-path contract holds exactly as
  documented when operated blind.

## Seat disagreements (adjudicated)

- **#57 severity** — Pragmatist flags, Skeptic dismisses as cosmetic. Kept as
  a real-but-minor finding: it's an early-run diagnostic pair disagreeing,
  which costs trust disproportionately to its size.
- **Headless bare-invoke behavior** — inferred error/exit-1 (Agent) vs
  observed silence/exit-0 (Conservative). Empirical wins; the silent exit 0 is
  the worse behavior and the finding stands either way (no TTY guard).

## Merged Dismissed (9)

- `Version = "dev"` beta smell — `Pragmatist`/`Visionary` verified
  goreleaser-ldflags resolves it; install path never shows "dev".
- No OSI license — [Pragmatist] deliberate stop, located not condemned.
- Bus factor 1 / no testimonials / SLA — [Pragmatist] wrong lens for a
  personal tool; viability = the maintainer's own daily dependence.
- Blessing "cryptographic engineering" framing as marketing — [Innovator]
  every claim under it checked out; technical truth in plain language.
- `--dry-run` unsigned carve-out — [Innovator] strict-decode-only, a named
  ADR trade-off.
- Zero-config claim — [Conservative] verified live: sensible defaults, no
  crash.
- `bench status` degradation — `Conservative`/`Agent` verified graceful,
  documented, machine-detectable, exit 0.
- Open-issue count as instability — [Skeptic] active backlog on a
  personal tool is hygiene, not decay; 1 of 48 is `bug`-labeled.
- `--any-file` TTY gate / Touch ID human gates — [Agent] announced,
  documented, machine-detectable refusals; intended bounds.

## Decisions only the owner can make

- Are the nine undocumented command groups *public surface* (document them) or
  *internal/unstable* (say so in the README)? The fix differs and the panel
  can't choose the intent.
- `fx`: ship it in the cask, or rewrite the README line to a suggested shell
  alias?
- The dropped gateway-auth scope from the claunch absorption: file it as a
  deferred issue, or strike the claim from the record?
- Exit-code discipline: typed codes at the forgectl boundary (surfacing the
  helper's existing 2/3/4/5), or document that agents must parse stderr?
- Fleet (#81): is the placeholder the intended state, or does a read-only
  first slice deserve scheduling?

Artifact: docs/discovery/2026-07-17-audience-review-readme-cli.md
