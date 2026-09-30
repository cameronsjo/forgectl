package organize

import "testing"

func TestCheckGlob(t *testing.T) {
	for _, g := range []string{"*/forge/* :: *", "[a-c]x", "a[b", "", "[!x]"} {
		if err := CheckGlob(g); err != nil {
			t.Errorf("CheckGlob(%q) = %v, want nil", g, err)
		}
	}
	if err := CheckGlob("*/[z-a]*"); err == nil {
		t.Error("CheckGlob accepted a reversed range that can never match")
	}
}

func TestMatch(t *testing.T) {
	tests := []struct {
		name string
		glob string
		s    string
		want bool
	}{
		{"literal equal", "abc", "abc", true},
		{"literal differs", "abc", "abd", false},
		{"star matches slash", "/a/*/c", "/a/b/x/c", true},
		{"star matches empty", "a*", "a", true},
		{"star is anchored at the start", "b*", "ab", false},
		{"star is anchored at the end", "*b", "ba", false},
		{"question matches one char", "a?c", "abc", true},
		{"question needs a char", "a?c", "ac", false},
		{"question matches slash", "a?c", "a/c", true},
		{"class matches", "[a-c]x", "bx", true},
		{"class rejects", "[a-c]x", "dx", false},
		{"negated class rejects member", "[!a]x", "ax", false},
		{"negated class accepts other", "[!a]x", "bx", true},
		{"unclosed bracket is literal", "a[b", "a[b", true},
		{"unclosed bracket matches nothing else", "a[b", "ab", false},
		{"lone bracket at end is literal", "a[", "a[", true},
		{"regexp dot is literal", "a.c", "abc", false},
		{"regexp dot matches a dot", "a.c", "a.c", true},
		{"regexp plus is literal", "a+", "aa", false},
		{"regexp parens are literal", "(a)", "(a)", true},
		{"regexp pipe is literal", "a|b", "a", false},
		{"case sensitive", "ABC", "abc", false},
		{"newline is matched by star", "a*c", "a\nb\nc", true},
		{"leading bracket in class is literal", "[]a]x", "]x", true},
		{"caret in class is literal after negation slot", "[^a]x", "^x", true},
		{"caret in class does not negate", "[^a]x", "bx", false},
		{"backslash in class is literal", `[\]x`, `\x`, true},
		{"posix class syntax is not special", "[[:alpha:]]", "a", false},
		{"empty glob matches empty", "", "", true},
		{"empty glob rejects text", "", "a", false},
		{"reversed range matches nothing and does not panic", "[z-a]", "m", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Match(tt.glob, tt.s); got != tt.want {
				t.Errorf("Match(%q, %q) = %v, want %v", tt.glob, tt.s, got, tt.want)
			}
		})
	}
}
