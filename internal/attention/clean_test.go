package attention

import (
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"  two\n\tlines  ", 0, "two lines"},
		{"esc\x1b[31m red", 0, "esc[31m red"},
		{"bidi\u202eflip", 0, "bidiflip"},
		{"zero\u200bwidth", 0, "zerowidth"},
		{"bad\xffutf8", 0, "bad\uFFFDutf8"},
		{"abcdef", 4, "abc…"},
		{"ab   cdef", 4, "ab…"},
	}
	for _, c := range cases {
		if got := Clean(c.in, c.max); got != c.want {
			t.Errorf("Clean(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
	if n := len([]rune(Clean(strings.Repeat("x", 500), MaxText))); n != MaxText {
		t.Errorf("cut to %d runes, want %d", n, MaxText)
	}
}
