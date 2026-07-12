# 0006 — Workflow blessing: user-presence signing, not author signing

- **Status:** Accepted (2026-07-12)
- **Context:** #10, reframed. Related: 0001, 0002 (corrects one consequence), 0004; groundwork for #15/#17.

## Context

A workflow file is an arbitrary-command-execution surface: `run` steps execute anything, and
agent-harness guardrails see only the outer `forgectl workflow run <name>`, never the steps
inside. #10 originally planned conventional author signing (minisign/cosign). That scheme
answers the wrong question here: the adversary is not a tampering third party but **the local
agent itself** — Claude (or any agent) can author a workflow file, hold a software signing key,
and self-sign. Author signing proves *a trusted key signed it*; the control this tool needs
proves *a human approved these exact bytes*.

**Threat boundary (explicit):** an agent that holds the user's account password or NOPASSWD
sudo is out of scope — such an agent already owns the machine. The design defends against an
agent with ordinary user-level file/exec access.

## Decision

**Blessing**: a human-presence ceremony signs the exact bytes of a workflow file; `workflow run`
refuses anything unblessed, always (no warn mode — a warning is invisible when an agent runs
the command).

- **Ceremony (macOS rung, first):** a Secure Enclave P-256 key created with a
  `userPresence` access control — Touch ID or account password **per signature**, no cache
  window, key cannot leave the chip. Persisted as a CryptoKit **sealed blob file**
  (`dataRepresentation`), not a keychain item: the spike showed the data-protection keychain
  demands a `keychain-access-groups` entitlement an ad-hoc-signed CLI cannot hold (`-34018`;
  self-granted entitlements → AMFI SIGKILL), while the sealed blob needs no keychain at all.
  The access control is baked into the blob and enforced by the SE/coreauthd — an agent
  loading the blob with its own code still faces the prompt (verified:
  `LAContext.interactionNotAllowed = true` → "User interaction is required"). The blob file
  therefore needs no special protection: useless off-device, presence-gated on-device.
