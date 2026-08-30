# Forgectl #413 — Remove Ambient Tokens from Pinned `gh` Processes

## Goal

Make the non-default GitHub-host credential boundary independent of GitHub
CLI behavior: `GH_TOKEN`, `GITHUB_TOKEN`, `GH_ENTERPRISE_TOKEN`, and
`GITHUB_ENTERPRISE_TOKEN` must be absent from the spawned `gh` process
environment, not present with empty values.

## Current evidence

- `githubauth.pinnedRunner` currently passes all four token names through
  `exec.Runner.RunWithEnv` with empty values on a non-default host.
- `RunWithEnv` only appends overrides to `os.Environ`; it cannot remove an
  inherited variable.
- GitHub CLI 2.98.0 treats empty token variables as unset, but that behavior is
  an implementation detail rather than a property enforced by forgectl.
- A `gh` subprocess can launch descendants such as Git and credential helpers,
  which inherit the empty variables too.
- `exec.Runner` is deliberately closed: production, fakes, and the few custom
  test adapters explicitly implement every execution mode.

## Chosen approach

Add an explicit `RunWithEnvFiltered` runner method that accepts environment
overrides plus exact variable names to remove from the inherited environment.
Implement it on `OSRunner`, record removals on `FakeRunner`, and make every
custom runner adapter delegate it explicitly. Route pinned `gh` calls through
that path: on non-default hosts, delete caller-supplied token overrides and
remove all four ambient token names before spawning; then apply the validated
`GH_HOST` override last.

Prove the production boundary with a real child process that distinguishes an
unset variable from an empty one. Keep fake-runner assertions for the wrapper's
routing and precedence rules, including plain `Run`, caller-supplied env,
default-host preservation, double wrapping, and invalid-host refusal.

## Alternatives declined

- **Keep empty-string scrubbing:** smaller, but leaves the security property
  dependent on current `gh` interpretation and does not remove the variables
  from descendants.
- **Special sentinel values in `RunWithEnv`:** avoids a new method but makes an
  ordinary string map carry hidden deletion semantics and risks colliding with
  legitimate empty environment values.
- **Optional filtered-runner interface:** narrows the core interface but turns
  a required security capability into a runtime type assertion; expanding the
  deliberately closed runner interface gives a compile-time completeness gate.
- **Bundle all #413 residuals:** mixes subprocess security plumbing with host
  routing, JSON schema, and persisted identity changes that have separate
  compatibility decisions.

## Boundaries

- This PR addresses only #413's empty-string token-scrub residual.
- Do not pin the remaining `branch`, `pr`, or `doctor` `gh` callers yet.
- Do not change JSON output, `Repo.Host`, clone layout, Gitea host derivation,
  or user configuration.
- Preserve current default-host behavior: operator token variables remain
  available to `gh` on `github.com`.
- Preserve existing `RunWithEnv` behavior for Homebrew and other callers.
- Do not edit generated `CHANGELOG.md`; carry any consumer-facing release prose
  through a Release Please commit override in the pull-request body.

## Checklist

- [x] Revalidate #413, #412/#414, current runner contracts, and `origin/main`.
- [x] Record Cameron's approval of the filtered-environment slice.
- [x] Persist this plan in its own commit before implementation.
- [x] Add and production-test the filtered environment execution primitive.
- [x] Make every runner implementation explicitly support the new method.
- [x] Route non-default-host `gh` calls through exact token removal.
- [x] Mutation/control-test absence, default-host preservation, and pin
      precedence across plain and explicit-environment calls.
- [ ] Run focused tests, the full relevant suite, vet, lint, formatting, and
      repository-wide tests with environmental boundaries recorded precisely.
- [ ] Polish the full diff, fix or disposition every finding, and record the
      release-note decision.
- [ ] Push a PR that references #413 without closing the umbrella issue, then
      monitor CI and review to terminal state before merge.

## Execution evidence

- The real-process test uses POSIX `${name+x}` expansion to distinguish an
  absent variable from an empty one. `RunWithEnvFiltered` reports the inherited
  test credential absent while preserving an unrelated override; its embedded
  `RunWithEnv` control reports an empty override present, proving the assertion
  detects the old behavior rather than accepting it.
- Wrapper tests prove all four credential names are absent from overrides and
  present exactly once in the removal set for both plain `Run` and
  `RunWithEnv`; default-host tokens remain untouched. The explicit filtered
  path also proves caller removals survive, caller token overrides lose, the
  validated host pin wins, and caller-owned slices are not mutated.
- `go test ./... -run '^$' -count=1` passes with the matching Go 1.26.5
  toolchain, compiling every package and proving all implementations of the
  deliberately closed `exec.Runner` interface support the new method.
