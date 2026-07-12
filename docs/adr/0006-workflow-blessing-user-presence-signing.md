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
