package pr

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// The encoded tmux window name for a review session:
//
//	name   = "pr-v2-" kind "-" label "-" digest
//	kind   = "l" / "r"
//	label  = 1*55( "a".."z" / DIGIT / "-" )
//	digest = 32( "a".."z" / "2".."7" )
//
// The digest is the identity; the label is cosmetic, exists so a human reading
// `tmux list-windows` can tell one review from another, and is never parsed
// back. Nothing reconstructs a session from a name — a name is derived FROM a
// prSessionKey and compared, never the reverse.
//
// The "pr-" head is reviewWindowPrefix, deliberately: forgectl#193's admission
// gate counts live reviews by that prefix, so a disambiguating scheme that left
// the namespace (a "prv2-" head, say) would silently stop counting local
// sessions against the concurrency cap. Versioning goes INSIDE the namespace.
const (
	maxPRSessionNameBytes = 96
	maxSessionLabelBytes  = 55
	sessionNameVersion    = "v2"
	// sessionDigestBytes is 160 bits of SHA-256, which base32-encodes to exactly
	// 32 characters with no padding. Truncating a hash is sound here: the digest
	// separates a bounded set of concurrently live sessions, and 160 bits leaves
	// no realistic collision even under deliberate search.
	sessionDigestBytes = 20
	sessionDigestChars = 32
)

// The bound is exactly the grammar's maximum, which this states as an
// assertion rather than a coincidence: 6 + 1 + 1 + 55 + 1 + 32 = 96.
const _ = uint(maxPRSessionNameBytes - (len(reviewWindowPrefix) + len(sessionNameVersion) + 1 + 1 + 1 + maxSessionLabelBytes + 1 + sessionDigestChars))

// nameRole distinguishes the windows a single session may own. It is mixed
// into the digest rather than appended to the label, so a role can never be
// lost to label truncation and the two windows are distinct identities rather
// than one name with a suffix.
//
// The wire values are frozen — renumbering one silently renames every existing
// window of that role.
type nameRole byte

const (
	// roleReview is the window the review agent runs in.
	roleReview nameRole = 0x01
	// roleShell is the operator's own way into the clean room (`pr open`),
	// which must never be mistaken for the review itself.
	roleShell nameRole = 0x02
)

// sessionDigestEncoding is lowercase RFC 4648 base32 without padding: the
// standard alphabet folded to lowercase, so the whole name lives in one
// case-insensitive charset and carries no "=" (a tmux exact-match sigil).
var sessionDigestEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// reSessionName is the grammar, enforced on the way out. Every generated name
// is checked against it before it is returned, so a bug in the builder fails
// loudly here rather than reaching tmux as a malformed target.
var reSessionName = regexp.MustCompile(`^pr-v2-[lr]-[a-z0-9-]{1,55}-[a-z2-7]{32}$`)

// encodedName renders the tmux window name for this key in the given role.
// label is free-form display text — a ref's own spelling, typically — and is
// sanitized to the label grammar; it must be valid UTF-8, which is the one
// thing sanitizing cannot repair without inventing bytes.
func (k prSessionKey) encodedName(label string, role nameRole) (string, error) {
	var kindLetter string
	switch k.kind {
	case kindLocal:
		kindLetter = "l"
	case kindRemote:
		kindLetter = "r"
	default:
		return "", fmt.Errorf("cannot name a session of kind %#x", byte(k.kind))
	}
	if !utf8.ValidString(label) {
		return "", fmt.Errorf("session label is not valid UTF-8")
	}
	switch role {
	case roleReview, roleShell:
	default:
		return "", fmt.Errorf("unknown session window role %#x", byte(role))
	}

	name := reviewWindowPrefix + sessionNameVersion + "-" + kindLetter + "-" +
		sanitizeSessionLabel(label) + "-" + k.digest(role)

	// Belt and braces: the pieces above are individually constrained, but this
	// string becomes a tmux operand, so the whole is verified as a whole.
	if len(name) > maxPRSessionNameBytes {
		return "", fmt.Errorf("session window name is %d bytes, over the %d bound", len(name), maxPRSessionNameBytes)
	}
	if !reSessionName.MatchString(name) {
		return "", fmt.Errorf("session window name %q is outside the generated grammar", name)
	}
	return name, nil
}

