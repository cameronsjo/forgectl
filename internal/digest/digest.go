// Package digest centralizes forgectl's canonical content-hash form so every
// producer of a "sha256:<hex>" identifier — key fingerprints, workflow
// definition hashes, per-step input hashes — renders it identically and cannot
// drift on prefix or casing.
package digest

import (
	"crypto/sha256"
	"encoding/hex"
)

// SHA256 returns the canonical identifier for b: the literal "sha256:" followed
// by the lowercase hex of the SHA-256 over b. It is the one place this form is
// constructed.
func SHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
