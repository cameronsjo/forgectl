package termsafe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/perftest"
)

func TestIsUnsafeTerminalRuneMatchesUnicodeProperties(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		want := unicode.IsControl(r) || unicode.In(r, unicode.Bidi_Control)
		if got := IsUnsafeTerminalRune(r); got != want {
			t.Fatalf("IsUnsafeTerminalRune(%U) = %t, want %t", r, got, want)
		}
	}
}

// TestIsInvisibleRune pins the validator classifier #916 added: every Cf, Zl
// and Zp rune, named ones included, and nothing graphic.
func TestIsInvisibleRune(t *testing.T) {
	for _, r := range []rune{0x200b, 0xfeff, 0x2060, 0x00ad, 0x200d, 0xe0001, 0xe0041, 0xe007f, 0x2028, 0x2029} {
		if !IsInvisibleRune(r) {
			t.Errorf("IsInvisibleRune(%U) = false, want true", r)
		}
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		want := unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)
		if got := IsInvisibleRune(r); got != want {
			t.Fatalf("IsInvisibleRune(%U) = %t, want %t", r, got, want)
		}
		if want && unicode.IsGraphic(r) {
			t.Fatalf("IsInvisibleRune(%U) is true for a graphic rune", r)
		}
	}
	for _, r := range "aZ9 é/#-_.中😀" {
		if IsInvisibleRune(r) {
			t.Errorf("IsInvisibleRune(%U) = true, want false for a visible rune", r)
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

// panickingError stands in for Go 1.26.0's os.errSymlink, whose Error method
// is a panic. os.RemoveAll can leak it wrapped in a *PathError (forgectl#783).
type panickingError struct{}

func (panickingError) Error() string { panic("errSymlink is not user-visible") }

func TestError_SurvivesAPanickingErrorMethod(t *testing.T) {
	inner := panickingError{}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "bare", err: inner, want: errTextUnavailable},
		{
			name: "path",
			err:  &os.PathError{Op: "openfdat", Path: "/tmp/run\ndir", Err: inner},
			want: "openfdat " + QuotePath("/tmp/run\ndir") + ": " + errTextUnavailable,
		},
		{
			name: "link",
			err:  &os.LinkError{Op: "rename", Old: "/tmp/a", New: "/tmp/b", Err: inner},
			want: "rename " + QuotePath("/tmp/a") + " " + QuotePath("/tmp/b") + ": " + errTextUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("Error panicked: %v", r)
					}
				}()
				got = Error(tt.err)
			}()
			if got.Error() != tt.want {
				t.Errorf("Error(%s).Error() = %q, want %q", tt.name, got.Error(), tt.want)
			}
			var p panickingError
			if !errors.As(got, &p) {
				t.Errorf("errors.As did not reach the panicking cause; the chain was not preserved")
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

func TestSafeLineMax(t *testing.T) {
	long := strings.Repeat("x", 50)
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"under the cap is SafeLine", "boom\x1b[31m", 40, SafeLine("boom\x1b[31m")},
		{"exactly the cap is not marked", long, 50, long},
		{"over the cap is cut and marked", long, 10, strings.Repeat("x", 10) + TruncatedMarker},
		{"zero means no cap", long, 0, long},
		// An escape is kept or dropped whole: "ab" fits, the 6-rune \u202e does not.
		{"never splits an escape", "ab\u202ecd", 5, "ab" + TruncatedMarker},
		// Controls expand when escaped; the cap counts the expansion.
		{"counts escaped runes", strings.Repeat("\x00", 20), 12, `\x00\x00\x00` + TruncatedMarker},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SafeLineMax(tt.in, tt.max); got != tt.want {
				t.Errorf("SafeLineMax(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
		})
	}
}

func TestSafeLineMaxOutputIsInert(t *testing.T) {
	in := strings.Repeat("a\x1b[2J\u2028\u202e", 100)
	got := SafeLineMax(in, 57)
	for _, r := range got {
		if IsUnsafeTerminalRune(r) || !unicode.IsGraphic(r) {
			t.Fatalf("SafeLineMax output carries unsafe rune %U: %q", r, got)
		}
	}
	body := strings.TrimSuffix(got, TruncatedMarker)
	if n := utf8.RuneCountInString(body); n > 57 {
		t.Errorf("body is %d runes, want <= 57", n)
	}
}

