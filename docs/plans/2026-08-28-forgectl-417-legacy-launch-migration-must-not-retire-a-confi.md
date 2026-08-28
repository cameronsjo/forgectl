---
body_sha256: "28bb33362f50e9869c8c2a462227903c0e905b65baaa5069bd35dc271235239b"
session: "woven-loom"
session_id: "251e811e-1ff7-4e32-9c72-f78c99c348b3"
machine: "cf6e768835c7"
approved_in: "pale-quill"
approved_session_id: "e0ddd8eb-2343-4965-af79-3a3a0999bb22"
status: in-review
next: "merge #418"
branch: fix/417-legacy-migration-lossy-supersession
pr: "https://github.com/cameronsjo/forgectl/pull/418"
updated: 2026-08-28
date: 2026-08-28
---

# forgectl #417 — legacy launch migration must not retire a config it only partly understood

## Goal

The auto-migration that runs on every launch surface refuses to render, back up, or unlink the legacy config when that file carries fields forgectl cannot represent — and never asserts "fully superseded" over a file it only partly decoded. A `claunch` config in a format forgectl does not model is named, not reported absent.

Closes cameronsjo/forgectl#417.

## Alternatives declined

- **Discover and import `~/.config/claunch/config.toml` as a second legacy source** (what #417 requests) — forgectl would model the schema of a tool it never absorbed and owe that tool's changes forever. Detect-and-refuse gives the operator the same honest answer without the coupling.
- **Warn instead of refuse, on the migration path** — a warning printed above a rendered `[launch]` section and an unlinked source is exactly the silent-lossy-success #417 reports. Warn-only is kept for *reading* the legacy config at launch, where aborting would be a false block on legitimate work.
- **Teach forgectl the current claunch's unmodeled fields** — that schema belongs to a separate, work-internal tool lineage; encoding it into a public binary crosses the ADR-0037 sensitivity boundary. #107 already holds the generic gateway-auth ask, and #417 is explicitly narrower.

## Panel

Panel: red-team seat ran — 7 findings, 6 folded in, 1 declined (see Panel review — findings declined)

## Panel review — findings declined

- **[LOW] "Out of scope item 3's live premise is stale"** — declined: the seat checked `~/.config/claunch/` and found only `claunch.conf.bak`, but item 3 is about two dead `match` paths inside forgectl's own `config.toml`, which this session verified directly by stat-ing `~/Projects/minute` and `~/Projects/infrastructure`. Different file, different claim; the item stands.

## Architecture

The plan's first draft aimed at the wrong function. The real call graph:

- **`migrateLegacyExplicit`** (`internal/cli/launch_migration.go:431-492`) is what `forgectl launch migrate` runs. It renders `[launch]` and returns. It takes **no** backup and **never** unlinks.
- **`migrateLocked`** (`:267-399`) is what actually destroys: `sourceUnlink` at `:374`, the `added == 0` → `"legacy config fully superseded, removed."` notice at `:391`. It is reached only through `migrateLegacyAutomatically` (`:401`).
- **`autoMigrateOrWarnLegacyLaunch`** (`internal/cli/launch.go:289`) calls that unconditionally — from `launch.go:144`, `launch_which.go:33`, and `launch_doctor.go:35`. So an ordinary `forgectl launch`, `launch which`, or `launch doctor` retires the file. Only `FORGECTL_SKIP_LEGACY_MIGRATE=1` opts out.

So a refusal placed only in `migrate` changes nothing about the reported bug: the operator's next `forgectl launch` finishes the lossy retirement for them.

Likewise the decode. `config.LoadLegacyLaunch` has exactly one non-test caller — `resolveLaunchConfig`'s `boundary == nil` fallback (`internal/cli/launch.go:240`). The decodes that feed the destructive path are `toml.Decode` in `unixLegacyProbe.Capture` (`internal/config/legacy_boundary_unix.go:195`, producing `boundary.Source.Launch`) and `loadReadOnly` (`:534`), with `internal/config/legacy_boundary_other.go:68` for non-unix. Undecoded keys must be captured there and carried on `LegacySnapshot`, which already retains the raw `Source.Data`.

The gate belongs in `migrateLocked` **before** the `shadow` branch at `:282` — ahead of every render, the backup ladder, and the unlink — and routes to `autoMigrateOrWarnLegacyLaunch`'s existing *warn* arm. Refusing must never abort a launch; it means "read leniently, migrate nothing, retire nothing."

## Tech Stack

- Go 1.26.0, module `github.com/cameronsjo/forgectl`
- TOML via `github.com/BurntSushi/toml` — `MetaData.Undecoded()` is the affordance being adopted
- Cobra CLI; `slog`; `termsafe` for untrusted-string rendering
- `go test ./...`; the existing `newLegacyHarness` fixture in `internal/cli/launch_init_test.go` is the integration seam

## Global Constraints

- **MUST NOT** name, quote, or paraphrase the work-internal claunch's fields, files, commands, or flags in any code, comment, test fixture, commit message, PR body, or issue comment. Fixtures use invented names (`unknown_field`). ADR-0037 boundary; `cadence:redaction` runs on every externally-posted body.
- **MUST NOT** alter the backup/unlink durability ladder at `internal/cli/launch_migration.go:328-385` — `backupDurableVerified`, the doubled `sourceRevalidate`/`backupRevalidate` loop, `sourceSyncParent`. A separate contract, out of scope. The notice switch at `:386-397` **is** in scope.
- **MUST NOT** turn the refusal into a launch failure. `autoMigrateOrWarnLegacyLaunch` already has a warn arm; the gate routes there. A refusal that blocks `forgectl launch` is a worse bug than the one being fixed.
- **MUST** render undecoded key names through `termsafe` — attacker-influenced input read from a config file, same treatment as paths at existing call sites.
- **MUST** add an `[Unreleased]` entry to the repo-root `CHANGELOG.md` in the same PR (git-workflow § Changelog Before PR).
- No new runtime dependencies. Go 1.26.0 floor. `golangci-lint` clean per the repo's `.golangci.yml`.

## Orchestrator

**Driver:** Opus — an irreversible source-retirement path on a call graph that already misled one planning pass, plus a sensitivity boundary a cheaper tier would not hold.

---

## Tasks

### Task 1 — carry undecoded keys on the legacy snapshot

**Files:**
- Modify: `internal/config/legacy_boundary_unix.go` (`Capture` ~195, `loadReadOnly` ~534)
- Modify: `internal/config/legacy_boundary_other.go` (~68)
- Modify: `internal/config/legacy_boundary.go` (`LegacySnapshot` field)
- Test: `internal/config/legacy_boundary_unix_test.go`

**Interfaces:**
- Consumes: *(none — first task)*
- Produces: `LegacySnapshot.UndecodedKeys []string` (sorted dotted keys) and `config.ErrLegacyUnsupportedFields` wrapping that list

**Dispatch:** Serial (wave 1) · in-context — Tasks 2 and 3 both consume the field

**Report:** `—`

**Steps:**
- [x] Write failing test: a captured snapshot over a legacy file with unknown top-level and nested keys exposes them on `UndecodedKeys` — expect RED
- [x] Capture `MetaData` from `toml.Decode` in `Capture` and populate `LegacySnapshot.UndecodedKeys`; mirror in `loadReadOnly` and the non-unix probe so all three decode sites agree
- [x] Add `ErrLegacyUnsupportedFields`, formatted after the precedent at `internal/workflow/parse.go:114`
- [x] Leave `config.LoadLegacyLaunch` alone — its one caller is the lenient `boundary == nil` fallback, off the destructive path
- [x] Run — expect GREEN
- [x] Commit: `fix(config): carry undecoded legacy keys on the migration snapshot`

---

### Task 2 — gate the destructive path, and fix both misleading messages

**Files:**
- Modify: `internal/cli/launch_migration.go` (`migrateLocked` ~267, gate before `:282`; notice switch `:386-397`; `migrateLegacyExplicit` ~431)
- Modify: `internal/cli/launch.go` (`autoMigrateOrWarnLegacyLaunch` ~289 — route the refusal to the warn arm)
- Modify: `internal/cli/launch_init.go` (`runLaunchMigrate` ~168-178)
- Test: `internal/cli/launch_migration_test.go`, `internal/cli/launch_init_test.go`

**Interfaces:**
- Consumes: `LegacySnapshot.UndecodedKeys`, `ErrLegacyUnsupportedFields` (Task 1)
- Produces: *(terminal — CLI behavior)*

**Dispatch:** Serial (after Task 1) · in-context — placement must be read off the surrounding transaction, not a spec

**Steps:**
- [x] Write failing test: undecoded keys present ⇒ `forgectl launch` reads the legacy config, warns, and leaves the source **on disk** with no `config.toml` written — expect RED
- [x] Write failing test: same fixture ⇒ `launch migrate` exits non-zero naming `unknown_field`, writing nothing — expect RED
- [x] Add the gate in `migrateLocked` before the `shadow` branch at `:282`, returning a refusal result ahead of every render, the backup ladder, and `sourceUnlink`
- [x] Route that refusal to `autoMigrateOrWarnLegacyLaunch`'s warn arm so `launch`/`which`/`doctor` degrade instead of failing
- [x] Refuse in `migrateLegacyExplicit` too, and surface `ErrLegacyUnsupportedFields` in `runLaunchMigrate` beside the existing `ErrLegacyMalformed` arm
- [x] Fix `migrateLocked`'s `added == 0` notice at `:391` to name `result.BackupPath`, matching its two sibling branches — a message asserting removal must carry the recovery pointer
- [x] Fix `launch_init.go:176` separately: it discards `result.Notice` and prints its own `Imported %d launch profile(s)`, so a zero-profile import needs its own line there. Different file from the notice switch — do not conflate them
- [x] Run — expect GREEN
- [x] Commit: `fix(launch): refuse to retire a legacy config with unrepresentable fields`

---

### Task 3 — name a claunch config forgectl cannot migrate

**Files:**
- Modify: `internal/config/legacy_boundary.go` (sibling probe beside the candidate-path derivation ~370-390)
- Modify: `internal/cli/launch_init.go`, `internal/cli/launch_doctor.go`
- Test: `internal/cli/launch_doctor_test.go`

**Interfaces:**
- Consumes: `boundary.LegacyPath` (existing)
- Produces: a probe reporting whether a sibling `config.toml` exists beside the legacy path

**Dispatch:** Serial (after Task 2 — shares `launch_init.go`) · in-context

**Steps:**
- [x] Write failing test: sibling `config.toml` present, `claunch.conf` absent ⇒ `migrate` and `doctor` name the found file and the format mismatch instead of `no legacy claunch.conf found` — expect RED
- [x] Derive the probe as `filepath.Join(filepath.Dir(boundary.LegacyPath), "config.toml")` — **never** from `configDir()`, which resolves to `~/Library/Application Support` on darwin while the legacy base is `~/.config`, and would report absent on the exact file #417 is about
- [x] Probe existence only; never decode it, never treat it as an import source
- [x] Word it as a mismatch the operator resolves by hand — forgectl migrates the historical format only
- [x] Run — expect GREEN
- [x] Commit: `fix(launch): name a claunch config forgectl cannot migrate`

---

### Task 4 — regression floor, changelog, ship

**Files:**
- Modify: `CHANGELOG.md`
- Test: `internal/cli/launch_init_test.go` (confirm, don't duplicate)

**Interfaces:**
- Consumes: Tasks 1-3
- Produces: *(terminal)*

**Dispatch:** Serial (after Tasks 1-3) · in-context

**Steps:**
- [x] Confirm `TestIntegration_LaunchInit_FromClaunch_ImportedProfileDrivesLaunch` (`internal/cli/launch_init_test.go:142`) stays green — a well-formed legacy file must still import and still retire exactly as before. Do not write a second copy
- [x] Confirm the dead `config.ValidateLegacyLaunch` (`internal/config/config.go:1126`, no callers in the main tree) is either wired to the new verdict or left untouched — decide explicitly, do not half-fix it
- [x] `go build ./... && go test ./...`
- [x] Add the `[Unreleased]` entry to the repo-root `CHANGELOG.md`
- [x] Open the PR with `Closes #417` as plain text, plus the producer-tuple trailer block (forgectl squash-merges, so the tuple rides the PR body too)
- [x] run `cadence-forge:polish`; fold findings

---

## Deviations

- **Task 1** — the plan says "mirror in `loadReadOnly`", but `loadReadOnly` returns a bare `LaunchConfig` and has no snapshot to carry keys on. Extracted a shared `decodeLegacyLaunch(data) (LaunchConfig, []string, error)` in `internal/config/legacy_boundary.go` instead: `Capture` keeps the keys, both `loadReadOnly` implementations discard them. All three decode sites now agree on what parses, and the read-only path stays lenient as the plan requires.
- **Task 2** — the plan calls for routing the refusal to `autoMigrateOrWarnLegacyLaunch`'s warn arm; that arm already fires on any non-nil `result.Err`, so the routing was a `switch` giving the refusal its own log line ("Declining to migrate…") instead of the misleading "did not fully retire the source". Behavior is as specified; no new arm was needed.
- **Task 2 (post-review)** — `launch doctor` printed the refusal notice only from its `default` arm, so a legacy file forgectl models *nothing* of left `lc` zero, took the "no launch profiles configured" arm, and reported exactly that while a refused `claunch.conf` sat beside it. The notice now prints ahead of the switch; `TestIntegration_LaunchDoctor_WhollyUnrepresentableLegacy_Warns` pins the input class the first doctor test missed (it carried a modelled `model` key, so it never took that arm).
- **Task 3 (post-review)** — the unmigratable-sibling path moved out of the returned error and onto stdout. `fang`, the styled error renderer, title-cases every word, so an absolute path came out as `/Var/Folders/Wl/…` — unusable for copy-paste. The error stays terse and keeps the exit non-zero.
- **Task 2 (post-panel)** — the refusal predicate was extracted to `unsupportedFieldsCause` and the gate hoisted into `migrateLegacyAutomatically` ahead of `ensureParent`, after the code-review and security arms both flagged that a refusal was still creating the config directory. See Learnings.
- **Task 2** — one existing assertion moved: `TestIntegration_LaunchShadow_DuplicateProjectMatch_NoOverwrite` (`internal/cli/launch_test.go:837`) pinned the old `legacy config fully superseded, removed.` wording. Updated to the new notice and extended to require the backup name, which is the fix the plan asked for.

## Learnings

- **The gate had to sit one frame higher than the plan specified.** The plan placed the refusal in `migrateLocked`, ahead of every render, the backup ladder, and the unlink. That is correct as far as it goes, but `migrateLegacyAutomatically` calls `ensureParent` — which mkdirs the config parent — and `withLock` before it ever reaches `migrateLocked`. So a refusal still created a config directory the operator never asked for, on the exact first-ever-run path #417 is about. The repo's own README already stated the invariant this broke ("No pre-capture refusal creates the config directory, writer lock, config temp, config file, or backup"), which is what makes it a real finding rather than a preference. The gate now runs in both places; `TestMigrationTransaction_RefusalCreatesNoConfigDirectory` pins it and was verified to go red without the hoist.
- **`config.ValidateLegacyLaunch` was deleted, not wired** (Task 4's owed ruling). It had no callers, its docstring claimed `launch doctor` used it when doctor calls `LoadReadOnlyLegacy` instead, and wiring it would have added a fourth `toml.Decode` site that could disagree with the shared helper — the exact divergence Task 1 removed.

---

## Verification

Primary — Go tests, using the existing `newLegacyHarness` seam. This is the real gate; the shell pass below is a smoke check only.

```sh
cd ~/Projects/cadence-ecosystem/forgectl
go build ./... && go test ./...
```

Smoke, against a scratch `HOME` with `XDG_CONFIG_HOME` **unset**. Setting `XDG_CONFIG_HOME` does not work here: `candidateLegacyMigrationPaths` (`internal/config/legacy_boundary.go:370-390`) derives the config path from `os.UserConfigDir()` and the legacy path from `XDG_CONFIG_HOME`, so `validateLegacyMigrationPaths` (`:392-411`) refuses with `ErrLegacyPathPolicy` before any behavior under test runs — every assertion would pass vacuously.

```sh
tmp=$(mktemp -d); mkdir -p "$tmp/.config/claunch"
run() { env -u XDG_CONFIG_HOME HOME="$tmp" ./forgectl "$@"; }

# 1. unrepresentable fields -> warn on launch, refuse on migrate, retire nothing
printf '[defaults]\nmodel = "opus"\nunknown_field = "x"\n' > "$tmp/.config/claunch/claunch.conf"
run launch which 2>&1 | grep -q 'unknown_field' && echo "PASS: named on which" || echo "FAIL"
test -f "$tmp/.config/claunch/claunch.conf" && echo "PASS: source retained" || echo "FAIL: source retired"
run launch migrate 2>&1 | grep -q 'unknown_field' && echo "PASS: refusal names the field" || echo "FAIL"

# 2. a config.toml forgectl cannot migrate -> named, not reported absent
rm "$tmp/.config/claunch/claunch.conf"; printf '[defaults]\nmodel = "opus"\n' > "$tmp/.config/claunch/config.toml"
run launch migrate 2>&1 | grep -q 'config.toml' && echo "PASS: named" || echo "FAIL"

# 3. a well-formed legacy file still imports and still retires
rm "$tmp/.config/claunch/config.toml"; printf '[defaults]\nmodel = "opus"\n' > "$tmp/.config/claunch/claunch.conf"
run launch migrate; echo "exit=$?"
```

Assert on the **message content**, not merely on exit status — a non-zero exit is also what a path-policy refusal produces, so exit alone is a green that could not have gone red.

## Out of scope — housekeeping found while grounding

Not part of this plan's contract; listed so it isn't lost.

1. Installed `forgectl` on `sjomba` is `0.12.0`; `v0.13.0` is tagged and #417 was measured against `0.14.0`. `brew upgrade forgectl`.
2. 14 `prunable` worktrees registered under `/private/tmp/forgectl-issue-*` on `codex/issue-*` branches. `git worktree prune`.
3. The live launch config has two dead `match` paths — `~/Projects/minute` and `~/Projects/infrastructure` — stale since the 2026-08-04 wing move; `cadence-ecosystem` has no profile and takes bare defaults.
