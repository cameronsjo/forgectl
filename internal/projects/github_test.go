package projects

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// repoJSON is one gh repo list record for owner/name.
func repoJSON(owner, name string) string {
	return `[{"isPrivate":false,"name":"` + name + `","sshUrl":"git@github.com:` + owner + `/` + name + `.git"}]`
}

// isRepoList reports whether a recorded call is `gh repo list <org> …`.
func isRepoList(c exec.Call) bool {
	return c.Name == "gh" && len(c.Args) >= 3 && c.Args[0] == "repo" && c.Args[1] == "list"
}

// isLoginDiscovery reports whether a recorded call is the one owner-discovery
// question githubauth asks.
func isLoginDiscovery(c exec.Call) bool {
	return c.Name == "gh" && strings.Join(c.Args, " ") == "api user --jq .login"
}

func TestGithubList_ConfiguredOwnersQueryEachOnceAndPinTheHost(t *testing.T) {
	t.Setenv("GH_HOST", "ghe.example.test")
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if len(args) >= 3 && args[0] == "repo" {
				return repoJSON(args[2], "tool-"+args[2]), nil
			}
			return "", errors.New("unexpected command")
		},
	}

	repos, notes, err := githubList(context.Background(), fake, []string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("githubList: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("notes = %v, want none on a healthy run", notes)
	}
	if len(repos) != 2 || repos[0].Owner != "alpha" || repos[1].Owner != "beta" {
		t.Fatalf("repos = %+v, want one per owner in input order", repos)
	}

	var lists, discoveries int
	for _, c := range fake.Calls {
		switch {
		case isRepoList(c):
			lists++
			if got := c.Env["GH_HOST"]; got != "github.com" {
				t.Errorf("repo list for %q ran with GH_HOST=%q, want github.com", c.Args[2], got)
			}
			if strings.Join(c.Args, " ") != "repo list "+c.Args[2]+" --limit 1000 --json name,sshUrl,isPrivate" {
				t.Errorf("repo list argv = %v", c.Args)
			}
		case isLoginDiscovery(c):
			discoveries++
		}
	}
	if lists != 2 {
		t.Errorf("repo list calls = %d, want 2 (one per unique owner)", lists)
	}
	if discoveries != 0 {
		t.Errorf("discovery calls = %d, want 0 when owners are configured", discoveries)
	}
}

func TestGithubList_AbsentConfigDiscoversLoginOnce(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if strings.Join(args, " ") == "api user --jq .login" {
				return "octocat", nil
			}
			return repoJSON("octocat", "forgectl"), nil
		},
	}

	repos, _, err := githubList(context.Background(), fake, nil)
	if err != nil {
		t.Fatalf("githubList: %v", err)
	}
	if len(repos) != 1 || repos[0].Owner != "octocat" {
		t.Fatalf("repos = %+v, want the discovered login's repos only", repos)
	}

	var lists, discoveries int
	for _, c := range fake.Calls {
		if isRepoList(c) {
			lists++
			if c.Args[2] != "octocat" {
				t.Errorf("listed owner %q, want the discovered login", c.Args[2])
			}
		}
		if isLoginDiscovery(c) {
			discoveries++
		}
	}
	if discoveries != 1 || lists != 1 {
		t.Errorf("discoveries=%d lists=%d, want 1 and 1", discoveries, lists)
	}
}

func TestGithubList_CaseVariantsQueryOnce(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			return repoJSON(args[2], "tool"), nil
		},
	}

	if _, _, err := githubList(context.Background(), fake, []string{"Alpha", "alpha", "ALPHA"}); err != nil {
		t.Fatalf("githubList: %v", err)
	}

	lists := 0
	for _, c := range fake.Calls {
		if isRepoList(c) {
			lists++
		}
	}
	if lists != 1 {
		t.Errorf("repo list calls = %d, want 1 — GitHub logins are case-insensitive", lists)
	}
}

func TestGithubList_MalformedOwnerYieldsZeroQueries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured []string
	}{
		{"flag-like owner", []string{"good", "--limit"}},
		{"traversal owner", []string{"good", ".."}},
		{"over budget owner", []string{"good", strings.Repeat("a", 1024)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &exec.FakeRunner{
				RunFunc: func(name string, args []string) (string, error) {
					return repoJSON("good", "tool"), nil
				},
			}

			repos, _, err := githubList(context.Background(), fake, tc.configured)

			if err == nil {
				t.Fatalf("githubList = %+v, want an error", repos)
			}
			if len(repos) != 0 {
				t.Errorf("repos = %+v, want none — a bad element must not leave earlier owners queried", repos)
			}
			for _, c := range fake.Calls {
				if isRepoList(c) {
					t.Errorf("a repo list ran for %q despite a malformed owner in the list", c.Args[2])
				}
			}
		})
	}
}

func TestGithubList_PartialFailureKeepsHealthyRowsAndNotesCategorically(t *testing.T) {
	hostile := errors.New("gh: \x1b[31mtoken ghp_deadbeef rejected for \x1b[0mbeta")
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if args[2] == "beta" {
				return "", hostile
			}
			// Delay the healthy owner so results complete out of input order —
			// the fold must still be deterministic.
			time.Sleep(20 * time.Millisecond)
			return repoJSON("alpha", "tool"), nil
		},
	}

	repos, notes, err := githubList(context.Background(), fake, []string{"alpha", "beta"})

	if err != nil {
		t.Fatalf("partial failure returned err = %v, want nil with healthy rows preserved", err)
	}
	if len(repos) != 1 || repos[0].Owner != "alpha" {
		t.Fatalf("repos = %+v, want alpha's rows preserved", repos)
	}
	if len(notes) != 1 || notes[0] != "github(beta): query failed" {
		t.Fatalf("notes = %v, want exactly [github(beta): query failed]", notes)
	}
	if strings.Contains(strings.Join(notes, " "), "ghp_deadbeef") {
		t.Errorf("notes leaked raw subprocess output: %v", notes)
	}
}

