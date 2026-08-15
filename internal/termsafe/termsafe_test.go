package termsafe

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
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

func TestSafeLineQuotesEveryBidiControl(t *testing.T) {
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
			assertQuotedInPlace(t, tt.r)
		})
	}
}

// assertQuotedInPlace checks SafeLine's actual contract: the rune is replaced by
// its VISIBLE escape, in place, with the surrounding text intact.
//
// Asserting only that the rune is gone would pass on a SafeLine that DELETED it,
// and deletion is the behaviour the whole boundary exists to avoid — an operator
// who cannot see that something was removed is being lied to just as surely as
// one whose cursor gets moved. The expected form comes from
// strconv.QuoteRuneToGraphic, the same source SafeLine builds from, so this
// pins the spelling rather than restating the implementation's arithmetic.
func assertQuotedInPlace(t *testing.T, r rune) {
	t.Helper()
	quoted := strconv.QuoteRuneToGraphic(r)
	want := "left" + quoted[1:len(quoted)-1] + "right"
	if got := SafeLine("left" + string(r) + "right"); got != want {
		t.Errorf("SafeLine with %U = %q, want %q", r, got, want)
	}
}

// TestSafeLineQuotesTabAndTheInvisibleFormattingResidual is #281's regression at
// the primitive: the retired Sanitize passed tab through by design and passed
// U+2028, U+2029, U+200B, U+00AD and U+2060 through by omission — none of them
// is Cc or Bidi_Control. SafeLine quotes all six because its rule is "unsafe OR
// non-graphic", which is the reason every human sink was moved onto it.
func TestSafeLineQuotesTabAndTheInvisibleFormattingResidual(t *testing.T) {
	for _, r := range []rune{'\t', 0x2028, 0x2029, 0x200b, 0x00ad, 0x2060} {
		assertQuotedInPlace(t, r)
	}
}

// TestSafeLinePreservesOrdinaryGraphicText is the other half of the boundary:
// SafeLine must not turn legitimate non-ASCII text into escapes. RTL script and
// emoji are graphic runes and survive verbatim. Joiners and variation selectors
// are deliberately NOT in this set — they are Cf, and SafeLine quotes them by
// its non-graphic rule, which
// TestVisibleQuotingDoesNotBroadenSharedClassifier pins from the other side.
func TestSafeLinePreservesOrdinaryGraphicText(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "Arabic text", input: "مرحبا"},
		{name: "Hebrew text", input: "שלום"},
		{name: "emoji", input: "emoji 🔥 test"},
		{name: "CJK", input: "咖啡 workflow"},
		{name: "ASCII with spaces", input: "plain text"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SafeLine(tt.input); got != tt.input {
				t.Errorf("SafeLine(%q) = %q, want unchanged", tt.input, got)
			}
		})
	}
}

// FuzzSafeLine holds the property the human boundary exists for: no unsafe and
// no non-graphic rune survives into the output, the result is one physical
// line, and a second pass changes nothing. The corpus carries the seeds the
// retired FuzzSanitize accumulated plus the #281 residual it never covered.
func FuzzSafeLine(f *testing.F) {
	seeds := []string{
		"",
		"plain text",
		"tab\ttab",
		"\x1b[31mred\x1b[0m",
		string(rune(0x9b)),
		string(rune(0x7f)),
		"emoji 🔥 test",
		"hidden" + string(rune(0x202e)) + "spoof" + string(rune(0x202c)),
		"zero" + string(rune(0x200b)) + "width",
		"join" + string(rune(0x200d)) + "er",
		"multi\nline\r\nstring",
		"咖啡 workflow",
		"line" + string(rune(0x2028)) + "sep",
		"para" + string(rune(0x2029)) + "sep",
		"soft" + string(rune(0x00ad)) + "hyphen",
		"word" + string(rune(0x2060)) + "joiner",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := SafeLine(s)
		for _, r := range got {
			if IsUnsafeTerminalRune(r) {
				t.Fatalf("unsafe terminal rune %U survived SafeLine: input=%q output=%q", r, s, got)
			}
			if !unicode.IsGraphic(r) {
				t.Fatalf("non-graphic rune %U survived SafeLine: input=%q output=%q", r, s, got)
			}
		}
		if strings.ContainsAny(got, "\n\r") {
			t.Fatalf("SafeLine output spans physical lines: input=%q output=%q", s, got)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("SafeLine produced invalid UTF-8: input=%q output=%q", s, got)
		}
		if twice := SafeLine(got); twice != got {
			t.Fatalf("SafeLine is not idempotent: once=%q twice=%q", got, twice)
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

// TestVisibleQuotingDoesNotBroadenSharedClassifier guards the one invariant the
// #281 convergence must not trade away. IsUnsafeTerminalRune is shared by the
// text renderer and the JSON filter, and only the text renderer may quote MORE
// than it names — a rune added to the classifier itself would start escaping
// values inside --json documents, where escaping is a contract change.
func TestVisibleQuotingDoesNotBroadenSharedClassifier(t *testing.T) {
	for _, r := range []rune{0x200c, 0x200d, 0xfe0e, 0xfe0f, 0x0e0100, 0x2028, 0x2029, 0x200b, 0x00ad, 0x2060} {
		if IsUnsafeTerminalRune(r) {
			t.Fatalf("permitted formatting rune %U was added to the shared classifier", r)
		}
	}

	for _, r := range []rune{0x200c, 0x200d} {
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
