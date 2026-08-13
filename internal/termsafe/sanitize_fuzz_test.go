package termsafe

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
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

func TestTextHandler_SafeFieldsRemainOnePhysicalLine(t *testing.T) {
	var out bytes.Buffer
	handler := slog.NewTextHandler(&out, nil)
	record := slog.NewRecord(time.Unix(0, 0), slog.LevelWarn, "migration refused", 0)
	record.Add("path", QuotePath("/tmp/a\n\x1b[2K\u202e"), "error", SafeLine("bad\r\x9bforged"))
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Count(got, "\n") != 1 || strings.ContainsAny(strings.TrimSuffix(got, "\n"), "\r\x1b") || strings.ContainsRune(got, '\u009b') || strings.ContainsRune(got, '\u202e') {
		t.Fatalf("TextHandler output is not one inert physical line: %q", got)
	}
}

func TestError_PreservesIdentityAndEscapesFilesystemPaths(t *testing.T) {
	sentinel := errors.New("permission\x1b[2Kdenied")
	tests := []struct {
		name string
		err  error
	}{
		{name: "path", err: &os.PathError{Op: "open\nforged", Path: "/tmp/a\nb\x1b[31m", Err: sentinel}},
		{name: "link", err: &os.LinkError{Op: "rename\rforged", Old: "/tmp/old\nline", New: "/tmp/new\u202eexe", Err: sentinel}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Error(tt.err)
			if !errors.Is(got, sentinel) {
				t.Fatalf("errors.Is(%v, sentinel) = false", got)
			}
			var pathErr *os.PathError
			var linkErr *os.LinkError
			if !errors.As(got, &pathErr) && !errors.As(got, &linkErr) {
				t.Fatalf("errors.As(%T) did not preserve filesystem error", tt.err)
			}
			if strings.Count(got.Error(), "\n") != 0 || strings.ContainsAny(got.Error(), "\r\x1b") || strings.ContainsRune(got.Error(), '\u202e') {
				t.Fatalf("safe error contains terminal control/format text: %q", got.Error())
			}
			for _, escaped := range []string{`\n`, `\x1b`} {
				if !strings.Contains(got.Error(), escaped) {
					t.Errorf("safe error %q does not visibly escape %q", got.Error(), escaped)
				}
			}
		})
	}
}

func TestSafeLineAndQuotePath_EscapeLayoutControlsAndBidiOverride(t *testing.T) {
	input := "a\tb\nc\rd\x1be\x7ff\u0085g\u202eh"
	for name, got := range map[string]string{
		"SafeLine":  SafeLine(input),
		"QuotePath": QuotePath(input),
	} {
		if strings.ContainsAny(got, "\t\n\r\x1b\x7f\u0085\u202e") {
			t.Fatalf("%s output %q retained a sink layout control or bidi override", name, got)
		}
		if strings.ContainsAny(got, "\n\r") {
			t.Fatalf("%s output spans physical lines: %q", name, got)
		}
	}
}

func TestSafeLineAndQuotePath_EscapeEverySharedUnsafeRune(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !IsUnsafeTerminalRune(r) {
			continue
		}
		for name, got := range map[string]string{
			"SafeLine":  SafeLine(string(r)),
			"QuotePath": QuotePath(string(r)),
		} {
			if strings.ContainsRune(got, r) {
				t.Fatalf("%s retained unsafe rune %U in %q", name, r, got)
			}
		}
	}
}

func TestVisibleQuotingDoesNotBroadenSharedSanitizeClassifier(t *testing.T) {
	for _, r := range []rune{'\u200c', '\u200d', '\ufe0e', '\ufe0f', '\U000e0100'} {
		input := "left" + string(r) + "right"
		if IsUnsafeTerminalRune(r) {
			t.Fatalf("permitted formatting rune %U was added to the shared classifier", r)
		}
		if got := Sanitize(input); got != input {
			t.Fatalf("Sanitize changed permitted formatting rune %U: %q", r, got)
		}
	}

	for _, r := range []rune{'\u200c', '\u200d'} {
		input := "left" + string(r) + "right"
		for name, got := range map[string]string{
			"SafeLine":  SafeLine(input),
			"QuotePath": QuotePath(input),
		} {
			if strings.ContainsRune(got, r) {
				t.Fatalf("%s did not visibly quote non-graphic rune %U: %q", name, r, got)
			}
		}
	}
}
