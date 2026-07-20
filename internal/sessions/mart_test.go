package sessions

import "testing"

// likeEscape must neutralize the LIKE/ILIKE metacharacters so a `why` fallback
// query matches as a literal substring, not a pattern. A DB-free unit test:
// the integration tests that exercise the query itself skip without a mart DSN.
func TestLikeEscape(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"50%", `50\%`},
		{"foo_bar", `foo\_bar`},
		{`a\b`, `a\\b`},
		{"%_\\", `\%\_\\`},
		{"", ""},
	}
	for _, c := range cases {
		if got := likeEscape(c.in); got != c.want {
			t.Errorf("likeEscape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