// digest is 160 bits of SHA-256 over the key's canonical bytes plus the role,
// base32-lowercased. The role is a suffix byte on the hash input rather than a
// field in the key: the key is the session's identity and must not vary by
// which of its windows is being named.
func (k prSessionKey) digest(role nameRole) string {
	sum := sha256.Sum256(append(k.canonical(), byte(role)))
	return sessionDigestEncoding.EncodeToString(sum[:sessionDigestBytes])
}

// sanitizeSessionLabel folds arbitrary display text into the label grammar. It
// is total: every input, including empty, invalid-UTF-8 bytes, and pure
// punctuation, yields a legal 1..55-byte label.
//
// TERMINAL SAFETY: a generated name DOES reach human sinks — `pr attach`'s error
// text quotes it, and slog carries it — so the question of control and bidi
// characters is live, not hypothetical. It is answered by construction rather
// than by termsafe.SafeLine at each sink: the label is built from an ALLOWLIST
// of [a-z0-9-] and the digest from base32's [a-z2-7], so a control byte, an
// ANSI escape, a NUL, or a bidi override (U+202E and friends) cannot survive
// into the output — every one of them is non-alphanumeric and collapses to a
// hyphen here. That is strictly stronger than sanitizing at the sink, which
// strips what it knows to look for; this cannot emit what it cannot spell.
// Callers therefore need no termsafe wrapper on a name from this codec, and
// that is the ONLY reason they are exempt — a future change that widens the
// label alphabet takes the exemption with it (see forgectl#281, #291, #295 for
// how often this class drifts).
//
// The guarantee is construction by allowlist AND VERIFICATION BY ALLOWLIST, and
// both halves are load-bearing. reSessionName in encodedName above is an
// independent second gate: it matches the WHOLE assembled string against
// ^pr-v2-[lr]-[a-z0-9-]{1,55}-[a-z2-7]{32}$ before any name is returned. Replace
// this function with a passthrough and no hostile name escapes — encodedName
// returns an error instead, and even that error escapes the bytes with %q rather
// than emitting them. Neither gate alone is as strong: without sanitizing, an
// ordinary repo name like "foo.bar" would fail a launch outright; without the
// grammar check, a sanitizer bug would leak straight to a tmux operand and a
// human sink. Do not retire the grammar check as redundant.
//
// ASCII letters lowercase, digits survive, and every other rune — punctuation,
// whitespace, control characters, anything non-ASCII, and the U+FFFD that
// invalid bytes decode to — collapses into a single hyphen. That deliberately
// loses information: the label is not an identity and does not have to be
// reversible, and mapping rather than rejecting is what keeps a legal repo
// name like "foo.bar" from failing a review launch outright.
func sanitizeSessionLabel(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pendingSeparator := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			// fall through to the write below
		case r >= 'A' && r <= 'Z':
			r += 'a' - 'A'
		default:
			// One hyphen per run of anything else, and never a leading one.
			pendingSeparator = b.Len() > 0
			continue
		}
		if pendingSeparator {
			b.WriteByte('-')
			pendingSeparator = false
		}
		b.WriteRune(r)
	}

	label := b.String()
	if len(label) > maxSessionLabelBytes {
		// Every retained rune is one ASCII byte, so a byte cut is a rune cut.
		label = label[:maxSessionLabelBytes]
	}
	label = strings.TrimRight(label, "-")
	if label == "" {
		// The grammar has no empty label, and a session whose display text was
		// entirely punctuation still needs a name. "x" is a placeholder, not an
		// identity — the digest is doing the work.
		return "x"
	}
	return label
}

// label is the display text a key contributes to its window name, rendered from
// the KEY's own fields rather than from the Ref that produced it. That is what
// makes a name a pure function of its key, as this file's head comment claims:
// a local Ref carries an Owner and a Number that the key does not (both are
// derived from the oid), so labelling from Ref.String() would let one key wear
// two names. Two distinct keys can perfectly well render the same label; only
// the digest has to separate them.
//
// An unknown kind renders empty, which sanitizeSessionLabel turns into the "x"
// placeholder — encodedName rejects the key on its own kind switch before any
// such name is returned.
func (k prSessionKey) label() string {
	switch k.kind {
	case kindLocal:
		return localOwnerSentinel + "-" + k.oid
	case kindRemote:
		return fmt.Sprintf("%s/%s#%d", k.owner, k.repo, k.number)
	}
	return ""
}
