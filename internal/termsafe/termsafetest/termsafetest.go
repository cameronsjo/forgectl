// Package termsafetest holds the terminal-inertness contract that every
// output-boundary test asserts, in one place.
//
// Three packages render text forgectl did not compose — internal/tmux assembles
// the session tree, internal/cli prints the listing tables, internal/tui draws
// the screens — and each needs the same two things: a hostile fixture, and an
// assertion that nothing unsafe reached the output. Spelled once here, they
// cannot drift into three subtly different notions of "safe", which is the
// failure this whole boundary exists to prevent.
//
// A normal package rather than a _test.go file, because a test helper defined
// in one package's test files is invisible to another's.
package termsafetest

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// testReporter is the part of testing.T AssertInert needs. Keeping the seam
// this narrow lets this shared assertion be tested without starting a nested
// Go test whose only success condition is failure.
type testReporter interface {
	Helper()
	Fatal(args ...any)
}

// Hostile decorates a benign label with the three shapes an untrusted name can
// carry into a terminal: a CSI introducer (repaints the screen), a
// right-to-left override (reorders the printed line), and a carriage return
// (overwrites what was already printed).
//
// Every one is spelled as a Go escape rather than written literally. A literal
// U+202E in this file would reorder the SOURCE — the test proving bidi is
// neutralized would be unreadable in the editor that has to review it.
func Hostile(label string) string {
	return label + "\x1b[31m" + "\u202e" + "\r"
}

// AssertInert is the whole-output contract: not one rune of the rendered
// string may be a terminal control, a Unicode bidi format character, or a
// non-graphic rune. Two exceptions, both emitted by forgectl and never
// contributed by a value: newline, which is the renderer's own row structure,
// and the narrow class of SGR sequences its own styles produce (see
// stripOwnSGR).
//
// It reads the ASSEMBLED output rather than asserting that a sanitizer was
// called. That is what makes it survive drift: a column added to a listing, a
// field added to the tree, or a sanitizer call dropped from an existing one all
// fail here without anybody remembering to extend the test.
func AssertInert(t testReporter, label, out string) {
	t.Helper()
	// Invalid UTF-8 first, because the rune scan below cannot see it: ranging a
	// string decodes a bad byte to U+FFFD, and unicode.IsGraphic(U+FFFD) is
	// true, so a lone 0x9B — the 8-bit CSI introducer, which a Latin-1 terminal
	// acts on exactly like ESC[ — would be accepted as an ordinary character.
	if !utf8.ValidString(out) {
		t.Fatal(inertFailure(label, utf8.RuneError, invalidUTF8Offset(out), out))
		return
	}
	// Scan with forgectl's own styling removed, but report against the original
	// so the failure message still carries the exact bytes that shipped.
	scanned, offsets := stripOwnSGR(out)
	for i, r := range scanned {
		if r == '\n' {
			continue
		}
		if termsafe.IsUnsafeTerminalRune(r) || !unicode.IsGraphic(r) {
			t.Fatal(inertFailure(label, r, offsets[i], out))
			return
		}
	}
}

// invalidUTF8Offset returns the byte offset of the first invalid UTF-8
// sequence in s, so the failure points at the offending byte rather than at 0.
func invalidUTF8Offset(s string) int {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			return i
		}
		i += size
	}
	return 0
}

// stripOwnSGR removes the SGR sequences forgectl's own styles emit, and only
// those. It returns the remaining text plus, for each retained byte, its offset
// in the original string, so a failure still points at real bytes.
//
// This exists because Lip Gloss v2 renders colour unconditionally: v1 consulted
// a package-level colour profile that was unset inside a test binary, so styled
// output arrived here as plain text and "reject every ESC" was the same
// assertion as "reject every ESC a VALUE contributed". v2 moved the downgrade
// to the writer, so forgectl's own colours are now present in what the tests
// read.
//
// The line is drawn at APPEARANCE: an SGR sequence setting colour or weight
// cannot move the cursor, erase the screen, reorder a line, or retitle the
// window, so letting those through costs this control none of what it exists
// to catch. Everything else still reaches the scan and still fails — a cursor
// move, an erase, an OSC, a C1 introducer, a bidi override, a bare or
// malformed ESC. Blink and conceal are excluded too: they change what the
// reader can actually see rather than how it looks.
//
// What this does give up is the colour-injection arm, which cannot be kept:
// huh renders its prompts in 256-colour SGR indistinguishable from an injected
// one. That arm is covered at the value boundary instead, by termsafe.SafeLine,
// which escapes a hostile sequence before it is ever styled — visible in
// TestConfirmPromptDrawsNothingUnsafe, where the hostile session name reaches
// the prompt as literal text.
func stripOwnSGR(s string) (string, []int) {
	var b strings.Builder
	offsets := make([]int, 0, len(s))
	for i := 0; i < len(s); {
		if n := ownSGRLen(s[i:]); n > 0 {
			i += n
			continue
		}
		b.WriteByte(s[i])
		offsets = append(offsets, i)
		i++
	}
	return b.String(), offsets
}

