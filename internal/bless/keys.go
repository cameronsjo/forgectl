package bless

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// ParsePublicKey decodes a PKIX-DER public key and enforces the ONE key shape
// this build trusts: ECDSA on the P-256 curve. Any other key type or curve is
// refused — the verifier must never accept a key the ceremony can't have
// produced, and narrowing the accepted shape here keeps a downgraded or exotic
// key from reaching ecdsa.VerifyASN1.
func ParsePublicKey(der []byte) (*ecdsa.PublicKey, error) {
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("parse PKIX public key: %w", err)
	}
	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("public key is %T, want ECDSA", pub)
	}
	if ecPub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("public key curve is %s, want P-256", ecPub.Curve.Params().Name)
	}
	return ecPub, nil
}

// EncodePublicKey serialises an ECDSA public key to PKIX DER — the on-the-wire
// form used by the anchor file, the trust store, and the helper's enroll/pubkey
// output. Fingerprints are computed over exactly these bytes.
func EncodePublicKey(pub *ecdsa.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("marshal PKIX public key: %w", err)
	}
	return der, nil
}

// Fingerprint returns the canonical key id for a PKIX-DER public key:
// "sha256:" plus the lowercase hex of the SHA-256 over the DER bytes. It hashes
// the raw bytes as given — callers pass the same PKIX DER they store, so the
// fingerprint an anchor advertises matches the one a store entry records.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ParseAnchorFile decodes an anchor file: a single base64-std line of PKIX DER,
// with optional trailing whitespace/newline. It returns the parsed P-256 public
// key. The anchor is the root of trust — it lives at a compiled-in, root-owned
// path (see AnchorPath) so an agent cannot substitute its own key.
func ParseAnchorFile(b []byte) (*ecdsa.PublicKey, error) {
	line := strings.TrimSpace(string(b))
	if line == "" {
		return nil, fmt.Errorf("anchor file is empty")
	}
	der, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		return nil, fmt.Errorf("anchor file is not valid base64: %w", err)
	}
	return ParsePublicKey(der)
}
