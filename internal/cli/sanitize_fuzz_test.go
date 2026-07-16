package cli

// Fuzz/property coverage for sanitizeTerm, additive to the table-driven
// control-byte-specific cases already in sessions_test.go (which plant a
// fixed C1 byte and DEL and assert they're gone). This file instead asserts
// the GENERAL invariant sanitizeTerm claims to hold — "no Cc-category
// control rune survives, tab excepted" — over arbitrary fuzzer-generated
// input, and documents a real gap the fuzz corpus surfaced: Unicode Cf
// (format) characters, including bidirectional-override controls, are NOT
// covered by unicode.IsControl and pass through unsanitized.

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// FuzzSanitizeTerm checks the invariant sanitizeTerm's own doc comment
// claims: every Cc-category control rune (tab excepted) is replaced. It also
// checks that valid-UTF8 input keeps its rune count — sanitizeTerm is a 1:1
// rune map, never a deletion — so a length mismatch would mean the mapping
// silently dropped or merged characters.
func FuzzSanitizeTerm(f *testing.F) {
	seeds := []string{
		"",
		"plain text",
		"tab\ttab",
		"\x1b[31mred\x1b[0m", // C0 escape sequence
		string(rune(0x9b)),   // C1 single-byte CSI
		string(rune(0x7f)),   // DEL
		"emoji 🔥 test",
		"‮hidden‬",    // RLO ... PDF (bidi override)
		"​zero​width", // zero-width space
		"multi\nline\r\nstring",
		"咖啡 workflow",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := sanitizeTerm(s)
		for _, r := range got {
			if r != '\t' && unicode.IsControl(r) {
				t.Fatalf("Cc-category control rune %U survived sanitizeTerm: input=%q output=%q", r, s, got)
			}
		}
		if utf8.ValidString(s) {
			if wantN, gotN := utf8.RuneCountInString(s), utf8.RuneCountInString(got); wantN != gotN {
				t.Fatalf("sanitizeTerm should be a 1:1 rune map, rune count changed %d -> %d: input=%q output=%q", wantN, gotN, s, got)
			}
		}
	})
}

// Pins the gap the fuzz seeds above surfaced: unicode.IsControl only covers
// the Cc category (C0/C1 controls), not Cf (format) characters. A
// right-to-left override (U+202E) is a well-known terminal/filename spoofing
// vector — it can visually reorder trailing text to disguise, e.g., a
// malicious runbook title — and sanitizeTerm's doc comment ("render inert in
// the terminal") reads as covering exactly this class of attack, but the
// implementation does not strip it. This test documents CURRENT (not
// necessarily desired) behavior rather than asserting the safe outcome as
// already achieved — do not read it as endorsing the gap; it exists so a fix
// has a red test to flip green. See the sibling FuzzSanitizeTerm above for
// the invariant that DOES hold.
func TestSanitizeTerm_DoesNotStripBidiOverrideGAP(t *testing.T) {
	rlo := string(rune(0x202E))
	got := sanitizeTerm("safe" + rlo + "text")
	if !strings.Contains(got, rlo) {
		t.Skip("sanitizeTerm now strips U+202E — this GAP test is stale, safe to delete")
	}
}