// ownSGRLen reports the byte length of a leading SGR sequence forgectl could
// have emitted, or 0 if s does not start with one.
func ownSGRLen(s string) int {
	const prefix = "\x1b["
	if !strings.HasPrefix(s, prefix) {
		return 0
	}
	end := strings.IndexByte(s, 'm')
	if end < 0 {
		return 0
	}
	// Reject anything with a non-parameter byte before the terminator: that is
	// some other CSI (a cursor move, an erase) and must reach the scan.
	params := s[len(prefix):end]
	if strings.TrimLeft(params, "0123456789;") != "" {
		return 0
	}
	if !ownSGRParams(params) {
		return 0
	}
	return end + 1
}

// ownSGRParams reports whether a parameter list sets appearance only.
//
// Accepted: reset and its per-attribute forms, bold/faint/italic/underline,
// reverse video, and every colour selector — basic, bright, and the 38/48
// extended forms. Reverse video is on the list because it is how huh draws the
// block cursor in a text input; it inverts a span rather than hiding it.
//
// Rejected: blink (5, 6) and conceal (8). Conceal makes text unreadable while
// leaving it in the output, which is the one appearance attribute that could
// hide content a reader is being asked to approve.
//
// Unknown parameters FAIL CLOSED — an attribute not named here (underline
// colour 58, say, or a colon-delimited sub-parameter form) is not stripped, so
// its ESC reaches the scan and the assertion fails. That is the safe direction:
// new legitimate styling surfaces as a loud test failure someone widens this
// list for, while an injected sequence is never waved through as "probably
// ours". It is also why this hand-written check is preferred over a general
// CSI decoder — x/ansi would classify a colon form as valid SGR, which is
// exactly the judgement this function must not delegate. lipgloss builds SGR
// through x/ansi's semicolon-only emitters (`38;5;<n>`, `38;2;<r>;<g>;<b>`), so
// the forms below are what the rendering stack can actually produce today.
func ownSGRParams(params string) bool {
	if params == "" { // ESC[m — bare reset, what lipgloss v2 emits
		return true
	}
	fields := strings.Split(params, ";")
	for i := 0; i < len(fields); {
		switch fields[i] {
		case "38", "48": // extended colour: 38;5;N or 38;2;R;G;B
			n, ok := extendedColorLen(fields[i:])
			if !ok {
				return false
			}
			i += n
		default:
			if !appearanceParam(fields[i]) {
				return false
			}
			i++
		}
	}
	return true
}

// extendedColorLen validates a 38/48 selector and returns how many fields it
// consumes. A truncated or out-of-range selector is rejected rather than
// skipped — a malformed sequence is exactly what an injection looks like.
func extendedColorLen(fields []string) (int, bool) {
	if len(fields) < 2 {
		return 0, false
	}
	var want int
	switch fields[1] {
	case "5": // 256-colour: 38;5;N
		want = 3
	case "2": // 24-bit: 38;2;R;G;B
		want = 5
	default:
		return 0, false
	}
	if len(fields) < want {
		return 0, false
	}
	for _, c := range fields[2:want] {
		if n, err := strconv.Atoi(c); err != nil || n < 0 || n > 255 {
			return 0, false
		}
	}
	return want, true
}

// appearanceParam reports whether a single SGR parameter only affects how text
// looks. Blink (5, 6) and conceal (8) are deliberately absent.
func appearanceParam(f string) bool {
	n, err := strconv.Atoi(f)
	if err != nil {
		return false
	}
	switch {
	case n >= 0 && n <= 4, // reset, bold, faint, italic, underline
		n == 7,               // reverse video — huh's text-input cursor
		n >= 21 && n <= 24,   // the matching "off" codes
		n >= 27 && n <= 29,   // reverse/conceal/strike OFF is safe to allow
		n >= 30 && n <= 37,   // basic foreground
		n == 39,              // default foreground
		n >= 40 && n <= 47,   // basic background
		n == 49,              // default background
		n >= 90 && n <= 97,   // bright foreground
		n >= 100 && n <= 107: // bright background
		return true
	}
	return false
}

// inertFailure builds the failure message, and carries the output twice: once
// as %q for a reader, and once as hex.
//
// The hex is the load-bearing half. %q alone produced a failure nobody could
// diagnose: its escapes are themselves text, so a %q line that is pasted,
// forwarded, or rendered by a terminal can arrive transformed — and an
// artifact that cannot be shown to be unedited is not evidence, however
// precise it looks. A real investigation stalled on exactly that, on a %q
// artifact carrying a literal RLO that %q can never emit. Hex has no escapes
// to reinterpret and no characters a terminal will act on, so the bytes that
// failed are recoverable from the failure message alone.
//
// It is a separate function so the message is testable without a failing
// test: the hex is the whole point of this helper, and a change that dropped
// or truncated it would otherwise ship green.
func inertFailure(label string, r rune, i int, out string) string {
	return fmt.Sprintf("%q emitted unsafe rune %q (U+%04X) at byte %d\nfull output: %q\noutput hex: %s",
		label, r, r, i, out, hex.EncodeToString([]byte(out)))
}
