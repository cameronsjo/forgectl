package pr

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// prSessionKey is the LOGICAL identity of one clean-room review session — the
// authority that every derived label, tmux window name, and digest is computed
// from, and that none of them can reconstruct.
//
// It exists because a *name* was never a safe identity. Before forgectl#218 the
// tmux window name was assembled from a Ref's display parts, so a synthetic
// local session (Owner "local", Repo a 7-char short oid, Number derived from
// that oid's first six hex digits) and a genuine PR under a forge owner
// literally named "local" could spell the identical name — and tmux's
// first-match targeting then pointed `pr attach` and `pr teardown` at whichever
// window it found first. The two are different things; the type system now says
// so. Locality is a KIND on the key, not a spelling in a string, so the two
// cases are not merely unlikely to collide — they are unrepresentable as one
// value.
//
// Its fields are unexported and it is constructed only by localSessionKey,
// remoteSessionKey, and decodeSessionKey, each of which validates. A key is a
// value: copy it freely, compare it with equal (never ==, which would compare
// display spelling rather than canonical identity).
type prSessionKey struct {
	kind sessionKind

	// local kind only.
	algorithm string // an oidAlgorithm* tag, never free text
	oid       string // lowercase hex, exactly the algorithm's width

	// remote kind only.
	owner  string // ASCII-lowercased
	repo   string // ASCII-lowercased
	number uint64 // strictly positive
}

// sessionKind discriminates the two things a review session can be. It is a
// byte because it is part of the canonical encoding below; the wire values are
// frozen and MUST NOT be renumbered.
type sessionKind byte

const (
	kindLocal  sessionKind = 0x01
	kindRemote sessionKind = 0x02
)

// The canonical encoding's frozen header. Changing any of these is a format
// break that decodeSessionKey must be taught to refuse rather than reinterpret.
const (
	sessionKeyMagic   = "FCPR"
	sessionKeyVersion = 0x02
)

// Declared oid widths. The algorithm tag travels inside the canonical bytes, so
// a short oid and a full one are distinct identities rather than a prefix
// relationship — which is what lets the width be widened later without a format
// break.
//
// oidAlgorithmShort is what a Ref actually carries today: newLocalRef stores a
// 7-char abbreviation in Ref.Repo, and Ref is the universal carrier through
// launch, attach, admission, and list. The plan for #218 specified the full
// HEAD oid; persisting it would need a breadcrumb schema field threaded through
// every Ref reconstruction site, and Ref is used as a map key in WindowsLive,
// so a partially-populated field would silently break lookups.
//
// debt: two commits sharing a 7-hex prefix, reviewed concurrently, still share
// one window name — unchanged from before #218, and not the collision #218 is
// about. Upgrade when the breadcrumb grows a persisted head oid: add it as
// oidAlgorithmSHA1/SHA256 here, with no change to the codec.
const (
	oidAlgorithmShort  = "short"
	oidAlgorithmSHA1   = "sha1"
	oidAlgorithmSHA256 = "sha256"
)

// The exact hex width of each declared algorithm, as CONSTANTS. They are
// constants rather than only map entries because localSessionKey's width switch
// cases on them: with constant cases, two algorithms declared at the same width
// are a compile error ("duplicate case"), whereas the map lookups this replaced
// were non-constant expressions — a duplicate compiled cleanly and the switch
// silently took whichever case was written first, quietly filing one algorithm's
// oids under another's tag. The tag travels inside the canonical bytes the
// window-name digest is taken over, so that mislabelling is an identity bug, not
// a cosmetic one. The distinctness the switch's comment asserts is now enforced
// by the compiler rather than stated in prose.
const (
	oidWidthShort  = 7
	oidWidthSHA1   = 40
	oidWidthSHA256 = 64
)

// A second, independent compile-time distinctness check, stated where the widths
// are declared rather than inferred from how they happen to be consumed: an
// array literal cannot carry a duplicate index, so making the widths the indexes
// makes a collision a compile error even if the switch below is ever rewritten
// into a form that no longer cases on constants. The value is discarded, so it
// costs nothing at runtime.
var _ = [...]bool{oidWidthShort: true, oidWidthSHA1: true, oidWidthSHA256: true}

// oidWidths maps each declared algorithm tag to its exact hex width, for the
// decoder — which arrives with the tag in hand and needs the width it implies. A
// partial oid — one whose length matches no declared width — is rejected
// outright rather than padded, truncated, or guessed at.
//
// It is built from the constants above so the two cannot disagree, and
// TestOidWidthsAgreeWithDeclaredConstants holds the map to exactly that set: a
// fourth algorithm added here but not to the constants would be reachable by the
// decoder and unreachable by the constructor, which is a decode-only identity no
// launch could ever produce.
var oidWidths = map[string]int{
	oidAlgorithmShort:  oidWidthShort,
	oidAlgorithmSHA1:   oidWidthSHA1,
	oidAlgorithmSHA256: oidWidthSHA256,
}