// TestQuoteArgMax_BoundsAndEscapes pins the #562 argv echo form: a hostile
// 10 KB argument becomes a bounded, quoted, control-free echo that marks its
// cut, and a short argument echoes whole with no marker.
func TestQuoteArgMax_BoundsAndEscapes(t *testing.T) {
	hostile := strings.Repeat("\x1b[2J\u202e\n\xff", 2048)
	got := QuoteArgMax(hostile, ArgEchoMaxRunes)
	if len(got) > ArgEchoMaxRunes*10+8 {
		t.Fatalf("len = %d, want bounded by the %d-rune input budget", len(got), ArgEchoMaxRunes)
	}
	if !strings.HasSuffix(got, `"…`) {
		t.Fatalf("cut echo %q does not end in a closing quote then the ellipsis", got)
	}
	for _, r := range got {
		if IsUnsafeTerminalRune(r) || r == utf8.RuneError {
			t.Fatalf("echo %q carries raw rune %U", got, r)
		}
	}

	if got := QuoteArgMax("a\x1bb", ArgEchoMaxRunes); got != `"a\x1bb"` {
		t.Fatalf("short hostile arg = %q, want it escaped with no cut", got)
	}
	if got := QuoteArgMax("owner/repo#x", ArgEchoMaxRunes); got != `"owner/repo#x"` {
		t.Fatalf("short arg = %q, want it echoed whole with no marker", got)
	}
	exact := strings.Repeat("a", ArgEchoMaxRunes)
	if got := QuoteArgMax(exact, ArgEchoMaxRunes); got != strconv.Quote(exact) {
		t.Fatalf("exactly-at-budget arg = %q, want no cut", got)
	}
	if got := QuoteArgMax(exact+"é", ArgEchoMaxRunes); got != strconv.Quote(exact)+"…" {
		t.Fatalf("one-over arg = %q, want the budget then the ellipsis", got)
	}
}

// A typed-nil *os.PathError or *os.LinkError is a non-nil error whose fields
// cannot be read; Error must not panic on it (forgectl#794).
//
// Mutation that turns it red: drop the `!= nil` guard from either branch of
// Error (its row panics reading Op).
func TestError_TypedNilFilesystemErrorDoesNotPanic(t *testing.T) {
	for name, err := range map[string]error{
		"path": (*os.PathError)(nil),
		"link": (*os.LinkError)(nil),
	} {
		t.Run(name, func(t *testing.T) {
			var got error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("Error panicked on a typed-nil %s error: %v", name, r)
					}
				}()
				got = Error(err)
			}()
			if got.Error() != errTextUnavailable {
				t.Errorf("Error(typed-nil %s) = %q, want %q", name, got.Error(), errTextUnavailable)
			}
		})
	}
}

// errorText's recovery leaves a Debug trace naming the Go types involved and
// never the panic value (forgectl#794).
//
// Mutation that turns it red: drop the slog.Debug call (no trace), or log
// the panic value r itself (its text shows).
func TestErrorText_LogsPanicTypeNotValue(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if got := errorText(panickingError{}); got != errTextUnavailable {
		t.Fatalf("errorText = %q", got)
	}
	logged := buf.String()
	if !strings.Contains(logged, "panic_type=string") || !strings.Contains(logged, "termsafe.panickingError") {
		t.Errorf("no type trace in the log:\n%s", logged)
	}
	if strings.Contains(logged, "errSymlink is not user-visible") {
		t.Errorf("the panic value reached the log:\n%s", logged)
	}
}

// TestError_CapsFilesystemPaths is #821: Error escaped each path but echoed
// it at any length. A path over PathEchoMaxRunes is cut in the middle, with
// the ellipsis between the quoted head and tail (#832); one exactly at the
// budget is shown whole.
func TestError_CapsFilesystemPaths(t *testing.T) {
	long := "/" + strings.Repeat("a", PathEchoMaxRunes) + "TAIL"
	atBudget := "/" + strings.Repeat("b", PathEchoMaxRunes-1)
	sentinel := errors.New("denied")
	for name, err := range map[string]error{
		"path":     &os.PathError{Op: "open", Path: long, Err: sentinel},
		"link old": &os.LinkError{Op: "rename", Old: long, New: "/tmp/b", Err: sentinel},
		"link new": &os.LinkError{Op: "rename", Old: "/tmp/a", New: long, Err: sentinel},
	} {
		t.Run(name, func(t *testing.T) {
			got := Error(err).Error()
			if strings.Count(got, "a") > PathEchoMaxRunes {
				t.Fatalf("Error echoed a %d-rune path uncapped: %d bytes", len(long), len(got))
			}
			if !strings.Contains(got, `"`+argEchoEllipsis+`"`) || !strings.Contains(got, `TAIL"`) {
				t.Errorf("Error(%s) = %q, want a middle cut marked by an ellipsis between quotes, the tail kept", name, got)
			}
		})
	}
	got := Error(&os.PathError{Op: "open", Path: atBudget, Err: sentinel}).Error()
	if want := "open " + QuotePath(atBudget) + ": denied"; got != want {
		t.Errorf("a path at the budget was altered: got %d bytes, want %d", len(got), len(want))
	}
}

