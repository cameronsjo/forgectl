# forgectl workflow blessing — user-presence signing for the workflow DSL

**Target repo:** `cameronsjo/forgectl` · **Issue:** #10 (reframed) · **Lands on:** current `main` (PR #83's module architecture **merged 2026-07-12**, `de768a9`)
**Deliverables:** design doc (ADR-0006) + this implementation plan
**Panel:** plan-reviewer ×2, red-team-reviewer, cameron-review ran — 19 findings, 14 folded in, 1 declined (see § Panel review — findings declined); 1 escalated to Cameron (ceremony rung) — **resolved 2026-07-12: full SE + Touch ID, eyes open on cost** (owner-lens hash-allowlist alternative recorded under Alternatives declined).

## Context

forgectl's workflow DSL (#9, shipped) executes TOML step lists — `worktree`, `strip`, `run`, `launch` — i.e. a workflow file is an arbitrary-command-execution surface. Agent-harness guardrails see only the outer `forgectl workflow run <name>`, never the steps inside; and forgectl may eventually run somewhat elevated. Issue #10 planned conventional author-signing (minisign/cosign), but the threat model was reframed:

**The adversary is the local agent itself.** Claude (or any agent) can author a workflow file and run it. The control must therefore be a **human-presence ceremony an agent cannot perform** — the user "blesses" the exact bytes via Touch ID / admin auth. Blessing proves *a human approved this content*, not merely *a trusted key signed it*. Boundary: an agent that holds the user's password or NOPASSWD sudo is **out of scope** (documented in ADR-0006).

The DSL shipped with the seam ready: ADR-0002's pipeline is parse → resolve → **verify** → plan → execute with an injectable `Verifier` (no-op `AllowAllVerifier`, live at `internal/cli/workflow.go:100-106`, receiving the workflow *name*, post-parse — both rewirings this plan performs were pre-declared in ADR-0002's consequences, with one correction below).

## Security design

### Ceremony ladder — one `Blesser` seam, macOS rung first (spike-gated)

| Environment | Ceremony | Status |
|---|---|---|
| macOS GUI | Secure Enclave P-256 key, `userPresence` access control → Touch ID (or account password) per signature; no cache window, key cannot leave the chip | **ships first — spike PASSED 2026-07-12 (sealed-blob variant)** |
| macOS fallback (a) | Keychain-held non-SE P-256 key with the same `userPresence` ACL | **spike result: unreachable for an ad-hoc CLI** — the data-protection keychain itself demands a `keychain-access-groups` entitlement (`-34018`; self-granted entitlements → AMFI SIGKILL). Recorded in ADR-0006; superseded by the sealed-blob approach, which needs no keychain at all |
| Windows | Windows Hello + TPM-backed key (CNG/KeyCredential) | seam only |
| Linux / headless | FIDO2 security key — physical touch as presence proof | seam only |
| Floor | Passphrase-encrypted software key ("a secret the agent doesn't know") | seam only |

**Spike outcome (2026-07-12, M3, ad-hoc-signed CLI):** CryptoKit `SecureEnclave.P256.Signing.PrivateKey` with `SecAccessControl(privateKeyUsage, userPresence)`, persisted as its `dataRepresentation` sealed blob in a plain file (the `age-plugin-se` pattern — no keychain, no entitlement). Enforcement verified: sign with `LAContext.interactionNotAllowed = true` fails `User interaction is required`; interactive sign shows the Touch ID prompt (human-tap latency observed). The ACL is baked into the sealed blob and enforced by the SE/coreauthd — an agent loading the blob with its own code still hits the prompt. The blob is useless off-device and presence-gated on-device, so the blob file itself needs no special protection.

**Blessings travel; the ceremony doesn't have to.** A blessing is a per-content signature — verify anywhere the public key is trusted. Headless environments verify but cannot bless new content, which is the desired property.

**Machine roles:** the M3 (GUI, Touch ID) is the blessing machine. The **M5 is verify-only** — SE key *creation* works headless but `userPresence` *use* never can over SSH, so enrolling it as a blesser is a no-op; provision it with the trust store + anchor only. Precondition gate before design freeze: probe M5's `sudo -ln` (must NOT be NOPASSWD — a NOPASSWD machine can't protect its anchor). **Probed 2026-07-12: PASS** (`sudo: a password is required` over non-interactive SSH).

### Mechanism

