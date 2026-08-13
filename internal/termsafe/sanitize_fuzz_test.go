package termsafe

import (
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestIsUnsafeTerminalRuneMatchesUnicodeProperties(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		want := unicode.IsControl(r) || unicode.In(r, unicode.Bidi_Control)
		if got := IsUnsafeTerminalRune(r); got != want {
			t.Fatalf("IsUnsafeTerminalRune(%U) = %t, want %t", r, got, want)
		}
	}
}

func TestSanitizeReplacesBidiControls(t *testing.T) {
	tests := []struct {
		name string
		r    rune
	}{
		{name: "ARABIC LETTER MARK", r: 0x061c},
		{name: "LEFT-TO-RIGHT MARK", r: 0x200e},
		{name: "RIGHT-TO-LEFT MARK", r: 0x200f},
		{name: "LEFT-TO-RIGHT EMBEDDING", r: 0x202a},
		{name: "RIGHT-TO-LEFT EMBEDDING", r: 0x202b},
		{name: "POP DIRECTIONAL FORMATTING", r: 0x202c},
		{name: "LEFT-TO-RIGHT OVERRIDE", r: 0x202d},
		{name: "RIGHT-TO-LEFT OVERRIDE", r: 0x202e},
		{name: "LEFT-TO-RIGHT ISOLATE", r: 0x2066},
		{name: "RIGHT-TO-LEFT ISOLATE", r: 0x2067},
		{name: "FIRST STRONG ISOLATE", r: 0x2068},
		{name: "POP DIRECTIONAL ISOLATE", r: 0x2069},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, want := Sanitize("left"+string(tt.r)+"right"), "left right"; got != want {
				t.Errorf("Sanitize with %U = %q, want %q", tt.r, got, want)
			}
		})
	}
}

func TestSanitizePreservesTabAlthoughItIsUnsafe(t *testing.T) {
	if !IsUnsafeTerminalRune('\t') {
		t.Fatal("tab is a Cc control and must be classified as unsafe")
	}
	if got, want := Sanitize("a\tb"), "a\tb"; got != want {
		t.Errorf("Sanitize(%q) = %q, want %q", want, got, want)
	}
}

func TestSanitizePreservesNonBidiFormattingAndRTLText(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "ZERO WIDTH NON-JOINER", input: "a\u200cb"},
		{name: "ZERO WIDTH JOINER", input: "a\u200db"},
		{name: "Persian shaping with ZWNJ", input: "می\u200cروم"},
		{name: "emoji ZWJ sequence", input: "👩\u200d💻"},
		{name: "text presentation selector", input: "✈\ufe0e"},
		{name: "emoji presentation selector", input: "✈\ufe0f"},
		{name: "supplementary variation selector", input: "a\U000e0100b"},
		{name: "ZERO WIDTH SPACE", input: "a\u200bb"},
		{name: "Arabic text", input: "مرحبا"},
		{name: "Hebrew text", input: "שלום"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Sanitize(tt.input); got != tt.input {
				t.Errorf("Sanitize(%q) = %q, want unchanged", tt.input, got)
			}
		})
	}
}

func FuzzSanitize(f *testing.F) {
	seeds := []string{
		"",
		"plain text",
		"tab\ttab",
		"\x1b[31mred\x1b[0m",
		string(rune(0x9b)),
		string(rune(0x7f)),
		"emoji 🔥 test",
		"hidden" + string(rune(0x202e)) + "spoof" + string(rune(0x202c)),
		"zero\u200bwidth",
		"join\u200der",
		"multi\nline\r\nstring",
		"咖啡 workflow",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := Sanitize(s)
		for _, r := range got {
			if r != '\t' && IsUnsafeTerminalRune(r) {
				t.Fatalf("unsafe terminal rune %U survived Sanitize: input=%q output=%q", r, s, got)
			}
		}

		if utf8.ValidString(s) {
			inputRunes, outputRunes := []rune(s), []rune(got)
			if len(inputRunes) != len(outputRunes) {
				t.Fatalf("Sanitize changed rune count %d -> %d: input=%q output=%q", len(inputRunes), len(outputRunes), s, got)
			}
			for i, inputRune := range inputRunes {
				want := inputRune
				if inputRune != '\t' && IsUnsafeTerminalRune(inputRune) {
					want = ' '
				}
				if outputRunes[i] != want {
					t.Fatalf("Sanitize rune %d = %U, want %U: input=%q output=%q", i, outputRunes[i], want, s, got)
				}
			}
		}

		if twice := Sanitize(got); twice != got {
			t.Fatalf("Sanitize is not idempotent: once=%q twice=%q", got, twice)
		}
	})
}
