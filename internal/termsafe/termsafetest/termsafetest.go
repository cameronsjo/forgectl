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
	"testing"
	"unicode"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

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
// non-graphic rune. Newline is the one exception — it is the renderer's own row
// structure, emitted by forgectl and never contributed by a value.
//
// It reads the ASSEMBLED output rather than asserting that a sanitizer was
// called. That is what makes it survive drift: a column added to a listing, a
// field added to the tree, or a sanitizer call dropped from an existing one all
// fail here without anybody remembering to extend the test.
func AssertInert(t *testing.T, label, out string) {
	t.Helper()
	for i, r := range out {
		if r == '\n' {
			continue
		}
		if termsafe.IsUnsafeTerminalRune(r) || !unicode.IsGraphic(r) {
			t.Fatalf("%s emitted unsafe rune %q at byte %d\nfull output: %q", label, r, i, out)
		}
	}
}
