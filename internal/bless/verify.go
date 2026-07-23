package bless

import (
	"crypto/ecdsa"
	"encoding/base64"
	"errors"
	"fmt"
	"os"

	"github.com/cameronsjo/forgectl/internal/config"
)

// AnchorPath is the compiled-in, root-owned trust anchor location. It is a
// constant on purpose: a config-settable anchor path would be an agent-writable
// bypass of the entire root of trust. There is deliberately no plumbing to
// override it anywhere in the tool.
const AnchorPath = "/etc/forgectl/trust-anchor.pub"

// Typed sentinels for every verify failure. Callers (the CLI, tests) match on
// these with errors.Is; each real error wraps the appropriate sentinel with %w
// plus context. The five cover the full fail-closed surface: no anchor, an
// invalid trust store, an unblessed file, a tampered blessing, and a blessing by
// a key the store doesn't know.
var (
	// ErrUnblessed: no blessing sidecar exists for the workflow file.
	ErrUnblessed = errors.New("workflow is not blessed")
	// ErrTampered: a blessing sidecar exists but its signature does not match
	// the bytes (or the sidecar itself is malformed).
	ErrTampered = errors.New("workflow blessing does not match its bytes")
	// ErrUnknownKey: the blessing was made by a key not enrolled in the store.
	ErrUnknownKey = errors.New("workflow was blessed by a key not in the trust store")
	// ErrTrustStoreInvalid: the trust store is missing, unsigned by the anchor,
	// or otherwise not trustworthy.
	ErrTrustStoreInvalid = errors.New("trust store is missing or invalid")
	// ErrTrustStoreMissing marks the specific case where the trust store file
	// does not exist — a clean not-yet-created / deleted state — as distinct
	// from a file that exists but is unreadable, unsigned, or corrupt. It wraps
	// ErrTrustStoreInvalid, so existing errors.Is(err, ErrTrustStoreInvalid)
	// checks still match; a caller that must tell genuine absence from a
	// present-but-broken store (e.g. trust rebuild's peer-drop guard) tests for
	// ErrTrustStoreMissing specifically.
	ErrTrustStoreMissing = fmt.Errorf("%w (not found)", ErrTrustStoreInvalid)
	// ErrNoAnchor: the compiled-in anchor is missing, not root-owned, or
	// writable — the root of trust cannot be established.
	ErrNoAnchor = errors.New("trust anchor is missing or not root-owned")
)

// Verifier checks that a workflow file's bytes were blessed by an enrolled key
// under a trust store rooted at the compiled-in anchor. Its seams are
// unexported so same-package tests can inject a fake anchor path, a simulated
// ownership check, and a temp-dir trust store; NewVerifier wires the real
// compiled-in defaults.
type Verifier struct {
	anchorPath     string
	anchorCheck    AnchorCheckFunc
	trustStorePath func() (string, error)
}

// NewVerifier returns a Verifier wired with the production root of trust: the
// compiled-in AnchorPath, the real root-ownership check, and the config-derived
// trust-store path.
func NewVerifier() *Verifier {
	return &Verifier{
		anchorPath:     AnchorPath,
		anchorCheck:    checkAnchorOwnership,
		trustStorePath: config.TrustStorePath,
	}
}

// Anchor establishes the root of trust WITHOUT the store: it runs the
// fail-closed steps 1–2 of Verify — the anchor ownership check and the anchor
// key parse+fingerprint — and returns the parsed anchor key and its fingerprint.
// It is factored out of TrustedStore so a caller that must reconstruct a MISSING
// or corrupt store (the `trust rebuild` recovery verb) can reach the authenticated
// anchor alone, when TrustedStore itself would fail on the very store it is trying
// to rebuild. The read is uncached — every call re-reads and re-checks the anchor
// from disk.
//
// Order:
//  1. Anchor ownership: regular file, root-owned, not group/world-writable.
//  2. Parse the anchor P-256 key; derive its fingerprint (the id the store and
//     its signature must reference).
func (v *Verifier) Anchor() (*ecdsa.PublicKey, string, error) {
	// (1) Anchor ownership.
	if err := v.anchorCheck(v.anchorPath); err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrNoAnchor, err)
	}

	// (2) Parse the anchor and fingerprint its canonical PKIX DER.
	anchorBytes, err := os.ReadFile(v.anchorPath)
	if err != nil {
		return nil, "", fmt.Errorf("%w: read anchor %s: %v", ErrNoAnchor, v.anchorPath, err)
	}
	anchorPub, err := ParseAnchorFile(anchorBytes)
	if err != nil {
		return nil, "", fmt.Errorf("%w: parse anchor %s: %v", ErrNoAnchor, v.anchorPath, err)
	}
	anchorDER, err := EncodePublicKey(anchorPub)
	if err != nil {
		return nil, "", fmt.Errorf("%w: encode anchor key: %v", ErrNoAnchor, err)
	}
	return anchorPub, Fingerprint(anchorDER), nil
}

