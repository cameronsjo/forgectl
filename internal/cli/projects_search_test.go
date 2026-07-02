package cli

import (
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/projects"
)

// A fork whose local directory name differs from its upstream repo name must
// still be findable by the directory name the user sees on disk — not only by
// the origin-derived repo name.
func TestFilterRepos_MatchesLocalDirName(t *testing.T) {
	repos := []projects.Repo{{
		Host: "github", Owner: "someone", Name: "original",
		Cloned: true, LocalPath: "/Users/x/Projects/my-fork",
	}}
	if got := filterRepos(repos, "", "my-fork"); len(got) != 1 {
		t.Errorf("query by directory name should match the fork; got %d", len(got))
	}
	if got := filterRepos(repos, "", "original"); len(got) != 1 {
		t.Errorf("query by repo name should also match; got %d", len(got))
	}
	if got := filterRepos(repos, "", "nomatch"); len(got) != 0 {
		t.Errorf("non-matching query should return nothing; got %d", len(got))
	}
}

// The list command's aliases (find/l/ls) must resolve so `forgectl projects find`
// runs the list command rather than falling through to the TUI.
func TestProjectsListAliasesResolve(t *testing.T) {
	parent := newProjectsCmd(projects.New(&exec.FakeRunner{}))
	for _, alias := range []string{"find", "l", "ls", "list"} {
		if c := findChild(parent, alias); c == nil || c.Name() != "list" {
			t.Errorf("alias %q did not resolve to the list command", alias)
		}
	}
}
