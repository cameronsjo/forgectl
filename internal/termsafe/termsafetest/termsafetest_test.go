package termsafetest

import (
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
)

// TestInertFailureCarriesRecoverableBytes pins the property the hex exists for:
// the exact bytes that failed must be recoverable from the failure message
// alone, without trusting that the message survived transport unedited.
//
// It decodes the hex back and compares to the original output rather than
// matching a literal, so a truncated, prefixed, or partially-escaped hex field
// fails here — matching a substring would pass on a prefix.
func TestInertFailureCarriesRecoverableBytes(t *testing.T) {
	// The hostile fixture itself: an ESC, a bidi override, and a CR — every
	// shape whose %q rendering a transport can mangle.
	out := "●  " + Hostile("work") + "  /tmp/w"

	msg := inertFailure("tmux ls", 0x1b, 9, out)

	field, ok := hexField(msg)
	if !ok {
		t.Fatalf("failure message carries no %q field:\n%s", "output hex:", msg)
	}
	decoded, err := hex.DecodeString(field)
	if err != nil {
		t.Fatalf("hex field does not decode (%v); the bytes are not recoverable: %q", err, field)
	}
	if string(decoded) != out {
		t.Errorf("hex did not round-trip to the failing output\n got %q\nwant %q", decoded, out)
	}
}

// TestInertFailureHexIsTransportSafe is the other half. Hex is worth having
// only because it contains nothing a terminal will act on and nothing a
// reader's paste can reinterpret — no ESC, no bidi override, no backslash
// escapes. If the field ever carried a raw byte again it would be back to
// being an artifact nobody can trust.
func TestInertFailureHexIsTransportSafe(t *testing.T) {
	out := "●  " + Hostile("work")

	field, ok := hexField(inertFailure("tmux ls", 0x1b, 9, out))
	if !ok {
		t.Fatal("failure message carries no hex field")
	}
	for i, r := range field {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Errorf("hex field byte %d is %q; only lowercase hex digits survive a transport", i, r)
		}
	}
	if !strings.Contains(field, "1b") {
		t.Errorf("hex field does not contain the ESC that failed: %q", field)
	}
}

// TestInertFailureKeepsTheReadableRendering guards the half a reader uses. The
// hex is for recovery; %q is what makes the message scannable, and dropping it
// in favour of hex alone would trade one unusable message for another.
func TestInertFailureKeepsTheReadableRendering(t *testing.T) {
	out := "●  " + Hostile("work")

	msg := inertFailure("tmux ls", 0x1b, 9, out)
	if !strings.Contains(msg, strconv.Quote(out)) {
		t.Errorf("failure message lost its %%q rendering:\n%s", msg)
	}
	if !strings.Contains(msg, "tmux ls") {
		t.Errorf("failure message lost the label:\n%s", msg)
	}
	if !strings.Contains(msg, "byte 9") {
		t.Errorf("failure message lost the byte offset:\n%s", msg)
	}
}

// hexField extracts the hex payload from a failure message. It is deliberately
// strict about the prefix: the tests above assert on what it returns, so a
// loose match would let them pass against the %q field instead.
func hexField(msg string) (string, bool) {
	const marker = "output hex: "
	i := strings.Index(msg, marker)
	if i < 0 {
		return "", false
	}
	field := msg[i+len(marker):]
	if end := strings.IndexByte(field, '\n'); end >= 0 {
		field = field[:end]
	}
	return field, true
}