// TrustedStore establishes the root of trust and returns the authenticated
// trust store — the fail-closed steps 1–3 of Verify, factored out so the exact
// order lives in one place and callers that need the enrolled-key set (the
// `workflow bless` / `trust list` verbs) reach it without re-implementing the
// anchor→store chain.
//
// Order:
//  1. Anchor ownership: regular file, root-owned, not group/world-writable.
//  2. Parse the anchor P-256 key; derive its fingerprint (the id the store and
//     its signature must reference).
//  3. Authenticate the trust store BEFORE parsing it: its sidecar must be signed
//     by the anchor (trust domain) and name the anchor's key id; only then is the
//     store TOML decoded, and its anchor_key_id must match too.
func (v *Verifier) TrustedStore() (Store, error) {
	// (1)+(2) Anchor ownership, parse, and fingerprint — the uncached anchor read.
	anchorPub, anchorFP, err := v.Anchor()
	if err != nil {
		return Store{}, err
	}

	// (3) Authenticate-before-parse the trust store.
	storePath, err := v.trustStorePath()
	if err != nil {
		return Store{}, fmt.Errorf("%w: resolve trust store path: %v", ErrTrustStoreInvalid, err)
	}
	storeBytes, err := os.ReadFile(storePath)
	if err != nil {
		if os.IsNotExist(err) {
			// Genuine absence — distinct from a present-but-unreadable store, so a
			// caller can tell "nothing here" from "here but I can't read it".
			return Store{}, fmt.Errorf("%w: %s", ErrTrustStoreMissing, storePath)
		}
		return Store{}, fmt.Errorf("%w: read trust store %s: %v", ErrTrustStoreInvalid, storePath, err)
	}
	storeSidecar, err := os.ReadFile(SidecarPath(storePath))
	if err != nil {
		return Store{}, fmt.Errorf("%w: read trust store sidecar: %v", ErrTrustStoreInvalid, err)
	}
	storeEnv, err := DecodeEnvelope(storeSidecar)
	if err != nil {
		return Store{}, fmt.Errorf("%w: trust store sidecar: %v", ErrTrustStoreInvalid, err)
	}
	if storeEnv.KeyID != anchorFP {
		return Store{}, fmt.Errorf("%w: trust store signed by %s, not the anchor %s", ErrTrustStoreInvalid, storeEnv.KeyID, anchorFP)
	}
	storeSig, err := base64.StdEncoding.DecodeString(storeEnv.Signature)
	if err != nil {
		return Store{}, fmt.Errorf("%w: trust store signature is not valid base64: %v", ErrTrustStoreInvalid, err)
	}
	storeDigest := TaggedDigest(DomainTrust, storeBytes)
	if !ecdsa.VerifyASN1(anchorPub, storeDigest[:], storeSig) {
		return Store{}, fmt.Errorf("%w: trust store signature does not verify under the anchor key", ErrTrustStoreInvalid)
	}
	// Signature passed — only now is it safe to parse the store bytes.
	store, err := DecodeStore(storeBytes)
	if err != nil {
		return Store{}, fmt.Errorf("%w: %v", ErrTrustStoreInvalid, err)
	}
	if store.AnchorKeyID != anchorFP {
		return Store{}, fmt.Errorf("%w: store anchor_key_id %s does not match the anchor %s", ErrTrustStoreInvalid, store.AnchorKeyID, anchorFP)
	}
	return store, nil
}

// Verify checks that data (the workflow file's bytes, read exactly once by the
// caller) was blessed. It follows a strict fail-closed order and never re-reads
// the workflow file — TOCTOU is closed by construction because the caller hands
// in the same bytes it will later parse.
//
// Steps 1–3 (anchor → authenticated store) are TrustedStore; step 4 here
// authenticates the workflow blessing: the sidecar must exist (else unblessed),
// decode cleanly, name a key the store knows, and carry a signature that
// verifies over the workflow-domain tagged digest of data.
func (v *Verifier) Verify(path string, data []byte) error {
	store, err := v.TrustedStore()
	if err != nil {
		return err
	}

	// (4) Authenticate the workflow blessing against the trusted store.
	wfSidecar, err := os.ReadFile(SidecarPath(path))
	if err != nil {
		// Only a genuinely absent sidecar means "unblessed" (and earns the
		// how-to-fix hint); any other read failure (permissions, EISDIR) is its
		// own fail-closed error so the user debugs the real problem.
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: run 'forgectl workflow bless <name>' to approve this file", ErrUnblessed)
		}
		return fmt.Errorf("read blessing sidecar %s: %w", SidecarPath(path), err)
	}
	wfEnv, err := DecodeEnvelope(wfSidecar)
	if err != nil {
		return fmt.Errorf("%w: malformed blessing sidecar: %v", ErrTampered, err)
	}
	trusted, ok := store.Lookup(wfEnv.KeyID)
	if !ok {
		return fmt.Errorf("%w: key %s is not enrolled in the trust store", ErrUnknownKey, wfEnv.KeyID)
	}
	keyDER, err := base64.StdEncoding.DecodeString(trusted.Pubkey)
	if err != nil {
		return fmt.Errorf("%w: store key %s pubkey is not valid base64: %v", ErrTrustStoreInvalid, wfEnv.KeyID, err)
	}
	// Defense in depth: the store is anchor-signed, so a key_id/pubkey mismatch
	// can only come from buggy enrollment tooling — but a fingerprint that
	// doesn't match its own pubkey means the store's identity claims are broken,
	// and verification must not proceed on a key looked up by a false id.
	if got := Fingerprint(keyDER); got != trusted.KeyID {
		return fmt.Errorf("%w: store key %s pubkey fingerprints to %s", ErrTrustStoreInvalid, trusted.KeyID, got)
	}
	keyPub, err := ParsePublicKey(keyDER)
	if err != nil {
		return fmt.Errorf("%w: store key %s: %v", ErrTrustStoreInvalid, wfEnv.KeyID, err)
	}
	wfSig, err := base64.StdEncoding.DecodeString(wfEnv.Signature)
	if err != nil {
		return fmt.Errorf("%w: workflow signature is not valid base64: %v", ErrTampered, err)
	}
	wfDigest := TaggedDigest(DomainWorkflow, data)
	if !ecdsa.VerifyASN1(keyPub, wfDigest[:], wfSig) {
		return fmt.Errorf("%w: workflow signature does not verify", ErrTampered)
	}
	return nil
}
