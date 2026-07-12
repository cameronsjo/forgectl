package forgive

import (
	"fmt"
	"testing"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"LS. ", "ls"},
		{"Kill,", "kill"},
		{"  Tree ", "tree"},
		{"ls", "ls"},
		{"PICK!", "pick"},
		{"", ""},
		{"rm;", "rm"},
		{"ls . ", "ls"},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("%q", tc.input), func(t *testing.T) {
			got := Normalize(tc.input)
			if got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// testAliases mirrors the tmux vocabulary (internal/cli's tmuxAliases) — the
// richest real map, kept inline because forgive cannot import cli. The
// resolver semantics under test are map-independent; the cases are the same
// ones the old package-level Canonical carried.
var testAliases = map[string][]string{
	"ls":      {"l", "list", "sessions"},
	"pick":    {"p", "go", "n", "new"},
	"kill":    {"k", "rm", "delete", "x"},
	"rename":  {"mv", "rn"},
	"windows": {"w"},
	"tree":    {"t"},
	"last":    {"-"},
	"cheat":   {"keys"},
}

func TestResolverCanonical(t *testing.T) {
	r := NewResolver(testAliases)
	tests := []struct {
		input     string
		wantCanon string
		wantKnown bool
	}{
		// canonical passthrough
		{"ls", "ls", true},
		// alias resolution
		{"rm", "kill", true},
		{"k", "kill", true},
		{"mv", "rename", true},
		{"-", "last", true},
		{"p", "pick", true},
		// autocorrect-then-alias: normalize strips trailing punct first
		{"RM,", "kill", true},
		{"X.", "kill", true},
		// unknown tokens
		{"frobnicate", "", false},
		{"--bogus", "", false},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("%q", tc.input), func(t *testing.T) {
			gotCanon, gotKnown := r.Canonical(tc.input)
			if gotCanon != tc.wantCanon || gotKnown != tc.wantKnown {
				t.Errorf("Canonical(%q) = (%q, %v), want (%q, %v)",
					tc.input, gotCanon, gotKnown, tc.wantCanon, tc.wantKnown)
			}
		})
	}
}

// TestResolverAllAliasesResolve verifies every alias in a map resolves to its
// canonical key, preventing reverse-lookup drift inside NewResolver.
func TestResolverAllAliasesResolve(t *testing.T) {
	r := NewResolver(testAliases)
	for canonical, aliases := range testAliases {
		for _, alias := range aliases {
			t.Run(fmt.Sprintf("%s->%s", alias, canonical), func(t *testing.T) {
				gotCanon, gotKnown := r.Canonical(alias)
				if !gotKnown {
					t.Errorf("Canonical(%q): expected known=true, got false", alias)
				}
				if gotCanon != canonical {
					t.Errorf("Canonical(%q) = %q, want %q", alias, gotCanon, canonical)
				}
			})
		}
	}
}