- **Signed pre-image (authoritative — applies at sign AND verify, everywhere):** `sha256("forgectl-bless-v1:workflow\n" ‖ raw file bytes)` for workflow blessings; `sha256("forgectl-bless-v1:trust\n" ‖ store bytes)` for the trust store. Domain separation prevents a workflow signature being replayed as a trust-store signature. Go computes the tagged digest on both sides; the helper signs a digest blind and never sees the file.
- **Verify path (always-on, security-critical): pure Go.** ECDSA P-256 over the tagged digest of the **raw file bytes**, before TOML decode (authenticate-before-parse). `CGO_ENABLED=0` untouched; main-binary build unchanged.
- **Sign/enroll ceremony: small Swift helper binary** (`forgectl-bless-helper`), invoked via the existing `internal/exec` Runner seam (house idiom — tmux/git/gh are shell-outs; fakeable with `FakeRunner`; the pattern mirrors `secretive`).
- **Blessing artifact: scheme-tagged sidecar** (`<name>.workflow.toml.blessing`): schema version, `algo="ecdsa-p256-sha256"`, key id, b64 DER signature, timestamp. Workflow bytes stay pristine (ADR-0001: one-byte edit invalidates).
- **TOCTOU:** read bytes once → verify those bytes → parse those same bytes. Never re-read.

### Trust anchoring (the verify side must also be out of the agent's reach)

- **Trust store** lists enrolled machine public keys. The store is itself a blessed file **signed by the anchor key specifically** (the first-enrolled local key — not "any enrolled key"); `trust add` re-signs with the anchor key. An agent edit invalidates it → fail closed.
- **Root-of-trust subtlety:** creating an SE key does *not* require user presence (only *using* it does) — an agent can mint keys. Anchor: `/etc/forgectl/trust-anchor.pub` (b64 PKIX of the anchor pubkey), written **once** at `trust init` via `sudo install -o root -g wheel -m 0644` — an interactive password prompt (`pam_tid` is not enabled on the M3, so this is a typed password; Touch ID-for-sudo only if `sudo_local` gains `pam_tid`). Path **compiled in, never configurable** (a config-settable anchor path is an agent-writable bypass). `trust init` refuses if the anchor exists.
- **Rotation is a full re-establishment (documented blast radius):** `sudo rm` the anchor + re-init mints a new anchor key → the store signature and **every workflow sidecar** signed under the old key fail closed until re-enrolled/re-signed/re-blessed (one Touch ID tap per workflow). The enrolled peer-key list is lost with the store — ADR-0006 names this cost; no silent carry-forward.
- **Fail closed** on missing/invalid anchor, store, or sidecar. Worst-case agent capability is denial (delete the store → workflows stop running), never bypass.

### The `--param` injection boundary (blessing covers bytes; params arrive at run time)

`interpolatePlanStep` interpolates **both `cmd` and `args`** of a `run` step, and `resolveParams` currently passes **undeclared** `--param` names straight through — so any `${name}` in a run step is agent-controllable at run time. Two controls:

1. **Reject undeclared `--param` names at resolve time** (a standalone hardening fix — nothing should silently accept params the workflow never declared).
2. **`workflow bless` refuses any `run` step whose `cmd` *or* `args` reference a `${param}`** (declared or not). References to **exports** of earlier steps (`${workspace}`, `${review}`) remain allowed — they're step-produced, not CLI-injectable, and are the core use case (`cmd="make", args=["-C","${workspace}"]`). The partition rule: a `${name}` resolvable to an earlier step's declared `Exports` is an export ref; anything else in a run step is refused at bless time. (Residual, documented: a blessed workflow that executes content from a cloned repo runs whatever that repo holds — the human blessed that *pattern*.)

### Policy

- `workflow run`: **enforce always** — refuse unsigned/invalid/tampered. No warn mode. The `ErrUnblessed` refusal message names the fix: `run 'forgectl workflow bless <name>' to approve this file`.
- `--dry-run`: allowed unsigned (executes nothing; BurntSushi TOML decode is the only surface exercised — named trade in ADR-0006). Authoring loop: author → dry-run → bless (one Touch ID tap) → run.
- Built-ins (`go:embed`): exempt — compiled into the binary, which is the trust surface. **`Source.Builtin` is pinned to byte provenance** (these bytes came from the embed FS), never to name-matching: a user file shadowing a builtin name is `Builtin=false` and requires blessing.
- Re-bless cost: one Touch ID tap per content change. Friction is the feature.

### Relationship to the issue arc

- **#10** — this work, reframed from author-provenance to human-presence blessing. Update the issue body.
- **#15** (attest outputs) / **#17** (signed registry) stack on the same signature/trust infrastructure later; registry entries would add *author* signatures as a second domain-tagged type. Licensing question stays parked with #17.
- ADR-0006 corrects ADR-0002's "no interface change" claim: `Verify(path string)` must become `Verify(path string, data []byte)` (one implementer, one call site).

## Implementation plan