// maxPRNumber is the largest PR number a key may carry. parseNumber already
// bounds a parsed ref to an int; this states the same bound where the value
// becomes a uint64 so the widening is obviously lossless.
const maxPRNumber = math.MaxInt

// maxSessionKeyFieldBytes bounds every string in a key. It is load-bearing, not
// hygiene: the canonical encoding prefixes each string with a uint16 length, so
// a 65536-byte owner would wrap that prefix to a short one and two DIFFERENT
// owners could encode to identical bytes — a forged identity rather than a
// malformed one. ValidOwnerRepoPart constrains the charset and says nothing
// about length, so the bound has to live here.
//
// 255 is far above anything real (GitHub caps owners at 39 and repos at 100)
// and far below the wrap point, so it can never be the thing that refuses a
// legitimate repository. TestLocalSessionKeyAcceptsExactlyTheDeclaredWidths
// allocates and exercises every oid width through this bound, so raising it
// also raises that test's work quadratically.
const maxSessionKeyFieldBytes = 255

// checkFieldLength rejects a string too long for the canonical encoding to
// represent unambiguously. field names the component for the error message.
func checkFieldLength(field, value string) error {
	if len(value) > maxSessionKeyFieldBytes {
		return fmt.Errorf("session key %s is %d bytes, over the %d-byte bound", field, len(value), maxSessionKeyFieldBytes)
	}
	return nil
}

// localSessionKey builds the key for a synthetic offline review of a local
// commit. oid is the git object id as the Ref carries it, in any case; it is
// lowercased and must be exactly one declared width of hex.
func localSessionKey(oid string) (prSessionKey, error) {
	lowered := asciiLower(oid)
	// Widths are unique across the declared algorithms, so this is a lookup, not
	// a search — written as a switch rather than a range over oidWidths so the
	// mapping is fixed in the source instead of depending on map order.
	//
	// The cases are the width CONSTANTS, not oidWidths lookups. That is what
	// makes the uniqueness above a compiler-enforced fact: constant cases that
	// collide fail to build, where the map lookups this replaced were
	// non-constant and a collision silently resolved to the first case.
	algorithm := ""
	switch len(lowered) {
	case oidWidthShort:
		algorithm = oidAlgorithmShort
	case oidWidthSHA1:
		algorithm = oidAlgorithmSHA1
	case oidWidthSHA256:
		algorithm = oidAlgorithmSHA256
	}
	if algorithm == "" {
		// The widths are interpolated from the constants rather than spelled out,
		// so widening the set cannot leave the error message lying about it.
		return prSessionKey{}, fmt.Errorf("local session oid %q is %d hex digits, which matches no declared width (%d, %d, or %d)",
			oid, len(lowered), oidWidthShort, oidWidthSHA1, oidWidthSHA256)
	}
	if !isLowerHex(lowered) {
		return prSessionKey{}, fmt.Errorf("local session oid %q is not hexadecimal", oid)
	}
	return prSessionKey{kind: kindLocal, algorithm: algorithm, oid: lowered}, nil
}

// remoteSessionKey builds the key for a real pull request. owner and repo are
// ASCII-lowercased, making mixed-case spellings of one repository a single
// identity — a #218 rule, and deliberately stricter than RefFromParts, which
// preserves the operator's spelling for display.
func remoteSessionKey(owner, repo string, number int) (prSessionKey, error) {
	if !ValidOwnerRepoPart(owner) || !ValidOwnerRepoPart(repo) {
		return prSessionKey{}, fmt.Errorf("session key owner/repo %q/%q outside allowed charset", owner, repo)
	}
	if err := checkFieldLength("owner", owner); err != nil {
		return prSessionKey{}, err
	}
	if err := checkFieldLength("repo", repo); err != nil {
		return prSessionKey{}, err
	}
	if number <= 0 {
		return prSessionKey{}, fmt.Errorf("session key PR number must be positive, got %d", number)
	}
	return prSessionKey{
		kind:   kindRemote,
		owner:  asciiLower(owner),
		repo:   asciiLower(repo),
		number: uint64(number),
	}, nil
}

// sessionKeyForRef derives the logical key for ref. It reads Ref.IsLocal() —
// the unforgeable locality flag — not the owner spelling, so a genuine remote
// ref whose owner is the string "local" takes the remote branch, which is the
// whole point of #218.
func sessionKeyForRef(ref Ref) (prSessionKey, error) {
	if ref.IsLocal() {
		// newLocalRef puts the short oid in Repo; see its doc comment.
		return localSessionKey(ref.Repo)
	}
	return remoteSessionKey(ref.Owner, ref.Repo, ref.Number)
}

