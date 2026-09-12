package sops

// Test plan for path.go and value.go
//
// ParsePath
//   [x] Accepted: one segment, several segments, digits, underscores, hyphens
//   [x] Refused: empty, a leading hyphen, a dotted key name, an empty
//       segment, spaces, shell metacharacters, quotes and brackets, an
//       over-long path, too many segments
//   [x] A refusal names the RULE and never the argument
//
// NormalizeValue
//   [x] Strips exactly one trailing newline (LF and CRLF), and no more
//   [x] Refused: empty, an interior newline, a C0 control byte, DEL,
//       invalid UTF-8, over the size ceiling
//   [x] A refusal never echoes the value
//   [x] Interior whitespace and a tab survive

import (
	"strings"
	"testing"
)

func TestParsePath_Accepted(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"key", []string{"key"}},
		{"block.key", []string{"block", "key"}},
		{"a.b.c.d", []string{"a", "b", "c", "d"}},
		{"agentgateway.llm_key_hermes", []string{"agentgateway", "llm_key_hermes"}},
		{"with-hyphen.also-one", []string{"with-hyphen", "also-one"}},
		{"_leading_underscore", []string{"_leading_underscore"}},
		{"digits123.4th", []string{"digits123", "4th"}},
		{"MixedCase.Key", []string{"MixedCase", "Key"}},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParsePath(c.in)
			if err != nil {
				t.Fatalf("ParsePath(%q): %v", c.in, err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("ParsePath(%q) = %v, want %v", c.in, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("segment %d = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestParsePath_Refused(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"a bare dot", "."},
		{"a trailing dot", "block."},
		{"a leading dot", ".key"},
		{"a doubled dot", "block..key"},
		{"a leading hyphen", "-flag"},
		{"a space", "block.my key"},
		{"a slash", "block/key"},
		{"a shell metacharacter", "block.key;rm"},
		{"a quote", `block."key"`},
		{"a bracket", "block.key[0]"},
		{"a dollar", "block.$key"},
		{"a newline", "block.key\nother"},
		{"too long", strings.Repeat("a", maxPathBytes+1)},
		{"too many segments", strings.TrimSuffix(strings.Repeat("a.", maxPathSegments+2), ".")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParsePath(c.in)
			if err == nil {
				t.Fatalf("ParsePath(%q) = %v, want a refusal", c.in, got)
			}
			// The refusal names the rule. It must never echo the argument:
			// this grammar admits hyphens, so it is WIDER than internal/env's
			// ValidKey and matches more real credential shapes — a token
			// pasted into the path slot by mistake is plausibly a secret.
			//
			// Checked only for inputs long enough to be distinctive. The
			// grammar itself contains "." and "-", so a one-character input
			// would "match" the message trivially and the assertion would be
			// about the wording rather than about a leak.
			if len(c.in) > 3 && strings.Contains(err.Error(), c.in) {
				t.Errorf("error %q echoed the argument", err.Error())
			}
			if !strings.Contains(err.Error(), PathGrammar) {
				t.Errorf("error = %q, want it to state the grammar", err.Error())
			}
		})
	}
}

// TestParsePath_DottedKeyIsUnreachable pins the deliberate limitation rather
// than leaving it implicit: a real key containing a dot cannot be addressed,
// because a dotted string cannot tell {a: {b.c: v}} from {a: {b: {c: v}}}.
// Refusing is honest; guessing is not.
func TestParsePath_DottedKeyIsUnreachable(t *testing.T) {
	if _, err := ParsePath(`block.my\.key`); err == nil {
		t.Error("a backslash-escaped dot was accepted; escaping is deliberately not supported")
	}
}

func TestJoinExtract(t *testing.T) {
	got := JoinExtract([]string{"agentgateway", "llm_key"})
	if want := `["agentgateway"]["llm_key"]`; got != want {
		t.Errorf("JoinExtract = %q, want %q", got, want)
	}
}

func TestNormalizeValue_Accepted(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "secret", "secret"},
		{"one trailing LF stripped", "secret\n", "secret"},
		{"one trailing CRLF stripped", "secret\r\n", "secret"},
		{"interior spaces survive", "a b  c", "a b  c"},
		{"a tab survives", "a\tb", "a\tb"},
		{"leading whitespace survives", "  padded", "  padded"},
		{"a hash survives", "pass#word", "pass#word"},
		{"a quote survives", "it's", "it's"},
		{"unicode survives", "pässwörd-日本", "pässwörd-日本"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NormalizeValue(c.in)
			if err != nil {
				t.Fatalf("NormalizeValue: %v", err)
			}
			if got != c.want {
				t.Errorf("NormalizeValue(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestNormalizeValue_Refused(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "empty value"},
		{"only a newline", "\n", "empty value"},
		// Exactly ONE trailing newline is stripped, never more. A second one
		// is interior once the first is gone, so it refuses — a greedy trim
		// would instead silently alter a value whose real last byte is a
		// newline.
		{"two trailing newlines", "secret\n\n", "single line"},
		{"an interior newline", "two\nlines", "single line"},
		{"an interior CR", "two\rlines", "single line"},
		{"a NUL byte", "before\x00after", "control character"},
		{"an escape byte", "before\x1bafter", "control character"},
		{"a DEL byte", "before\x7fafter", "control character"},
		{"invalid UTF-8", "bad\xff\xfebytes", "valid UTF-8"},
		{"over the ceiling", strings.Repeat("a", maxValueBytes+1), "ceiling"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NormalizeValue(c.in)
			if err == nil {
				t.Fatalf("NormalizeValue accepted %q, want a refusal", c.in)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), c.want)
			}
			if got != "" {
				t.Errorf("NormalizeValue returned %q on a refusal, want the empty string", got)
			}
			// The whole input here is a candidate secret, so no refusal may
			// carry any of it. Checked on a distinctive slice rather than the
			// whole string, since a one-character input would match trivially.
			if len(c.in) > 4 && strings.Contains(err.Error(), c.in[:4]) {
				t.Errorf("error %q echoed part of the value", err.Error())
			}
		})
	}
}

// TestNormalizeValue_ControlByteIsWhatStopsTheLoop records why the control-byte
// refusal is a correctness requirement and not hygiene.
//
// YAML forbids C0 bytes outside tab and LF even inside a single-quoted scalar,
// so a value carrying one produces a document sops cannot parse — and sops
// responds to an unparseable document by re-invoking its editor forever.
// Measured on 3.13.3: 36,851 invocations and 8.4 MB of stderr in three
// minutes, still going when it was killed.
func TestNormalizeValue_ControlByteIsWhatStopsTheLoop(t *testing.T) {
	// The byte is placed in the MIDDLE, so a trailing-newline strip cannot
	// absorb it. A trailing \n is a legitimate artifact of the producing
	// command and is stripped; an interior one is not, and that asymmetry is
	// tested separately above.
	for b := 0; b < 0x20; b++ {
		in := "before" + string(rune(b)) + "after"
		_, err := NormalizeValue(in)
		if b == '\t' {
			if err != nil {
				t.Errorf("tab (0x%02x) was refused: %v — YAML permits it in a scalar", b, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("control byte 0x%02x was accepted, want a refusal", b)
		}
	}
	if _, err := NormalizeValue("before\x7fafter"); err == nil {
		t.Error("DEL (0x7f) was accepted, want a refusal")
	}
}