func TestGithubList_EveryOwnerFailingReturnsSafeAggregate(t *testing.T) {
	hostile := errors.New("gh: \x1b[31mghp_deadbeef\x1b[0m")
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) { return "", hostile },
	}

	repos, notes, err := githubList(context.Background(), fake, []string{"alpha", "beta"})

	if err == nil {
		t.Fatal("all owners failing must return an error")
	}
	if len(repos) != 0 {
		t.Errorf("repos = %+v, want none", repos)
	}
	want := []string{"github(alpha): query failed", "github(beta): query failed"}
	if len(notes) != 2 || notes[0] != want[0] || notes[1] != want[1] {
		t.Fatalf("notes = %v, want %v in owner order", notes, want)
	}
	if strings.Contains(err.Error(), "ghp_deadbeef") || strings.Contains(err.Error(), "\x1b") {
		t.Errorf("aggregate error leaked raw subprocess output: %v", err)
	}
}

func TestGithubList_BadJSONIsACategoricalOwnerFailure(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) { return "not json", nil },
	}

	_, notes, err := githubList(context.Background(), fake, []string{"alpha"})

	if err == nil {
		t.Fatal("a JSON parse failure for the only owner must return an error")
	}
	if len(notes) != 1 || notes[0] != "github(alpha): query failed" {
		t.Fatalf("notes = %v, want the categorical owner note", notes)
	}
}

func TestGithubListOrg_UsesArbitraryLoginAndPinsTheHost(t *testing.T) {
	t.Setenv("GH_HOST", "ghe.example.test")
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			return repoJSON("anthropics", "claude-code"), nil
		},
	}

	repos, err := githubListOrg(context.Background(), fake, "anthropics")
	if err != nil {
		t.Fatalf("githubListOrg: %v", err)
	}
	if len(repos) != 1 || repos[0].Owner != "anthropics" || repos[0].Name != "claude-code" {
		t.Fatalf("repos = %+v, want anthropics/claude-code", repos)
	}

	last := fake.Last()
	if !isRepoList(last) || last.Args[2] != "anthropics" {
		t.Errorf("gh call args = %v, want [repo list anthropics …]", last.Args)
	}
	if got := last.Env["GH_HOST"]; got != "github.com" {
		t.Errorf("GH_HOST = %q, want github.com even under an ambient enterprise host", got)
	}
}

func TestGithubListOrg_RejectsHostileOrgBeforeInvoking(t *testing.T) {
	for _, org := range []string{"--limit", "..", "a/b", "own er", ""} {
		fake := &exec.FakeRunner{}

		if _, err := githubListOrg(context.Background(), fake, org); err == nil {
			t.Errorf("githubListOrg(%q) = nil error, want a rejection", org)
		}
		if len(fake.Calls) != 0 {
			t.Errorf("githubListOrg(%q) spawned %d subprocesses, want 0", org, len(fake.Calls))
		}
	}
}

// TestInventory_KeepsOwnerNotesAndOneAggregateNote covers the plumbing: a
// GitHub host that fails outright must still report WHICH owners failed, on
// top of exactly one host-level note — and the surviving Gitea rows come back
// regardless.
func TestInventory_KeepsOwnerNotesAndOneAggregateNote(t *testing.T) {
	tmp := t.TempDir()
	teaTSV := "owner\tname\ttype\tssh\n" +
		"cameron\thomeclaw\tsource\tssh://git@git.sjo.lol:222/cameron/homeclaw.git\n"
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			switch name {
			case "gh":
				return "", errors.New("gh: not authenticated")
			case "tea":
				return teaTSV, nil
			}
			return "", nil
		},
	}
	c := New(fake, WithGitHubOwners([]string{"alpha", "beta"}))
	c.Dir = tmp

	repos, notes, err := c.Inventory(context.Background())
	if err != nil {
		t.Fatalf("a GitHub outage must not fail the whole inventory: %v", err)
	}
	if len(repos) != 1 || repos[0].Host != "gitea" {
		t.Fatalf("repos = %+v, want the surviving Gitea row", repos)
	}

	aggregates := 0
	for _, n := range notes {
		if strings.HasPrefix(n, "github: ") {
			aggregates++
		}
	}
	want := []string{"github(alpha): query failed", "github(beta): query failed"}
	if len(notes) != 3 || notes[0] != want[0] || notes[1] != want[1] {
		t.Fatalf("notes = %v, want the two owner notes in order plus one aggregate", notes)
	}
	if aggregates != 1 {
		t.Errorf("host-level github notes = %d, want exactly 1", aggregates)
	}
}

func TestWithGitHubOwners_CopiesInput(t *testing.T) {
	owners := []string{"alpha"}
	c := New(&exec.FakeRunner{}, WithGitHubOwners(owners))

	owners[0] = "mutated"

	if len(c.githubOwners) != 1 || c.githubOwners[0] != "alpha" {
		t.Fatalf("client owners = %v, want the value copied at construction", c.githubOwners)
	}
}
