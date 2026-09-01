package projects

import (
	"strings"
	"testing"
)

func TestResolveWings_EmptyTableMissesEverything(t *testing.T) {
	table, err := ResolveWings("github.com", nil)
	if err != nil {
		t.Fatalf("ResolveWings: %v", err)
	}
	if got := table.For("cameronsjo", "forgectl"); got != "" {
		t.Errorf("empty table returned wing %q; every repo must fall to the host tree", got)
	}
	if got := (WingTable{}).For("cameronsjo", "forgectl"); got != "" {
		t.Errorf("zero-value table returned wing %q", got)
	}
}

// TestResolveWings_FailsClosed covers every config shape that would otherwise
// put two different things at one directory, or make a repo's tree a coin
// flip. Each row is a config an operator could write today.
func TestResolveWings_FailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		host    string
		wings   []Wing
		wantErr string
	}{
		{
			name:    "wing named after the default github host",
			host:    "github.com",
			wings:   []Wing{{Name: "github.com"}},
			wantErr: "named after the configured [github] host",
		},
		{
			name:    "wing named after a configured GHE host",
			host:    "github.example.com",
			wings:   []Wing{{Name: "github.example.com"}},
			wantErr: "named after the configured [github] host",
		},
		{
			name:    "wing name is case-insensitively the host",
			host:    "github.com",
			wings:   []Wing{{Name: "GitHub.com"}},
			wantErr: "named after the configured [github] host",
		},
		{
			name:    "two wings share a name",
			host:    "github.com",
			wings:   []Wing{{Name: "mcp"}, {Name: "mcp"}},
			wantErr: "repeats an earlier wing name",
		},
		{
			name:    "one repo in two wings",
			host:    "github.com",
			wings:   []Wing{{Name: "a", Repos: []string{"o/r"}}, {Name: "b", Repos: []string{"O/R"}}},
			wantErr: "already claimed by wing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveWings(tc.host, tc.wings)
			if err == nil {
				t.Fatal("want an error, got nil — this config must fail closed")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestResolveWings_RejectsUnsafeNames covers what the path-segment charset
// buys over a traversal-only check. A wing name becomes a directory directly
// under the projects root, so ':' (legal on APFS, rendered as '/' in Finder),
// a leading '.' (a tree invisible to ls), whitespace, control characters, and
// non-ASCII homoglyphs must all be refused — not merely traversal.
func TestResolveWings_RejectsUnsafeNames(t *testing.T) {
	for _, name := range []string{
		"", " ", ".", "..", "../escape", "wing/sub", `wing\sub`,
		"-wing", "wing-", "wing:8443", ".hidden", "hidden.",
		// Leading/trailing whitespace is TRIMMED, matching ResolveHost, so the
		// newline case has to be interior to test the charset rather than the trim.
		"wing_underscore", "WING space", "wíng", "wing\x1b[31m", "wi\nng",
		strings.Repeat("a", 254),
	} {
		t.Run("name="+strings.ReplaceAll(name, "\n", `\n`), func(t *testing.T) {
			if _, err := ResolveWings("github.com", []Wing{{Name: name}}); err == nil {
				t.Errorf("wing name %q was accepted; it becomes a directory under the projects root", name)
			}
		})
	}
}

func TestResolveWings_RejectsUnsafeRepos(t *testing.T) {
	for _, repo := range []string{"", "noslash", "a/b/c", "../x", "-flag/repo", "owner/-flag", "/leading", "trailing/"} {
		t.Run("repo="+repo, func(t *testing.T) {
			if _, err := ResolveWings("github.com", []Wing{{Name: "w", Repos: []string{repo}}}); err == nil {
				t.Errorf("wing repo %q was accepted", repo)
			}
		})
	}
}

func TestWingTable_LookupIsCaseInsensitive(t *testing.T) {
	table, err := ResolveWings("github.com", []Wing{
		{Name: "cadence-ecosystem", Repos: []string{"cameronsjo/cadence", "cameronsjo/forgectl"}},
		{Name: "mcp", Repos: []string{"cameronsjo/some-mcp"}},
	})
	if err != nil {
		t.Fatalf("ResolveWings: %v", err)
	}
	for _, tc := range []struct{ owner, name, want string }{
		{"cameronsjo", "cadence", "cadence-ecosystem"},
		{"CameronSjo", "CADENCE", "cadence-ecosystem"},
		{"cameronsjo", "forgectl", "cadence-ecosystem"},
		{"cameronsjo", "some-mcp", "mcp"},
		{"cameronsjo", "unlisted", ""},
		{"other", "cadence", ""},
		{"", "cadence", ""},
		{"cameronsjo", "", ""},
	} {
		if got := table.For(tc.owner, tc.name); got != tc.want {
			t.Errorf("For(%q, %q) = %q, want %q", tc.owner, tc.name, got, tc.want)
		}
	}
	if got := len(table.Names()); got != 2 {
		t.Errorf("Names() returned %d wings, want 2", got)
	}
}

// TestResolveWings_NameIsNormalized pins that a wing's directory name is the
// lowercased form, matching Placement's lowercasing of every other segment —
// the tree must mirror the dedup identity in one spelling.
func TestResolveWings_NameIsNormalized(t *testing.T) {
	table, err := ResolveWings("github.com", []Wing{{Name: "Lord-Huron", Repos: []string{"cameronsjo/x"}}})
	if err != nil {
		t.Fatalf("ResolveWings: %v", err)
	}
	if got := table.For("cameronsjo", "x"); got != "lord-huron" {
		t.Errorf("For = %q, want the lowercased wing name", got)
	}
}

// TestResolveWings_AcceptsTheRealEstateWings is the shape check against live
// disk: every wing name in use today must be configurable.
func TestResolveWings_AcceptsTheRealEstateWings(t *testing.T) {
	names := []string{"artificer", "mcp", "obsidian", "smart-home", "lord-huron", "github.io", "cadence-ecosystem"}
	wings := make([]Wing, 0, len(names))
	for _, n := range names {
		wings = append(wings, Wing{Name: n})
	}
	if _, err := ResolveWings("github.com", wings); err != nil {
		t.Fatalf("the wing names live on disk today must all be configurable: %v", err)
	}
}
