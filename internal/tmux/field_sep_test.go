package tmux

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// render joins fields the way a given tmux hands them back: 3.7b emits the raw
// 0x1f, 3.5a and older octal-escape it. Every parser here has to read both, and
// the point of the tests below is that neither rendering is the "real" one — the
// CI runner ships 3.4 and a Homebrew Mac ships 3.7b, so a parser that handles
// only one is green on exactly one of them.
func render(escaped bool, fields ...string) string {
	if escaped {
		return strings.Join(fields, escapedFieldSep)
	}
	return strings.Join(fields, FieldSep)
}

func renderings(t *testing.T, fn func(t *testing.T, escaped bool)) {
	t.Helper()
	for _, escaped := range []bool{false, true} {
		name := map[bool]string{false: "raw (tmux 3.7b)", true: "octal-escaped (tmux <=3.5a)"}[escaped]
		t.Run(name, func(t *testing.T) { fn(t, escaped) })
	}
}

// TestSplitFieldsPrefersRawSeparator pins the disambiguation rule: an escaping
// tmux never emits a bare 0x1f (it escapes control bytes AND the backslash), so
// a line carrying the raw separator came from a non-escaping tmux, where a
// literal `\037` in a name is four ordinary characters and must stay in place.
func TestSplitFieldsPrefersRawSeparator(t *testing.T) {
	line := "a" + FieldSep + `lit\037eral` + FieldSep + "c"
	want := []string{"a", `lit\037eral`, "c"}
	if got := SplitFields(line); !reflect.DeepEqual(got, want) {
		t.Errorf("SplitFields(%q) = %q, want %q", line, got, want)
	}
}

// underscoreSep is the THIRD rendering, and the one SplitFields deliberately
// cannot read: tmux 3.7b under a non-UTF-8 locale SUBSTITUTES an underscore for
// the 0x1f byte rather than escaping it. Measured on alpine:edge (tmux 3.7b),
// isolated socket, stdout a pipe, format
// `#{window_id}<0x1f>#{window_name}<0x1f>END`:
//
//	LANG=C           -> "@0_win_END"  (od -c: @ 0 _ w i n _ E N D)
//	LANG=C.UTF-8     -> "@0<0x1f>win<0x1f>END"
//	LANG=en_US.UTF-8 -> "@0<0x1f>win<0x1f>END"
//
// tmux 3.5a (alpine:3.21) emits the octal escape in all three locales.
const underscoreSep = "_"

// renderUnderscore joins fields the way tmux 3.7b does under LANG=C.
func renderUnderscore(fields ...string) string {
	return strings.Join(fields, underscoreSep)
}

// SplitFields must leave that rendering alone. The substitution is lossy, and
// `_` is legal in every name tmux prints, so teaching SplitFields this
// separator would split ordinary rows into forged ones — strictly worse than
// the failure it would repair.
func TestSplitFieldsDoesNotSplitOnUnderscore(t *testing.T) {
	line := renderUnderscore("@0", "my_window", "END")
	got := SplitFields(line)
	if len(got) != 1 || got[0] != line {
		t.Fatalf("SplitFields(%q) = %q, want the line intact as a single field", line, got)
	}
}

// TestParsersFailClosedOnUnreadableSeparator is the regression for the
// fail-OPEN that rendering caused. Every row collapses to one field, every
// exact-count check drops it, and before parsedRows the parsers returned an
// empty slice and NO error — so WindowsLive reported ok=true with an all-false
// map (contradicting its own doc comment), LiveReviews counted 0 and the
// concurrency cap granted a full batch on a saturated machine, and
// VerifyDispatched called every healthy dispatch gone. All of it silent.
func TestParsersFailClosedOnUnreadableSeparator(t *testing.T) {
	t.Run("windows", func(t *testing.T) {
		got, err := parseWindows(renderUnderscore("123", "456", "@7", "reviews", "1", "pr-o-r-1", "0", "2"))
		assertUnreadable(t, err, got == nil)
	})
	t.Run("panes", func(t *testing.T) {
		got, err := parsePanes(renderUnderscore("reviews", "1", "0", "title", "claude", "1"))
		assertUnreadable(t, err, got == nil)
	})
	t.Run("sessions", func(t *testing.T) {
		got, err := parseSessions(renderUnderscore("reviews", "2", "1", "1700000000", "/tmp/wt"))
		assertUnreadable(t, err, got == nil)
	})
}

func assertUnreadable(t *testing.T, err error, nilRows bool) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error: a non-empty output that parsed to zero rows must never read as zero live")
	}
	if !errors.Is(err, ErrUnreadableFields) {
		t.Errorf("error = %v, want it to wrap ErrUnreadableFields", err)
	}
	if !nilRows {
		t.Error("rows must be nil when the separator is unreadable, never an empty slice a caller can range over")
	}
}

// Empty output stays a legitimate empty result — a server with no windows is
// not a failure, and turning it into one would refuse every launch on a fresh
// machine.
func TestParsersAcceptEmptyOutput(t *testing.T) {
	if got, err := parseWindows(""); err != nil || len(got) != 0 {
		t.Errorf("parseWindows(%q) = %+v, %v; want no rows and no error", "", got, err)
	}
	if got, err := parsePanes(""); err != nil || len(got) != 0 {
		t.Errorf("parsePanes(%q) = %+v, %v; want no rows and no error", "", got, err)
	}
	if got, err := parseSessions(""); err != nil || len(got) != 0 {
		t.Errorf("parseSessions(%q) = %+v, %v; want no rows and no error", "", got, err)
	}
}

// The contract has to reach the admission gate, not just the parser:
// LiveReviews and WindowsLive both key off ListWindows.
func TestListWindowsPropagatesUnreadableSeparator(t *testing.T) {
	row := renderUnderscore("123", "456", "@7", "reviews", "1", "pr-o-r-1", "0", "2")
	fake := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return row, nil }}
	got, err := New(fake).ListWindows(context.Background())
	if err == nil {
		t.Fatalf("ListWindows = %+v, nil; want an error so LiveReviews reports ok=false", got)
	}
	if !errors.Is(err, ErrUnreadableFields) {
		t.Errorf("error = %v, want it to wrap ErrUnreadableFields", err)
	}
}

// mostRecentSession is the LastSession attach target. An unreadable separator
// there previously returned "" — rendered as "no session to attach to", which
// reads as an empty server rather than a broken read.
func TestMostRecentSessionFailsClosedOnUnreadableSeparator(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) {
		return renderUnderscore("1700000000", "reviews"), nil
	}}
	got, err := New(fake).mostRecentSession(context.Background())
	if err == nil {
		t.Fatalf("mostRecentSession = %q, nil; want an error", got)
	}
	if !errors.Is(err, ErrUnreadableFields) {
		t.Errorf("error = %v, want it to wrap ErrUnreadableFields", err)
	}
}

// parseGenerationIdentity already errored on this input, but its caller wraps
// the cause in "tmux 2.2 or newer is required", which blamed a perfectly modern
// tmux for the operator's locale. Pin that the real cause is identifiable now.
func TestGenerationIdentityNamesTheSeparatorNotTheVersion(t *testing.T) {
	if _, err := parseGenerationIdentity(renderUnderscore("4677", "1786644304", "@1")); !errors.Is(err, ErrUnreadableFields) {
		t.Errorf("error = %v, want it to wrap ErrUnreadableFields", err)
	}
}
