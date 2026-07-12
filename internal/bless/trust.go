package bless

import (
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// Store is the parsed trust store: the set of enrolled machine public keys plus
// the id of the anchor key that signs the store itself. The store is a blessed
// file — its adjacent .blessing sidecar is signed specifically by the anchor key
// (DomainTrust), so an agent edit invalidates it and the verify path fails
// closed.
//
//	schema        = 1
//	anchor_key_id = "sha256:..."
//
//	[[key]]
//	key_id   = "sha256:..."
//	machine  = "sjomba"
//	pubkey   = "<base64 std PKIX DER>"
//	added_at = "RFC3339 string"
type Store struct {
	Schema      int          `toml:"schema"`
	AnchorKeyID string       `toml:"anchor_key_id"`
	Keys        []TrustedKey `toml:"key"`
}

// TrustedKey is one enrolled machine key. Pubkey is base64-std of the PKIX DER;
// KeyID is its fingerprint (Fingerprint over that DER), recorded so Lookup is a
// map-style match without re-hashing on every verify.
type TrustedKey struct {
	KeyID   string `toml:"key_id"`
	Machine string `toml:"machine"`
	Pubkey  string `toml:"pubkey"`
	AddedAt string `toml:"added_at"`
}

// StoreSchema is the only trust-store schema version this build understands.
const StoreSchema = 1

// EncodeStore serialises a Store to trust-store TOML bytes.
func EncodeStore(s Store) ([]byte, error) {
	var b strings.Builder
	if err := toml.NewEncoder(&b).Encode(s); err != nil {
		return nil, fmt.Errorf("encode trust store: %w", err)
	}
	return []byte(b.String()), nil
}

// DecodeStore parses trust-store TOML into a Store. The decode is STRICT —
// mirroring workflow.Parse and DecodeEnvelope, an unknown key is rejected rather
// than dropped, and the schema is gated so a forward-versioned store is refused.
// Callers on the verify path MUST authenticate the raw store bytes (trust-domain
// signature by the anchor key) BEFORE calling this — authenticate-before-parse.
func DecodeStore(data []byte) (Store, error) {
	var s Store
	md, err := toml.Decode(string(data), &s)
	if err != nil {
		return Store{}, fmt.Errorf("decode trust store: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return Store{}, fmt.Errorf("decode trust store: unknown key(s) %s", joinKeys(undecoded))
	}
	if s.Schema != StoreSchema {
		return Store{}, fmt.Errorf("decode trust store: unsupported schema %d (want %d)", s.Schema, StoreSchema)
	}
	return s, nil
}

// Lookup returns the enrolled key with the given id, and whether it was found.
func (s Store) Lookup(keyID string) (TrustedKey, bool) {
	for _, k := range s.Keys {
		if k.KeyID == keyID {
			return k, true
		}
	}
	return TrustedKey{}, false
}

// AnchorCheckFunc validates the anchor file's ownership and permissions. It is
// an injectable seam so same-package tests can simulate a non-root or
// group/world-writable anchor without a real root-owned /etc file. A nil error
// means the anchor sits at a location an agent cannot have written.
type AnchorCheckFunc func(path string) error

// checkAnchorOwnership is the production AnchorCheckFunc. It requires the anchor
// to be a regular file, owned by uid 0 (root), with no group- or world-write
// bit. The uid read is platform-specific (statOwnerUID) and fails closed on any
// platform where ownership can't be determined.
func checkAnchorOwnership(path string) error {
	// Lstat, not Stat: a symlink at the anchor path must be refused outright
	// rather than followed to wherever it points. Reaching /etc/forgectl to
	// plant one already requires root (out of scope), but the check everything
	// roots on should not depend on that reasoning.
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("anchor %s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("anchor %s is group- or world-writable (mode %#o)", path, perm)
	}
	uid, err := statOwnerUID(info)
	if err != nil {
		return err
	}
	if uid != 0 {
		return fmt.Errorf("anchor %s is owned by uid %d, want 0 (root)", path, uid)
	}
	return nil
}
