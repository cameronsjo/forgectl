// Package bless is the workflow-blessing trust core: it verifies that a human
// approved a workflow file's exact bytes via a user-presence ceremony (Touch ID
// on macOS), and assembles the signature envelopes that prove it.
//
// The threat model treats the local agent as the adversary (ADR-0006): an agent
// can author and run workflow files, so the control is a ceremony the agent
// cannot perform. Signing therefore shells out to a small helper binary through
// the exec.Runner seam (see blesser.go); verification is pure Go so the
// always-on path never needs CGO and works headless.
//
// Import discipline: this package depends only on stdlib, internal/exec, and
// internal/config. It MUST NOT import internal/workflow or internal/step — the
// verify path authenticates raw bytes before any TOML decode, so it cannot
// depend on the parser it protects.
package bless

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// Domain is a signing-domain tag prepended to the pre-image before hashing.
// Domain separation ensures a signature made over one kind of content can never
// be replayed as a signature over another: a workflow blessing and a trust-store
// blessing hash disjoint pre-images even for identical bytes.
type Domain string

const (
	// DomainWorkflow tags a workflow-file blessing. The trailing newline is part
	// of the byte-exact tag and MUST NOT change — it is baked into every existing
	// signature.
	DomainWorkflow Domain = "forgectl-bless-v1:workflow\n"
	// DomainTrust tags a trust-store blessing (the store is itself a signed file,
	// signed specifically by the anchor key). Same byte-exact-tag rule.
	DomainTrust Domain = "forgectl-bless-v1:trust\n"
)

// EnvelopeSchema is the only sidecar schema version this build understands. A
// sidecar declaring anything else is rejected — a newer schema's fields must
// never be silently dropped.
const EnvelopeSchema = 1

// AlgoECDSAP256SHA256 is the only signature algorithm this build accepts: ECDSA
// over the P-256 curve, ASN.1/DER-encoded, over a SHA-256 tagged digest.
const AlgoECDSAP256SHA256 = "ecdsa-p256-sha256"

// keyIDPattern is the canonical key-id form: "sha256:" plus 64 lowercase hex
// digits (the SHA-256 fingerprint of a PKIX-DER public key). Anchored so a
// trailing-garbage id is rejected.
var keyIDPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// TaggedDigest computes sha256(tag ‖ data) for a signing domain. It is the ONE
// function used by both sign and verify, so the domain-separation property holds
// by construction: signer and verifier can only ever agree when they pass the
// same Domain. A workflow signature can never authenticate trust-store bytes
// because the two domains hash different pre-images.
func TaggedDigest(d Domain, data []byte) [32]byte {
	pre := make([]byte, 0, len(d)+len(data))
	pre = append(pre, []byte(d)...)
	pre = append(pre, data...)
	return sha256.Sum256(pre)
}

// Envelope is the parsed form of a *.blessing sidecar. It carries everything a
// verifier needs to re-derive and check a signature: which key signed, over
// which algorithm, and the signature itself (base64-std of the ASN.1/DER ECDSA
// signature). The signed bytes themselves live in the adjacent workflow (or
// trust-store) file — the envelope never embeds them, so a one-byte edit to the
// signed file invalidates the blessing.
//
// Only the signature is cryptographically load-bearing: key_id selects the
// verification key (a wrong id simply fails verification) and signed_at is
// unauthenticated advisory metadata — nothing may treat envelope fields other
// than the verified signature as trustworthy.
//
//	schema    = 1
//	algo      = "ecdsa-p256-sha256"
//	key_id    = "sha256:<64 lowercase hex>"
//	signature = "<base64 std of ASN.1/DER ECDSA sig>"
//	signed_at = "RFC3339 string"
type Envelope struct {
	Schema    int    `toml:"schema"`
	Algo      string `toml:"algo"`
	KeyID     string `toml:"key_id"`
	Signature string `toml:"signature"`
	SignedAt  string `toml:"signed_at"`
}

// SidecarPath returns the blessing sidecar path for a signed file: the file path
// with ".blessing" appended. The sidecar sits next to the workflow — an
// agent-writable directory is fine, since any edit to either file fails
// verification.
func SidecarPath(path string) string {
	return path + ".blessing"
}

// EncodeEnvelope serialises an Envelope to sidecar TOML bytes.
func EncodeEnvelope(e Envelope) ([]byte, error) {
	var b strings.Builder
	if err := toml.NewEncoder(&b).Encode(e); err != nil {
		return nil, fmt.Errorf("encode envelope: %w", err)
	}
	return []byte(b.String()), nil
}

// DecodeEnvelope parses sidecar TOML into an Envelope and validates it. The
// decode is STRICT — mirroring workflow.Parse, an unknown key is a hard error
// rather than a silent drop, so a tampered or forward-versioned sidecar is
// refused. Validation enforces the frozen envelope contract: schema == 1, the
// one supported algo, a canonical key_id, and a syntactically valid base64
// signature. (Signature *verification* is the verifier's job; this only proves
// the field is decodable base64, catching a corrupt sidecar early.)
func DecodeEnvelope(data []byte) (Envelope, error) {
	var e Envelope
	md, err := toml.Decode(string(data), &e)
	if err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return Envelope{}, fmt.Errorf("decode envelope: unknown key(s) %s", joinKeys(undecoded))
	}
	if e.Schema != EnvelopeSchema {
		return Envelope{}, fmt.Errorf("decode envelope: unsupported schema %d (want %d)", e.Schema, EnvelopeSchema)
	}
	if e.Algo != AlgoECDSAP256SHA256 {
		return Envelope{}, fmt.Errorf("decode envelope: unsupported algo %q (want %q)", e.Algo, AlgoECDSAP256SHA256)
	}
	if !keyIDPattern.MatchString(e.KeyID) {
		return Envelope{}, fmt.Errorf("decode envelope: malformed key_id %q", e.KeyID)
	}
	if _, err := base64.StdEncoding.DecodeString(e.Signature); err != nil {
		return Envelope{}, fmt.Errorf("decode envelope: signature is not valid base64: %w", err)
	}
	return e, nil
}

// joinKeys renders a []toml.Key list for an error message, mirroring the
// workflow parser's unknown-key reporting.
func joinKeys(keys []toml.Key) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k.String()
	}
	return strings.Join(parts, ", ")
}