// equal reports whether two keys are the same identity. It compares canonical
// bytes rather than fields so there is exactly one definition of equality, and
// it is the same one the digest in a window name is computed over.
//
// NO PRODUCTION CALLER TODAY, deliberately. Production never compares two keys:
// a key is derived from a Ref and immediately rendered to a name, and every
// downstream comparison — ResolveWindowExact, the admission gate — is a
// comparison of NAMES. equal exists so the tests below assert identity the way
// the digest defines it rather than field by field, which is the only definition
// a collision test may use; asserting on fields would pass on a pair that
// encodes, and therefore names, identically. Do not read its presence as a
// production equality path that something forgot to call.
func (k prSessionKey) equal(other prSessionKey) bool {
	return string(k.canonical()) == string(other.canonical())
}

// canonical renders the key's frozen wire form:
//
//	"FCPR" 0x02 kind
//	local:  str(algorithm) str(oid)
//	remote: str(owner) str(repo) uint64be(number)
//
// where str is a big-endian uint16 byte length followed by the bytes. Exactly
// one encoding exists per key, which is what makes a digest over these bytes a
// sound identity: decodeSessionKey(canonical(k)) re-encodes byte-identically.
//
// A zero key encodes to a kind byte of 0x00, which decodeSessionKey refuses —
// an uninitialized key can never masquerade as a real one.
func (k prSessionKey) canonical() []byte {
	buf := make([]byte, 0, 64)
	buf = append(buf, sessionKeyMagic...)
	buf = append(buf, sessionKeyVersion, byte(k.kind))
	switch k.kind {
	case kindLocal:
		buf = appendLenString(buf, k.algorithm)
		buf = appendLenString(buf, k.oid)
	case kindRemote:
		buf = appendLenString(buf, k.owner)
		buf = appendLenString(buf, k.repo)
		buf = binary.BigEndian.AppendUint64(buf, k.number)
	}
	return buf
}

// appendLenString appends s as a big-endian uint16 length plus its bytes.
//
// The bound is enforced HERE, at the narrowing, rather than trusted from the
// constructors. Every current caller passes a field checkFieldLength already
// bounded, so this is unreachable today — but the invariant is exactly the one
// whose violation forges an identity: a 65536-byte owner wraps the uint16
// length prefix to a short one, and two different owners then encode to
// identical canonical bytes that the window-name digest is taken over. Keeping
// the check at the conversion means a future constructor that forgets
// checkFieldLength cannot silently reintroduce the wrap.
//
// It panics rather than returning an error because canonical, and the equal
// and digest that build on it, are total functions on a value type — an
// identity has no failure mode to report. Reaching this is a programming error
// (a constructor that skipped its bound), and the only alternative to stopping
// is emitting bytes that name the wrong session.
// The length is bound to a local so the check and the conversion read the same
// value — which is also the form gosec's range analysis can follow, clearing
// the G115 it reports on the unguarded narrowing.
func appendLenString(buf []byte, s string) []byte {
	n := len(s)
	if n > maxSessionKeyFieldBytes {
		panic(fmt.Sprintf("pr: session key field is %d bytes, over the %d-byte bound; a constructor skipped checkFieldLength", n, maxSessionKeyFieldBytes))
	}
	buf = binary.BigEndian.AppendUint16(buf, uint16(n))
	return append(buf, s...)
}

// decodeSessionKey parses canonical bytes back into a key, STRICTLY: wrong
// magic, unknown version or kind, truncation, trailing bytes, invalid UTF-8, an
// undeclared algorithm, an oid of the wrong width or case, an owner/repo
// outside the charset or not already lowercased, and a zero PR number are all
// refusals. Nothing is normalized on the way in — a value that would re-encode
// differently than it arrived is rejected rather than repaired, so the round
// trip is byte-identical by construction.
//
// NO PRODUCTION CALLER TODAY, deliberately, and worth stating because a strict
// parser sitting beside an identity codec reads as load-bearing. Nothing in
// forgectl parses a session key: keys are built from a Ref and rendered
// forward, never reconstructed. The production caller arrives with forgectl#299
// item 1 — a breadcrumb that persists the key itself rather than re-deriving it
// from a Ref — at which point these bytes become untrusted input from disk and
// every refusal below starts earning its keep.
//
// Until then it is not dead weight but the ROUND-TRIP ORACLE for canonical: the
// property that a decode which succeeds must re-encode byte-identically is what
// pins canonical to exactly one encoding per key, and one encoding per key is
// the premise the window-name digest rests on. TestSessionKeyRoundTripIsByte-
// Identical checks that over constructed keys and FuzzDecodeSessionKey checks it
// over arbitrary bytes, so the surface is exercised as a proof harness even
// though production does not call it.
func decodeSessionKey(b []byte) (prSessionKey, error) {
	rest := b
	if len(rest) < len(sessionKeyMagic)+2 {
		return prSessionKey{}, fmt.Errorf("session key is %d bytes, too short for a header", len(b))
	}
	if string(rest[:len(sessionKeyMagic)]) != sessionKeyMagic {
		return prSessionKey{}, fmt.Errorf("session key has wrong magic")
	}
	rest = rest[len(sessionKeyMagic):]
	if rest[0] != sessionKeyVersion {
		return prSessionKey{}, fmt.Errorf("session key version %#x is not supported", rest[0])
	}
	kind := sessionKind(rest[1])
	rest = rest[2:]

	var key prSessionKey
	var err error
	switch kind {
	case kindLocal:
		key, rest, err = decodeLocalBody(rest)
	case kindRemote:
		key, rest, err = decodeRemoteBody(rest)
	default:
		return prSessionKey{}, fmt.Errorf("session key kind %#x is not supported", byte(kind))
	}
	if err != nil {
		return prSessionKey{}, err
	}
	if len(rest) != 0 {
		return prSessionKey{}, fmt.Errorf("session key has %d trailing bytes", len(rest))
	}
	return key, nil
}

