package termsafetest

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

type recordingReporter struct {
	helper   bool
	failures []string
}

func (r *recordingReporter) Helper() { r.helper = true }

func (r *recordingReporter) Fatal(args ...any) {
	r.failures = append(r.failures, fmt.Sprint(args...))
}

func TestAssertInertClassifiesEveryHostileRuneClass(t *testing.T) {
	tests := []struct {
		name string
		r    rune
	}{
		{name: "C0 NUL", r: 0x00},
		{name: "C0 BEL", r: 0x07},
		{name: "C0 ESC", r: 0x1b},
		{name: "C0 CR", r: 0x0d},
		{name: "DEL", r: 0x7f},
		{name: "C1 start", r: 0x80},
		{name: "C1 CSI", r: 0x9b},
		{name: "C1 OSC", r: 0x9d},
		{name: "C1 end", r: 0x9f},
		{name: "left-to-right embedding", r: 0x202a},
		{name: "right-to-left embedding", r: 0x202b},
		{name: "pop directional formatting", r: 0x202c},
		{name: "left-to-right override", r: 0x202d},
		{name: "right-to-left override", r: 0x202e},
		{name: "zero width space", r: 0x200b},
		{name: "word joiner", r: 0x2060},
		{name: "soft hyphen", r: 0x00ad},
		{name: "line separator", r: 0x2028},
		{name: "paragraph separator", r: 0x2029},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reporter := &recordingReporter{}
			AssertInert(reporter, tt.name, "before"+string(tt.r)+"after")

			if !reporter.helper {
				t.Error("AssertInert did not mark itself as a test helper")
			}
			if len(reporter.failures) != 1 {
				t.Fatalf("AssertInert recorded %d failures, want exactly 1", len(reporter.failures))
			}
			want := fmt.Sprintf("U+%04X", tt.r)
			if !strings.Contains(reporter.failures[0], want) {
				t.Errorf("failure = %q, want the rejected rune %s", reporter.failures[0], want)
			}
		})
	}
}

func TestAssertInertAcceptsGraphicTextAndNewlines(t *testing.T) {
	reporter := &recordingReporter{}
	AssertInert(reporter, "benign", "plain text\nsecond line ●")

	if !reporter.helper {
		t.Error("AssertInert did not mark itself as a test helper")
	}
	if len(reporter.failures) != 0 {
		t.Errorf("AssertInert rejected benign output: %v", reporter.failures)
	}
}

func TestInertFailureQuotesTheDiagnosticLabel(t *testing.T) {
	label := "sink\x1b[31m\u202e"
	msg := inertFailure(label, 0x1b, 0, "\x1b")
	if strings.Contains(msg, label) {
		t.Fatalf("failure message emitted the label raw: %q", msg)
	}
	if !strings.Contains(msg, strconv.Quote(label)) {
		t.Errorf("failure message did not retain a readable quoted label: %q", msg)
	}
}

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
