package termsafe

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestSafeLineMaxJSONFitsItsByteCap is #963's primitive: whatever the input,
// the result encoded by encoding/json takes at most maxJSONBytes bytes between
// the quotes, the marker included, and a cut value keeps its head and ends in
// TruncatedMarker.
//
// Mutations that turn it red: count '<' (or '"', or a 4-byte rune) as one
// byte in jsonStringBytes; drop the marker from the byte budget (return
// safe.String() + TruncatedMarker instead of the fit prefix); ignore
// maxBytes in safeLineCapped.
func TestSafeLineMaxJSONFitsItsByteCap(t *testing.T) {
	inputs := map[string]string{
		"html-escaped":    strings.Repeat("<>&", 3000),
		"four-byte emoji": strings.Repeat("\U0001F600", 3000),
		"controls":        strings.Repeat("\x1b\u202e\u2028\n", 3000),
		"quotes":          strings.Repeat("\"\\", 3000),
		"invalid bytes":   strings.Repeat("\xff", 3000),
		"cjk":             strings.Repeat("\u6f22", 3000),
		"mixed":           strings.Repeat("a<\U0001F600\x1b&\u2028\"\\\xff\u6f22", 1000),
	}
	for _, maxBytes := range []int{16, 17, 100, 1536, 4096} {
		for name, s := range inputs {
			got := SafeLineMaxJSON(s, 0, maxBytes)
			enc, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if n := len(enc) - 2; n > maxBytes {
				t.Errorf("%s at %d: encoded %d bytes, over the cap", name, maxBytes, n)
			}
			if !strings.HasSuffix(got, TruncatedMarker) {
				t.Errorf("%s at %d: = %.40q…; want the truncation marker", name, maxBytes, got)
			}
			if !strings.HasPrefix(SafeLine(s), strings.TrimSuffix(got, TruncatedMarker)) {
				t.Errorf("%s at %d: kept text is not a prefix of SafeLine's output", name, maxBytes)
			}
		}
	}
}

// TestSafeLineMaxJSONMatchesSafeLineMax: with no byte cap it is SafeLineMax,
// and a value under both caps comes back whole with no marker.
//
// Mutation that turns it red: append TruncatedMarker unconditionally at the
// end of safeLineCapped.
func TestSafeLineMaxJSONMatchesSafeLineMax(t *testing.T) {
	for _, s := range []string{"", "plain", "a\u202eb", strings.Repeat("x\x1b", 900)} {
		if got, want := SafeLineMaxJSON(s, 1280, 0), SafeLineMax(s, 1280); got != want {
			t.Errorf("SafeLineMaxJSON(%.20q, 1280, 0) = %.40q, want SafeLineMax's %.40q", s, got, want)
		}
	}
	if got := SafeLineMaxJSON("short \u202e<", 1280, 1536); got != `short \u202e<` {
		t.Errorf("short value = %q; want it escaped and whole", got)
	}
	// The rune cap still binds first on plain ASCII.
	got := SafeLineMaxJSON(strings.Repeat("a", 5000), 1280, 1536)
	if n := utf8.RuneCountInString(strings.TrimSuffix(got, TruncatedMarker)); n != 1280 {
		t.Errorf("kept %d runes of plain ASCII; want the 1280-rune cap", n)
	}
}

// TestSafeLineMaxJSONCutsBetweenWholeEscapes: an escape is kept or dropped
// entire, never split into text that reads as something else.
//
// Mutation that turns it red: cut the output at the byte budget
// (safe.String()[:maxBytes-markerBytes]) instead of at fit.
func TestSafeLineMaxJSONCutsBetweenWholeEscapes(t *testing.T) {
	for budget := 16; budget < 60; budget++ {
		got := strings.TrimSuffix(SafeLineMaxJSON(strings.Repeat("\u202e", 50), 0, budget), TruncatedMarker)
		if len(got)%len(`\u202e`) != 0 || strings.ReplaceAll(got, `\u202e`, "") != "" {
			t.Errorf("budget %d kept %q; want whole \\u202e escapes only", budget, got)
		}
	}
}

// TestJSONStringBytesMatchesEncodingJSON keeps the size model honest: for
// valid UTF-8, and for an invalid byte, it is exactly what encoding/json
// writes.
//
// Mutations that turn it red: count U+2028 as its three UTF-8 bytes; drop
// '\b' or '\f' from the two-byte short-escape arm (encoding/json writes them
// as \b and \f, not as \u0008 and \u000c).
func TestJSONStringBytesMatchesEncodingJSON(t *testing.T) {
	for _, s := range []string{"", "plain", "<>&", "\"\\", "\n\r\t\x00\x1f", "\b", "\f", "a\bb\fc\x0b", "\u2028\u2029", "\u6f22\U0001F600é", "\ufffd", "a\x7fb", "\xff"} {
		enc, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := jsonStringBytes(s), len(enc)-2; got != want {
			t.Errorf("jsonStringBytes(%q) = %d, encoding/json writes %d", s, got, want)
		}
	}
}