Target tree: **current `main`** (`de768a9`, module architecture integrated). Fresh feature branch; worktree per session-entry posture.

### Spike first (throwaway, before commitments)

**Can an ad-hoc-signed CLI create/use Secure Enclave keys with `userPresence`?** RESULT: yes, via CryptoKit sealed blobs (see ladder table). The keychain-persistence route is entitlement-gated and dead for ad-hoc; no signing identity purchased (per the pre-committed off-ramp).

### Packaging — library, not module

Blessing is **not** a new `module.Manifest` (verbs are `workflow bless|verify|trust` subverbs; ADR-0005: domain logic in plain libraries). Module count pin stays 16.

New: `internal/bless/` — `envelope.go` (sidecar TOML; domain-tagged digest computation lives here, one function used by sign and verify), `keys.go` (PKIX-DER↔b64, `sha256:`-hex fingerprint), `trust.go` (store schema, anchor-verified load, root-ownership check via injectable stat seam), `verify.go` (implements `workflow.Verifier`; typed errors `ErrUnblessed`/`ErrTampered`/`ErrUnknownKey`/`ErrTrustStoreInvalid`/`ErrNoAnchor`), `blesser.go` (`Blesser` interface {Enroll, PublicKey, Sign} + `HelperBlesser` over `exec.Runner`; helper discovery: dir of `os.Executable()`, `FORGECTL_BLESS_HELPER` for dev), `bless.go` (ceremonies incl. the `${param}`-in-run-step refusal), tests. Plus `internal/cli/workflow_bless.go` (+test), `helper/forgectl-bless-helper/` (Swift, ~250 LOC, outside the Go module), `docs/adr/0006-workflow-blessing-user-presence-signing.md`.

Changed: `internal/workflow/verify.go` (signature change; **delete `AllowAllVerifier`** — no exported re-enable path), `internal/workflow/builtins.go` (split `Resolve` → `Load(name) (Source{Name,Path,Data,Builtin}, error)`, `Builtin` = byte provenance; `Resolve` stays as `Load`+`Parse` wrapper), `internal/workflow/plan.go` (`resolveParams` rejects undeclared params), `internal/cli/workflow.go` (byte-flow + subverbs), `internal/config/config.go` (`TrustStorePath()` = `<configDir>/trust.toml` — `configDir()` already appends `forgectl`, no double-nesting; plus an injectable config-dir test seam), `.goreleaser.yaml` + `.github/workflows/{release,ci}.yml`.

`internal/bless` imports only `internal/exec`, `internal/config`, stdlib crypto. CGO stays 0.

### Byte-flow rewiring (run path)

```go
src, _ := workflow.Load(name)            // bytes read exactly ONCE
if !src.Builtin && !dryRun {             // builtin exemption (byte provenance) + authoring loop
    if err := bless.NewVerifier().Verify(src.Path, src.Data); err != nil { return … }
}
wf, _ := workflow.Parse(src.Data)        // THE SAME bytes — TOCTOU closed by construction
```

### Trust material — verify order (fail closed at every step)

Sidecar next to the workflow (agent-writable dir is fine — any edit to either file fails verification). Store + its `.blessing` under `<configDir>/`. Order: (1) anchor stat — regular file, uid 0, not group/world-writable (injectable check) → (2) parse anchor P-256 → (3) store raw bytes + sidecar: key_id == anchor fingerprint, verify **trust-domain** tagged digest, only then TOML-decode → (4) workflow sidecar: missing → `ErrUnblessed`; unknown key_id → `ErrUnknownKey`; bad sig over the **workflow-domain** tagged digest → `ErrTampered`.

Verify is pure Go — works on Linux if store/anchor provisioned; sign/enroll on non-darwin returns a typed "no blessing backend on this platform" error (the seam for Hello/FIDO2/passphrase).

### Helper contract (`forgectl-bless-helper`)

Go pipes the b64 32-byte **tagged** digest via `Runner.RunWithInput`; JSON on stdout; typed exit codes (2 = user cancelled Touch ID, 3 = label exists, 4 = key not found, 5 = bad digest). Verbs: `enroll --label`, `pubkey --label`, `sign --label`, `version`. Swift: CryptoKit `SecureEnclave.P256.Signing.PrivateKey` (+`SecAccessControl(privateKeyUsage, .userPresence)`), key persisted as sealed `dataRepresentation` blob under the helper's key dir; sign via `key.signature(for: digest)`; exports PKIX DER so Go stays `ParsePKIXPublicKey`.

### CLI verbs (inside `newWorkflowCmd`)