func decodeLocalBody(b []byte) (prSessionKey, []byte, error) {
	algorithm, rest, err := takeLenString(b, "algorithm")
	if err != nil {
		return prSessionKey{}, nil, err
	}
	oid, rest, err := takeLenString(rest, "oid")
	if err != nil {
		return prSessionKey{}, nil, err
	}
	width, declared := oidWidths[algorithm]
	if !declared {
		return prSessionKey{}, nil, fmt.Errorf("session key oid algorithm %q is not declared", algorithm)
	}
	if len(oid) != width {
		return prSessionKey{}, nil, fmt.Errorf("session key %s oid is %d hex digits, want %d", algorithm, len(oid), width)
	}
	if !isLowerHex(oid) {
		return prSessionKey{}, nil, fmt.Errorf("session key oid is not lowercase hexadecimal")
	}
	return prSessionKey{kind: kindLocal, algorithm: algorithm, oid: oid}, rest, nil
}

func decodeRemoteBody(b []byte) (prSessionKey, []byte, error) {
	owner, rest, err := takeLenString(b, "owner")
	if err != nil {
		return prSessionKey{}, nil, err
	}
	repo, rest, err := takeLenString(rest, "repo")
	if err != nil {
		return prSessionKey{}, nil, err
	}
	if !ValidOwnerRepoPart(owner) || !ValidOwnerRepoPart(repo) {
		return prSessionKey{}, nil, fmt.Errorf("session key owner/repo outside allowed charset")
	}
	if owner != asciiLower(owner) || repo != asciiLower(repo) {
		return prSessionKey{}, nil, fmt.Errorf("session key owner/repo is not canonically lowercased")
	}
	if len(rest) < 8 {
		return prSessionKey{}, nil, fmt.Errorf("session key is truncated before its PR number")
	}
	number := binary.BigEndian.Uint64(rest[:8])
	if number == 0 || number > maxPRNumber {
		return prSessionKey{}, nil, fmt.Errorf("session key PR number %d is out of range", number)
	}
	return prSessionKey{kind: kindRemote, owner: owner, repo: repo, number: number}, rest[8:], nil
}

// takeLenString reads one uint16-length-prefixed string, rejecting truncation
// and invalid UTF-8. field names the component for the error message.
func takeLenString(b []byte, field string) (string, []byte, error) {
	if len(b) < 2 {
		return "", nil, fmt.Errorf("session key is truncated before its %s length", field)
	}
	n := int(binary.BigEndian.Uint16(b[:2]))
	b = b[2:]
	if len(b) < n {
		return "", nil, fmt.Errorf("session key %s claims %d bytes but only %d remain", field, n, len(b))
	}
	s := string(b[:n])
	if !utf8.ValidString(s) {
		return "", nil, fmt.Errorf("session key %s is not valid UTF-8", field)
	}
	// The same bound the constructors apply. Without it, decode could mint a key
	// no constructor can build — which would mean two ways to hold one identity,
	// only one of them validated.
	if err := checkFieldLength(field, s); err != nil {
		return "", nil, err
	}
	return s, b[n:], nil
}

// asciiLower lowercases ASCII letters and leaves every other byte alone. It is
// deliberately not strings.ToLower: Unicode case folding is locale-sensitive
// and can change a string's byte length, neither of which belongs in an
// identity that a digest is computed over. The charset that reaches it is
// already ASCII-only.
func asciiLower(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := range len(s) {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

// isLowerHex reports whether s is non-empty and entirely [0-9a-f].
func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