- **Split binary:** the ceremony lives in a small Swift helper (`forgectl-bless-helper`)
  invoked through the `exec.Runner` seam; it signs a 32-byte digest blind and never sees the
  file. The **verify path is pure Go** (stdlib `crypto/ecdsa` + PKIX) — `CGO_ENABLED=0`
  untouched, verification works on any platform the trust material reaches.
  The helper is resolved **only** as a sibling of the running executable. There is
  deliberately **no environment override** for that path: the helper serves `Enroll` and
  `PublicKey` as well as `Sign`, and `trust init` enrolls at the one moment no trust store
  exists yet — so whatever public key the helper returns *becomes the anchor*. An agent that
  can append an `export` to a shell rc file (well within a same-UID adversary's reach) would
  otherwise have the human's own `sudo` install the **agent's** key as the root of trust, with
  no Touch ID prompt ever appearing. Injectability for tests lives in in-process Go vars,
  which nothing outside the process can reach.
- **The anchor is written from argv, never from a file the invoking user can rewrite.** A
  single interactive `sudo sh -c` leg receives the base64 public key as a positional argument
  and creates the anchor under `set -C` (noclobber → `O_EXCL`: atomic, refuses an existing
  file, refuses a symlink), then the install is **read back and compared** to the key we meant
  to install. Staging through a temp file would leave it writable across the *interactive sudo
  wait* — seconds while the human types a password — and a same-UID agent could swap its
  contents so `sudo` rooted the attacker's key. The anchor is a **public** key, so carrying it
  in argv costs no confidentiality and buys immutability at exec time.
- **Domain-separated pre-image:** `sha256("forgectl-bless-v1:workflow\n" ‖ bytes)` for
  workflows, `sha256("forgectl-bless-v1:trust\n" ‖ bytes)` for the trust store — a workflow
  signature can never be replayed as a trust-store signature or vice versa.
- **Sidecar envelope** (`<file>.blessing`: schema, algo, key id, DER signature, timestamp);
  workflow bytes stay pristine, one edited byte invalidates (0001). Verification runs on the
  **raw bytes before TOML decode** (authenticate-before-parse), and the run path reads bytes
  exactly once — verify and parse consume the same buffer (TOCTOU closed by construction).
- **Trust anchoring:** enrolled machine keys live in a trust store that is itself signed by
  the **anchor key specifically** (the first-enrolled key). The anchor public key lives at
  `/etc/forgectl/trust-anchor.pub`, written once via interactive `sudo install` — the path is
  **compiled in, never configurable** (a config-settable anchor path is an agent-writable
  bypass). Creating SE keys needs no presence — an agent can mint keys — so the root of trust
  is the root-owned anchor, not key possession. **Fail closed** on missing/invalid anchor,
  store, or sidecar: the worst an agent can do is denial (delete the store), never bypass.
- **The `--param` boundary:** blessing covers bytes; params arrive at run time and interpolate
  into `run` steps' `cmd`/`args`. Two controls: `resolveParams` rejects undeclared `--param`
  names outright, and `workflow bless` refuses any `run` step whose `cmd` or `args` reference
  a `${param}` — only references resolvable to an **earlier step's declared exports**
  (`${workspace}`, `${review}`) are allowed, because those are step-produced, not
  CLI-injectable. Residual (documented): a blessed workflow that executes content from a
  cloned repo runs whatever that repo holds — the human blessed that *pattern*.
- **Built-ins exempt** — compiled into the binary, which is the trust surface. The exemption
  keys on **byte provenance** (the bytes came from the embed FS), never name-matching: a user
  file shadowing a builtin name requires blessing.
- **`--dry-run` allowed unsigned** (named trade): it executes nothing; strict TOML decode is
  the only surface exercised. This keeps the authoring loop humane: author → dry-run →
  bless (one Touch ID tap) → run. Re-blessing on every content change is the point — friction
  is the feature.
- **Correction to 0002:** its consequences claimed #10 needs "no interface change". Wrong by
  one parameter: authenticate-before-parse on bytes read once requires
  `Verify(path string, data []byte)`. One implementer, one call site; `AllowAllVerifier` is
  deleted outright rather than left as an exported re-enable path.

## Alternatives considered

- **minisign / cosign-keyless / GPG author signing** (the original #10). Rejected: proves
  authorship, not presence — an agent holding a software key self-signs silently. Kept as a
  future *registry* signature type (#17), where author provenance is the right question.
- **sudo-gated hash allowlist** (root-owned `blessed.sha256`, appended via sudo). Escalated
  to the owner rather than rejected outright: delivers the core invariant far cheaper, pure
  Go, cross-platform — but costs the signature/provenance arc #15/#17 build on, and sudo's
  timestamp cache is a closable-but-real window. Owner decision 2026-07-12: full SE +
  Touch ID, eyes open on cost.
- **Keychain-held key with a `userPresence` ACL** (non-SE fallback). Mooted by the spike:
  unreachable for an ad-hoc CLI (the data-protection keychain itself is entitlement-gated),
  and the sealed blob is strictly better — same ceremony, no Apple gate.
- **sudo as the per-bless ceremony.** Rejected: the sudo timestamp cache means one typed
  password opens a window an agent can ride. Acceptable only for the one-time anchor install.
- **CGO Security.framework bindings in forgectl proper.** Rejected: breaks `CGO_ENABLED=0`
  and drags the ceremony into the always-on binary; the helper split keeps verify pure Go.
- **Warn-mode default.** Rejected: warnings print to a terminal the agent, not the human, is
  reading.

## What review caught (recorded, because the misses are instructive)

Three reviews ran: a security lens, a code-quality lens, and CodeRabbit. The first two cleared
the trust core and found real but narrower issues. **CodeRabbit found the two bugs that
actually mattered**, and both were bootstrap bugs — the moment the root of trust is
*established*, not the steady state it protects:

1. The anchor staged through a user-writable temp file across the interactive sudo wait.
2. The helper-path env override, which also served enrollment.

Both let a same-UID agent get **its own key installed as the root-owned anchor** — total
bypass, no root and no password required.

A **third**, found by adversarial dogfooding (planting a key and running the real helper) and
independently confirmed by a second review pass, was the sharpest: the key-blob directory is
agent-writable and SE key **creation needs no presence**, so an agent mints its own
*non-presence* key, writes the blob at the compiled-in label, and waits. `trust init`'s
idempotent-enroll fallback (added so a cancelled-sudo run could resume) then enrolls the
agent's key, signs the store with it **silently** — no Touch ID, because it isn't
presence-gated — and anchors it. The convenience affordance was the exploit; it could not tell
its own prior key from an agent's plant. **Fix:** the helper *proves* a key is presence-gated
before trusting it — a signature attempted under `LAContext.interactionNotAllowed = true`
succeeds for a non-presence key and throws `-1004` for a presence-gated one (a discriminator
that needs no human and shows no UI, verified on hardware); `pubkey`/`sign` refuse a
non-presence key (exit 6 → `ErrKeyNotPresenceGated`), which `EnsureKey` propagates rather than
reusing.

The reason two reviewers walked past #2 is worth naming: the code carried a confident doc
comment asserting the override was *"deliberate and safe — it redirects only the SIGN side."*
That sentence was wrong, and it read as evidence. A comment claiming a security property is a
**claim, not a proof**; it should carry the argument, or it becomes the thing that hides the
bug. Both fixes are now pinned by regression tests
(`TestNewHelperBlesser_EnvVarCannotRedirectHelper`,
`TestInstallAnchor_SingleSudoLegCarriesKeyInArgv`,
`TestInstallAnchor_PostVerifyCatchesSwappedKey`).

Generalizable: **a trust system's weakest moment is its bootstrap.** Steady-state verification
got the most design attention and held up; establishment — where there is not yet a store to
check against — is where *all three* compromises lived. Review the ceremony that *creates*
trust at least as hard as the one that *checks* it.

### Residual: binary integrity is assumed (tracked, not yet closed)

The sibling-discovery fix assumes the helper binary *at* the sibling path is trustworthy. On
Apple Silicon the default Homebrew prefix (`/opt/homebrew`) is **user-owned**, so a same-UID
agent can overwrite `forgectl-bless-helper` — or `forgectl` itself — with an ad-hoc-signed
impostor and win the same bootstrap by a different door. This is at the edge of the threat
boundary: if the install prefix is agent-writable, the always-on `forgectl` binary (which runs
the pure-Go verify path in-process) is equally rewritable, so binary integrity is a base
assumption of the whole feature, not something the code can enforce against a same-UID
adversary without a Developer ID signature (which the spike's off-ramp declined to buy) or a
root-owned install path. **Stated, not silently assumed:** the guarantee holds only where
forgectl and its helper are installed to a location the invoking user's agents cannot
overwrite. Closing it for the default brew install is tracked as a follow-on (Developer ID
signing + `codesign` verification before exec, or a root-owned `libexec`).

## Consequences

- **Blessings travel; the ceremony doesn't have to.** A blessing is a per-content signature —
  any machine with the anchor + store verifies. Headless machines (the M5) are verify-only:
  SE `userPresence` can never be satisfied over SSH, which is the desired property, so they
  are provisioned with trust material and never enrolled as blessers.
- **Rotation is a full re-establishment.** `sudo rm` the anchor + re-init mints a new anchor:
  the store signature and every sidecar under the old key fail closed until re-enrolled,
  re-signed, and re-blessed (one tap per workflow). The enrolled peer-key list dies with the
  store. Named cost; no silent carry-forward.
- Other-platform rungs (Windows Hello/TPM, FIDO2 touch, passphrase floor) slot in behind the
  same `Blesser` seam; non-darwin sign/enroll returns a typed "no blessing backend" error
  until then.
- `#15` (attest outputs) and `#17` (signed registry) stack on this envelope/trust
  infrastructure as additional domain-tagged signature types.
