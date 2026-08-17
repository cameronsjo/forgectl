package surface

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
)

// nonceBytes is the rendezvous nonce's width: 256 bits.
const nonceBytes = 32

// ErrInvalidNonce reports a malformed nonce. A *mismatched* nonce is
// deliberately not distinguished from a malformed one at the call site — see
// Nonce.Equal.
var ErrInvalidNonce = errors.New("surface: invalid rendezvous nonce")

// Nonce is the one-use value the outer process generates, puts in the
// bootstrap command a terminal manager types, and then expects back from
// whatever connects to its socket.
//
// It is not a secret that defeats a hostile process running as the same user:
// that process can read our argv and our environment, and the threat model
// says so. What it defeats is *accidental* cross-talk — a stale trampoline from
// a previous launch, a second forgectl racing on the same machine — and it
// makes an unrelated process's connection fail closed rather than receive an
// invocation. Same-user exclusion is the peer-credential check's job, not this.
type Nonce struct{ raw []byte }

// NewNonce draws a fresh nonce.
func NewNonce() (Nonce, error) {
	buf := make([]byte, nonceBytes)
	if _, err := rand.Read(buf); err != nil {
		return Nonce{}, fmt.Errorf("surface: draw rendezvous nonce: %w", err)
	}
	return Nonce{raw: buf}, nil
}

// ParseNonce decodes the hex form carried in a bootstrap command or a hello
// frame. The grammar is exact — 64 lowercase hex characters — because forgectl
// is the only encoder and every other spelling is something else.
func ParseNonce(s string) (Nonce, error) {
	if len(s) != nonceHexLen || !lowerHexString(s) {
		return Nonce{}, ErrInvalidNonce
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return Nonce{}, ErrInvalidNonce
	}
	return Nonce{raw: raw}, nil
}

// String is the encoded form. It is not redacted, and that is deliberate: this
// value has to be rendered to build the bootstrap command line. Containment is
// the caller's job — the bootstrap is wrapped in an opaque exec.Arg before it
// reaches an adapter — and pretending otherwise here would mean a type that
// cannot do the one thing it exists for.
func (n Nonce) String() string { return hex.EncodeToString(n.raw) }

// Valid reports whether this nonce carries a full-width value.
func (n Nonce) Valid() bool { return len(n.raw) == nonceBytes }

// Equal compares in constant time.
//
// This is the comparison the whole handshake turns on, and it is why
// exec.SecretArg.Equal is not used for it: that method is a plain `==`,
// documented as a confirmation oracle, which is right for a test asserting two
// opaque values match and wrong for a value an unauthenticated peer supplies.
// A byte-at-a-time comparison against attacker-chosen input leaks the matching
// prefix through timing, and the peer here gets to choose its input and retry.
//
// The width check is not redundant with ConstantTimeCompare, and getting that
// wrong fails *open*. ConstantTimeCompare returns 0 for mismatched lengths, but
// it returns **1** for two zero-length slices — so two zero-value Nonces
// compare equal, and a peer that sent nothing would authenticate against a
// service that generated nothing. That is precisely the state a bug or an early
// return produces, so validity is checked first and the comparison only ever
// runs on two full-width values.
//
// Checking the width outside constant time is safe because the width is public:
// every real nonce is 32 bytes, so the branch reveals nothing an attacker did
// not already know. The *contents* are what the timing-safe comparison
// protects, and they are compared at equal length every time.
//
// A malformed nonce and a wrong nonce are deliberately not distinguished to the
// caller: two error paths of different cost are a side channel dressed as good
// diagnostics.
func (n Nonce) Equal(other Nonce) bool {
	if !n.Valid() || !other.Valid() {
		return false
	}
	return subtle.ConstantTimeCompare(n.raw, other.raw) == 1
}