// TestError_CapsWrappedFilesystemPaths is #837: a *PathError or *LinkError
// wrapped by fmt.Errorf before it reached Error was escaped but never capped,
// because only err itself was reconstructed. Error now finds each over-cap one
// in the chain and renders its span of the message in the capped form, so
// the root error handler caps a wrapped path too.
//
// Mutations: make Error's fallback branch SafeLine(errorText(err)) again and
// every "capped" row echoes the path whole; drop the Unwrap() []error case in
// overlongPathErrors and the join row does; set nextFree[i] to len(text)
// after a match in findSpans and the "twice" row caps only the first
// occurrence.
func TestError_CapsWrappedFilesystemPaths(t *testing.T) {
	long := "/" + strings.Repeat("a", PathEchoMaxRunes) + "TAIL"
	long2 := "/" + strings.Repeat("a", PathEchoMaxRunes) + "OTHER"
	sentinel := errors.New("denied")
	pathErr := &os.PathError{Op: "open", Path: long, Err: sentinel}
	pathErr2 := &os.PathError{Op: "stat", Path: long2, Err: sentinel}
	linkErr := &os.LinkError{Op: "rename", Old: "/tmp/a", New: long, Err: sentinel}
	capped := Error(pathErr).Error()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"capped once wrapped", fmt.Errorf("resolve cwd: %w", pathErr), "resolve cwd: " + capped},
		{"capped twice wrapped", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", pathErr)), "outer: inner: " + capped},
		{"capped link", fmt.Errorf("finalize: %w", linkErr), "finalize: " + Error(linkErr).Error()},
		{"capped join", fmt.Errorf("both: %w", errors.Join(pathErr, pathErr2)), "both: " + capped + `\n` + Error(pathErr2).Error()},
		{"capped twice in the text", fmt.Errorf("%w; again %v", pathErr, pathErr), capped + "; again " + capped},
		{"capped with a hostile wrapper", fmt.Errorf("x\x1b[31m: %w", pathErr), `x\x1b[31m: ` + capped},
		{"short path left as SafeLine renders it", fmt.Errorf("w: %w", &os.PathError{Op: "open", Path: "/tmp/x", Err: sentinel}), "w: open /tmp/x: denied"},
		{"no verbatim span", opaqueWrap{pathErr}, "opaque failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Error(tt.err).Error()
			if got != tt.want {
				t.Fatalf("Error() =\n%q\nwant\n%q", got, tt.want)
			}
			if again := Error(Error(tt.err)).Error(); again != got {
				t.Errorf("Error is not idempotent:\n%q\nthen\n%q", got, again)
			}
			if !errors.Is(Error(tt.err), sentinel) {
				t.Errorf("Error lost the unwrap chain")
			}
		})
	}
}

// opaqueWrap wraps an error without embedding its text, so there is no
// verbatim span for Error to find.
type opaqueWrap struct{ err error }

func (o opaqueWrap) Error() string { return "opaque failure" }
func (o opaqueWrap) Unwrap() error { return o.err }

// TestQuotePathMax_ShortPathAllocatesNoMoreThanQuoteText is #837's fast path:
// a path no longer in bytes than the budget skips the rune-offset slice, so it
// costs exactly QuoteText's allocations.
//
// Mutation: delete the len(path) <= maxRunes fast path and QuotePathMax
// allocates the starts slice on top of QuoteText's.
func TestQuotePathMax_ShortPathAllocatesNoMoreThanQuoteText(t *testing.T) {
	path := "/home/user/src/project/internal/cli/execute.go"
	var sink string
	quote := testing.AllocsPerRun(100, func() { sink = QuoteText(path) })
	capped := testing.AllocsPerRun(100, func() { sink = QuotePathMax(path, 0) })
	_ = sink
	if capped > quote {
		t.Errorf("QuotePathMax allocated %v times for a short path, QuoteText %v", capped, quote)
	}
}