- `workflow bless <name>` — refuse builtins; require valid trust store; refuse `${param}` in run-step cmd/args; Touch ID → write sidecar 0644.
- `workflow verify <name>` — typed result, non-zero exit (preflight/CI use).
- `workflow trust init` — **idempotent enroll** (reuse an existing labeled key rather than exit-3 wedging) → sign store (Touch ID) → sudo-install anchor; refuse if anchor exists; **recoverable**: a failed/cancelled sudo leg leaves key+store reusable, re-run completes the anchor write only.
- `workflow trust list` · `workflow trust add --pubkey --machine` (peer enrollment; re-signed by the **anchor key**; may be a follow-on PR).

### Release / CI

Release job → **macos-latest**; pre-goreleaser step builds the Swift helper (universal, ad-hoc codesign). Split goreleaser build into darwin/linux ids; darwin archives carry the helper via `files:`; extend the cask `xattr` hook — **verify goreleaser v2 `homebrew_casks` supports a second binary; else `custom_block`**. `Version.swift` generated from the tag; forgectl *warns* on helper-version mismatch (ceremony path only). CI adds a macos `swift build` job; Go job untouched.

### Sequencing & size

1. SE spike (throwaway) → DONE, sealed-blob rung selected.
2. PR 1: `internal/bless` + `resolveParams` hardening + tests; `helper/` + CI job.
3. PR 2: `Load` split + `Verifier` signature change + CLI wiring/verbs; ADR-0006.
4. PR 3: goreleaser/cask/release-lane changes.

≈ 2,300–2,800 LOC total. M5 provisioning (store + anchor copy, verify-only) after PR 3 ships via brew.

## Verification

- **Unit table** (`internal/bless` + CLI tests; generated test keys, `t.TempDir()`, injected anchor-ownership check): bless→verify round-trip; tamper one byte → `ErrTampered`; missing sidecar → `ErrUnblessed`; unknown key → `ErrUnknownKey`; store tampered → `ErrTrustStoreInvalid`; anchor missing/non-root/world-writable → `ErrNoAnchor`; **domain separation — a workflow signature presented as a trust-store signature is rejected (and vice versa)**; **bless refuses `${param}` in run-step cmd AND args, allows export refs**; **undeclared `--param` rejected at resolve**; **`trust init` refuses when anchor exists**; **anchor path ignores any config override (compiled-in)**; **shadow-builtin user file gets `Builtin=false` → requires blessing**; dry-run unsigned allowed; run unsigned refused with the bless-command hint; builtin exempt (spy verifier never consulted); TOCTOU (`Load` → `os.Remove` → `Parse(src.Data)` succeeds); helper argv/stdin contract via `FakeRunner` (digest as `Call.Input`, canned JSON with a real test-key signature); `sudo install` recorded `Interactive: true`; exit-code→typed-error mapping; **trust-init resume after cancelled sudo leg**.
- **Live end-to-end on the M3:** `trust init` (Touch ID + sudo password) → author a workflow → `run` refused (message names `bless`) → `bless` (Touch ID) → `run` executes → edit one byte → refused again.
- **Agent-simulation check (from a Claude session):** (a) run unblessed, (b) edit a blessed file and run, (c) edit trust.toml and run, (d) attempt `bless`, (e) **drive a blessed workflow via `--param` into a run step** — all five must fail/refuse without a human present.
- **M5 precondition probe** (before design freeze): DONE — PASS.

## Panel review — findings declined

- **"Keychain fallback (a) voids the human-presence guarantee (extractable key → agent signs silently)"** — declined as overstated at plan time; the spike then mooted the rung entirely (unreachable ad-hoc). The shipped design's sealed blob is presence-gated by the SE itself.

## Alternatives declined

- **minisign / cosign-keyless / GPG author signing** (original #10 candidates) — proves authorship, not human presence; an agent holding the file and a software key could self-sign. Kept as future *registry* (#17) signature type. Note (owner lens): this plan rolls **no crypto** — stdlib `crypto/ecdsa` + PKIX + Apple CryptoKit/`SecKey`; only the *trust-anchoring* is bespoke.
- **sudo-gated hash-allowlist (~300–500 LOC)** — escalated to the owner rather than declined: delivers the core invariant far cheaper, pure Go, cross-platform; costs the signature/provenance arc (#15/#17 would rebuild) and uses a typed-password ceremony with a closable cache window. Decision 2026-07-12: full SE + Touch ID, eyes open on cost.
- **sudo-gated key file as the per-bless ceremony** — sudo timestamp cache; acceptable only for one-time enrollment.
- **CGO Security.framework bindings in forgectl proper** — breaks `CGO_ENABLED=0`; helper split keeps verify pure-Go.
- **Warn-mode default** — warn is invisible when an agent runs the command; enforce-always is the point.
