package tmux

import (
	"reflect"
	"strings"
	"testing"
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