// TestQuotePath_CapsKeepingTheFinalElement is #832 items 1 and 2: QuotePath
// echoed a path at any length, and the cap it lacked kept only the head, so a
// long path lost the filename that identifies it. QuotePath now cuts in the
// middle and keeps the final element whole when it fits.
//
// Mutations: make QuotePath return QuoteText(path) and the "deep" row comes
// back uncapped; make QuotePathMax keep only the head (tail = 0) and every
// cut row loses its tail; drop the final-element branch and the "deep" row
// keeps half a budget of directory instead of exactly the filename.
func TestQuotePath_CapsKeepingTheFinalElement(t *testing.T) {
	dir := "/" + strings.Repeat("d", PathEchoMaxRunes)
	tests := []struct {
		name, path string
		max        int
		want       string
	}{
		{"short path unchanged", "/tmp/a b", 0, `"/tmp/a b"`},
		{"at the budget unchanged", "/abcd", 5, `"/abcd"`},
		{"final element kept whole", "/abcdefghij/name.go", 12, `"/abc"…"/name.go"`},
		{"long final element keeps half the budget", "/" + strings.Repeat("x", 20) + "END", 8, `"/xxx"…"xEND"`},
		{"no separator keeps half the budget", strings.Repeat("y", 20) + "END", 8, `"yyyy"…"yEND"`},
		{"one-rune budget keeps the head only", "/abc", 1, `"/"…`},
		{"trailing separator keeps the directory name", "/abcdefghij/name/", 12, `"/abcde"…"/name/"`},
		{"only separators keeps half the budget", strings.Repeat("/", 20), 8, `"////"…"////"`},
		{"escapes are never split", "/\u202e\u202e\u202e/f", 4, `"/\u202e"…"/f"`},
		{"invalid UTF-8 counts per byte", "/\xff\xfe\xfd\xfc/f", 4, `"/\xff"…"/f"`},
		{"deep", dir + "/name.go", 0, QuoteText(dir[:PathEchoMaxRunes-len("/name.go")]) + "…" + `"/name.go"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			if tt.max == 0 {
				got = QuotePath(tt.path)
			} else {
				got = QuotePathMax(tt.path, tt.max)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestQuotePathIfUnsafe_KeepsALongOrdinaryPathWhole pins the uncapped form
// (#832): QuotePathIfUnsafe's callers print machine-parseable fields, so a
// long but ordinary path has to come back byte-identical.
//
// Mutation: quote through QuotePath in QuotePathIfUnsafe and the long path
// comes back cut and quoted.
func TestQuotePathIfUnsafe_KeepsALongOrdinaryPathWhole(t *testing.T) {
	long := "/" + strings.Repeat("p", 2*PathEchoMaxRunes) + "/name.go"
	if got := QuotePathIfUnsafe(long); got != long {
		t.Errorf("QuotePathIfUnsafe altered a %d-rune ordinary path: %d bytes back", len(long), len(got))
	}
}

// fanOutCycle is an error whose Unwrap() []error returns itself twice beside
// a path error, a fan-out-2 cycle: a depth-only bound walks 2^100 nodes.
type fanOutCycle struct{ pathErr error }

func (f *fanOutCycle) Error() string   { return "cycle: " + f.pathErr.Error() }
func (f *fanOutCycle) Unwrap() []error { return []error{f, f, f.pathErr} }

// TestError_FanOutUnwrapCycleTerminates is #845 item 1: the chain walk has a
// node budget, so a cycle through Unwrap() []error ends, and the path error
// it keeps revisiting is still capped once per span. The walk returns in
// milliseconds; the deadline is a hang backstop only, generous so host load
// cannot trip it (forgectl#879).
//
// Mutation that turns it red: drop `|| budget <= 0` from overlongPathErrors's
// guard, and the walk never returns within the deadline.
func TestError_FanOutUnwrapCycleTerminates(t *testing.T) {
	const deadline = 10 * time.Second
	pathErr := &os.PathError{Op: "open", Path: "/" + strings.Repeat("a", PathEchoMaxRunes) + "TAIL", Err: errors.New("denied")}
	cyclic := &fanOutCycle{pathErr: pathErr}
	done := make(chan string, 1)
	go func() { done <- Error(cyclic).Error() }()
	select {
	case got := <-done:
		if want := "cycle: " + Error(pathErr).Error(); got != want {
			t.Errorf("Error() =\n%q\nwant\n%q", got, want)
		}
	case <-time.After(deadline):
		t.Fatalf("Error did not return within %v on a fan-out-2 Unwrap cycle", deadline)
	}
}

// TestError_JoinOfManyOverlongPathsIsLinear is #845 item 2: an errors.Join of
// 2000 over-cap path errors (a ~1 MB message) took 3.2s when every match
// re-scanned and re-concatenated the rest of the message; one scan and one
// build are linear. The check is a ratio in CPU time (perftest.Linear,
// forgectl#879): the Join of 4000 against the Join of 500. Below about 500 errors the old cost is not yet quadratic,
// which is why the sizes are this large.
//
// Mutation that turns it red: restore the recursive capWrappedPaths from
// before #845 (strings.Index per error, recursing on both sides of each
// match), and eight times the errors costs 60 to 70 times as much.
func TestError_JoinOfManyOverlongPathsIsLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	const n, k = 4000, 8
	_, small := joinOfOverlongPaths(n / k)
	errs, err := joinOfOverlongPaths(n)
	var got string
	perftest.Linear(t, "Error of a Join of over-cap path errors", k,
		func() { _ = Error(small).Error() },
		func() { got = Error(err).Error() })
	for _, i := range []int{0, n / 2, n - 1} {
		if !strings.Contains(got, Error(errs[i]).Error()) {
			t.Errorf("error %d was not capped in the joined message", i)
		}
	}
	if strings.Contains(got, errs[n-1].(*os.PathError).Path) {
		t.Error("a whole over-cap path survived in the joined message")
	}
}

func BenchmarkError_JoinOfOverlongPaths(b *testing.B) {
	_, err := joinOfOverlongPaths(2000)
	for b.Loop() {
		_ = Error(err).Error()
	}
}

func joinOfOverlongPaths(n int) (errs []error, joined error) {
	errs = make([]error, n)
	for i := range errs {
		errs[i] = &os.PathError{Op: "open", Path: "/" + strings.Repeat("a", PathEchoMaxRunes) + strconv.Itoa(i), Err: errors.New("denied")}
	}
	return errs, errors.Join(errs...)
}

// rawCopyWrap renders text but unwraps to err, standing in for a wrapper that
// holds a raw copy of a path rather than its path error's native text.
type rawCopyWrap struct {
	text string
	err  error
}

func (r rawCopyWrap) Error() string { return r.text }
func (r rawCopyWrap) Unwrap() error { return r.err }

// TestError_RawLookAlikeIsCappedOnTheFirstPass is #845 item 3. The path holds
// the literal text `\x1b` and the wrapper a raw ESC in its place. SafeLine
// leaves `\` alone, so the wrapper's escaped text equals the native text. A
// search of the raw message missed it on the first pass and found it on the
// second, so Error(Error(e)) != Error(e) and the first pass left the path
// whole. Matching in escaped space caps it on the first pass.
//
// Mutation that turns it red: restore the pre-#845 capWrappedPaths, which
// searched the raw message.
func TestError_RawLookAlikeIsCappedOnTheFirstPass(t *testing.T) {
	long := "/" + strings.Repeat("a", PathEchoMaxRunes) + `\x1b` + "TAIL"
	pathErr := &os.PathError{Op: "open", Path: long, Err: errors.New("denied")}
	raw := strings.Replace(pathErr.Error(), `\x1b`, "\x1b", 1)
	err := rawCopyWrap{text: "w: " + raw, err: pathErr}

	got := Error(err).Error()
	if want := "w: " + Error(pathErr).Error(); got != want {
		t.Errorf("Error() =\n%q\nwant\n%q", got, want)
	}
	if again := Error(Error(err)).Error(); again != got {
		t.Errorf("Error is not idempotent:\n%q\nthen\n%q", got, again)
	}
}

// TestError_CapsAWrappedPathHoldingAControl pins the other half of matching
// in escaped space: a %w-wrapped path error whose path holds a newline is
// found by its escaped native text, since the message it is searched in is
// escaped too.
//
// Mutation that turns it red: search for the raw native text rather than
// SafeLine(native) in capWrappedPaths.
func TestError_CapsAWrappedPathHoldingAControl(t *testing.T) {
	pathErr := &os.PathError{Op: "open", Path: "/" + strings.Repeat("a", PathEchoMaxRunes) + "\nTAIL", Err: errors.New("denied")}
	if got, want := Error(fmt.Errorf("w: %w", pathErr)).Error(), "w: "+Error(pathErr).Error(); got != want {
		t.Errorf("Error() =\n%q\nwant\n%q", got, want)
	}
}

// TestError_CapsAPathErrorNestedInErr is #845 item 4: the *PathError and
// *LinkError arms printed .Err through its native Error method, so an
// over-cap path error nested there was echoed whole.
//
// Mutation that turns it red: render .Err as SafeLine(errorText(x.Err))
// again in either arm (causeText's non-nil branch).
func TestError_CapsAPathErrorNestedInErr(t *testing.T) {
	long := "/" + strings.Repeat("a", 4*PathEchoMaxRunes) + "TAIL"
	inner := &os.PathError{Op: "lstat", Path: long, Err: errors.New("denied")}
	for name, err := range map[string]error{
		"path": &os.PathError{Op: "open", Path: "/tmp/x", Err: inner},
		"link": &os.LinkError{Op: "rename", Old: "/tmp/a", New: "/tmp/b", Err: inner},
	} {
		t.Run(name, func(t *testing.T) {
			got := Error(err).Error()
			if strings.Contains(got, long) || strings.Count(got, "a") > PathEchoMaxRunes+8 {
				t.Fatalf("Error echoed the nested %d-rune path uncapped: %d bytes", len(long), len(got))
			}
			if !strings.HasSuffix(got, ": "+Error(inner).Error()) {
				t.Errorf("Error(%s) = %q, want the nested error in its capped form", name, got)
			}
			if !errors.Is(Error(err), inner) {
				t.Error("Error lost the unwrap chain")
			}
		})
	}
}

// safeLineReference is SafeLine as it was before the #847 ASCII fast path:
// one safeRune per rune range yields. The fast path must match it exactly.
func safeLineReference(s string) string {
	var safe strings.Builder
	for _, r := range s {
		safe.WriteString(safeRune(r))
	}
	return safe.String()
}

// safeLineEquivalenceSeeds covers each branch of the fast path: all plain
// ASCII, a plain prefix before the first special byte, each ASCII boundary
// (space, tilde, DEL, the C0 controls), multi-byte runes, a valid U+FFFD, and
// invalid and truncated UTF-8.
var safeLineEquivalenceSeeds = []string{
	"",
	"plain text ~ !",
	" ",
	"~",
	"\x7f",
	"\x00\x01\x1f",
	"tab\ttab",
	"prefix\x1b[31mred\x1b[0m",
	"emoji 🔥 test",
	"咖啡 workflow",
	"hidden\u202espoof\u202c",
	"zero\u200bwidth",
	"soft\u00adhyphen",
	"\ufffd valid replacement",
	"bad \xff byte",
	"truncated \xe2\x82",
	"\xc0\xaf overlong",
	"\xed\xa0\x80 surrogate",
	"\u0085 NEL",
	"\U0010ffff max",
}

// TestSafeLineMatchesTheSlowPath is #847 item 3: the ASCII fast path is
// performance only, so it must render every input exactly as the per-rune
// loop did.
//
// Mutation that turns it red: widen isPlainASCII to c <= 0x7f, so DEL is
// copied raw instead of escaped.
func TestSafeLineMatchesTheSlowPath(t *testing.T) {
	for _, s := range safeLineEquivalenceSeeds {
		if got, want := SafeLine(s), safeLineReference(s); got != want {
			t.Errorf("SafeLine(%q) = %q, want %q", s, got, want)
		}
	}
	// All 256 single bytes, and each after a plain prefix.
	for b := range 256 {
		for _, s := range []string{string([]byte{byte(b)}), "ab" + string([]byte{byte(b)}) + "cd"} {
			if got, want := SafeLine(s), safeLineReference(s); got != want {
				t.Errorf("SafeLine(%q) = %q, want %q", s, got, want)
			}
		}
	}
}

// FuzzSafeLineMatchesTheSlowPath fuzzes the #847 equivalence.
func FuzzSafeLineMatchesTheSlowPath(f *testing.F) {
	for _, s := range safeLineEquivalenceSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if got, want := SafeLine(s), safeLineReference(s); got != want {
			t.Fatalf("SafeLine(%q) = %q, want %q", s, got, want)
		}
	})
}

// BenchmarkSafeLine compares SafeLine with the per-rune reference on a 1 MB
// message, the size #847 measured at about 90 ms, in plain ASCII and in text
// that is mostly ASCII with a non-ASCII rune every 64 bytes.
func BenchmarkSafeLine(b *testing.B) {
	ascii := strings.Repeat("open /home/user/src/project/file.go: denied ", 1<<20/44)
	mixed := strings.Repeat(strings.Repeat("a", 61)+"é", 1<<20/63)
	for _, in := range []struct{ name, s string }{{"ascii", ascii}, {"mixed", mixed}} {
		b.Run(in.name+"/fast", func(b *testing.B) {
			b.SetBytes(int64(len(in.s)))
			for b.Loop() {
				_ = SafeLine(in.s)
			}
		})
		b.Run(in.name+"/reference", func(b *testing.B) {
			b.SetBytes(int64(len(in.s)))
			for b.Loop() {
				_ = safeLineReference(in.s)
			}
		})
	}
}

// TestError_LongerContainingSpanWins is #847 item 5: where two path errors'
// spans overlap, the one earlier in chain order used to win, so a container
// wrapping an error that comes first in the chain kept its own over-cap path
// escaped but whole.
//
// Mutation that turns it red: drop the longest-first sort in
// capWrappedPaths, so chain order alone decides an overlap again.
func TestError_LongerContainingSpanWins(t *testing.T) {
	long := "/" + strings.Repeat("a", 4*PathEchoMaxRunes) + "TAIL"
	old := "/" + strings.Repeat("o", 4*PathEchoMaxRunes) + "OLD"
	pe := &os.PathError{Op: "open", Path: long, Err: errors.New("denied")}
	le := &os.LinkError{Op: "link", Old: old, New: "/n", Err: pe}
	for name, err := range map[string]error{
		"join":  errors.Join(pe, le),
		"wrapw": fmt.Errorf("%w; %w", pe, le),
	} {
		t.Run(name, func(t *testing.T) {
			got := Error(err).Error()
			if strings.Contains(got, strings.Repeat("o", PathEchoMaxRunes+1)) {
				t.Fatalf("the container's %d-rune Old path escaped the cap: %d bytes", len(old), len(got))
			}
			if strings.Contains(got, strings.Repeat("a", PathEchoMaxRunes+1)) {
				t.Fatalf("the wrapped %d-rune path escaped the cap: %d bytes", len(long), len(got))
			}
			if !strings.Contains(got, Error(le).Error()) {
				t.Errorf("Error = %q, want the container in its capped form", got)
			}
			if !errors.Is(Error(err), pe) {
				t.Error("Error lost the unwrap chain")
			}
		})
	}
}

// TestError_SelfCyclicPathErrorTerminates is #847 item 8: a *PathError whose
// Err is itself made causeText re-enter Error until the stack overflowed,
// which is fatal. The depth bound stops it at a fixed marker.
//
// Mutation that turns it red (a fatal stack overflow, not a clean failure):
// drop the depth > maxRenderDepth check in errorAt.
func TestError_SelfCyclicPathErrorTerminates(t *testing.T) {
	pe := &os.PathError{Op: "open", Path: "/p", Err: nil}
	pe.Err = pe
	le := &os.LinkError{Op: "rename", Old: "/a", New: "/b", Err: nil}
	le.Err = &os.PathError{Op: "lstat", Path: "/c", Err: le}
	for name, err := range map[string]error{"path": pe, "link": le} {
		t.Run(name, func(t *testing.T) {
			got := Error(err).Error()
			if !strings.HasSuffix(got, errTextTooDeep) {
				t.Fatalf("Error of a self-cycle does not end at the depth marker: %.200q…", got)
			}
			if n := strings.Count(got, `"/`); n < maxRenderDepth || n > 3*maxRenderDepth {
				t.Errorf("Error rendered %d levels, want about maxRenderDepth (%d)", n, maxRenderDepth)
			}
		})
	}
}
