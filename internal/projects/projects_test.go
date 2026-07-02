package projects

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// mkGitDir creates base/name with a .git marker so Discover treats it as a repo.
func mkGitDir(t *testing.T, base, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(base, name, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// inventoryRunFunc fakes the git/gh/tea calls Inventory makes for a fixture with
// two cloned repos (forgectl→github, homeclaw→gitea) plus remote-only repos.
// The gh list also carries a *homeclaw* so the cross-host case is exercised:
// github/homeclaw stays uncloned while gitea/homeclaw (the local origin) is the
// cloned one.
func inventoryRunFunc(tmp string) func(string, []string) (string, error) {
	origins := map[string]string{
		filepath.Join(tmp, "forgectl"): "git@github.com:cameronsjo/forgectl.git",
		filepath.Join(tmp, "homeclaw"): "ssh://git@git.sjo.lol:222/cameron/homeclaw.git",
		// "scratch" has a .git but no origin → local-only.
	}
	ghJSON := `[{"name":"forgectl","sshUrl":"git@github.com:cameronsjo/forgectl.git","isPrivate":false},` +
		`{"name":"homeclaw","sshUrl":"git@github.com:cameronsjo/homeclaw.git","isPrivate":false},` +
		`{"name":"newgh","sshUrl":"git@github.com:cameronsjo/newgh.git","isPrivate":true}]`
	teaTSV := "owner\tname\ttype\tssh\n" +
		"cameron\thomeclaw\tsource\tssh://git@git.sjo.lol:222/cameron/homeclaw.git\n" +
		"cameron\tnewgt\tsource\tssh://git@git.sjo.lol:222/cameron/newgt.git\n"

	return func(name string, args []string) (string, error) {
		switch name {
		case "gh":
			return ghJSON, nil
		case "tea":
			return teaTSV, nil
		case "git":
			if len(args) >= 5 && args[0] == "-C" && args[2] == "remote" && args[3] == "get-url" {
				if u, ok := origins[args[1]]; ok {
					return u, nil
				}
				return "", errors.New("no origin set")
			}
			// status --porcelain / rev-list → clean, 0 ahead.
			return "", nil
		}
		return "", nil
	}
}

func findRepo(repos []Repo, host, name string) (Repo, bool) {
	for _, r := range repos {
		if r.Host == host && r.Name == name {
			return r, true
		}
	}
	return Repo{}, false
}

func TestInventory_MergeDedupCrossHost(t *testing.T) {
	tmp := t.TempDir()
	mkGitDir(t, tmp, "forgectl") // origin → github → dedups with gh list
	mkGitDir(t, tmp, "homeclaw") // origin → gitea  → dedups with tea list
	mkGitDir(t, tmp, "scratch")  // git, no origin  → local-only
	if err := os.Mkdir(filepath.Join(tmp, "notes"), 0o755); err != nil {
		t.Fatal(err) // non-git dir → local-only
	}

	fake := &exec.FakeRunner{RunFunc: inventoryRunFunc(tmp)}
	c := &Client{Dir: tmp, run: fake}

	repos, notes, err := c.Inventory(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("expected no degradation notes when both hosts succeed, got %v", notes)
	}

	// No duplicate keys.
	keys := map[string]int{}
	for _, r := range repos {
		keys[r.Key()]++
	}
	for k, n := range keys {
		if n > 1 {
			t.Errorf("duplicate key %q appears %d times", k, n)
		}
	}

	if len(repos) != 7 {
		t.Fatalf("got %d repos, want 7: %+v", len(repos), repos)
	}

	// Local clones win: cloned, with LocalPath + identity from origin URL.
	if r, ok := findRepo(repos, "github", "forgectl"); !ok || !r.Cloned || r.LocalPath == "" {
		t.Errorf("github/forgectl should be cloned with a local path: %+v (found=%v)", r, ok)
	}
	if r, ok := findRepo(repos, "gitea", "homeclaw"); !ok || !r.Cloned || r.LocalPath == "" {
		t.Errorf("gitea/homeclaw should be cloned with a local path: %+v (found=%v)", r, ok)
	}

	// Cross-host: github/homeclaw is a DISTINCT, uncloned row (not collapsed into
	// the cloned gitea/homeclaw by bare name).
	if r, ok := findRepo(repos, "github", "homeclaw"); !ok || r.Cloned {
		t.Errorf("github/homeclaw should exist and be uncloned (cross-host): %+v (found=%v)", r, ok)
	}

	// Remote-only repos present and uncloned.
	if r, ok := findRepo(repos, "github", "newgh"); !ok || r.Cloned {
		t.Errorf("github/newgh should be uncloned: %+v (found=%v)", r, ok)
	}
	if r, ok := findRepo(repos, "gitea", "newgt"); !ok || r.Cloned {
		t.Errorf("gitea/newgt should be uncloned: %+v (found=%v)", r, ok)
	}

	// Local-only dirs: host "", cloned true.
	for _, n := range []string{"scratch", "notes"} {
		if r, ok := findRepo(repos, "", n); !ok || !r.Cloned {
			t.Errorf("local-only %q should be present and cloned: %+v (found=%v)", n, r, ok)
		}
	}
}

func TestInventory_DegradesWhenHostErrors(t *testing.T) {
	tmp := t.TempDir() // no local clones
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
	c := &Client{Dir: tmp, run: fake}

	repos, notes, err := c.Inventory(context.Background())
	if err != nil {
		t.Fatalf("a single host outage must not fail the call: %v", err)
	}
	if len(repos) != 1 || repos[0].Host != "gitea" {
		t.Fatalf("expected the surviving gitea repo, got %+v", repos)
	}
	if len(notes) != 1 {
		t.Fatalf("expected one degradation note, got %v", notes)
	}
	if !strings.Contains(notes[0], "github") {
		t.Errorf("note should name the failed host: %q", notes[0])
	}
}

func TestClone_DispatchesByHost(t *testing.T) {
	tmp := t.TempDir()

	t.Run("github goes through gh", func(t *testing.T) {
		fake := &exec.FakeRunner{}
		c := &Client{Dir: tmp, run: fake}
		dest, err := c.Clone(context.Background(), Repo{
			Host: "github", Owner: "cameronsjo", Name: "newgh",
			SSHURL: "git@github.com:cameronsjo/newgh.git",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if dest != filepath.Join(tmp, "newgh") {
			t.Errorf("dest = %q; want %q", dest, filepath.Join(tmp, "newgh"))
		}
		last := fake.Last()
		if last.Name != "gh" || len(last.Args) < 3 || last.Args[0] != "repo" || last.Args[1] != "clone" || last.Args[2] != "cameronsjo/newgh" {
			t.Errorf("expected `gh repo clone cameronsjo/newgh`, got %q %v", last.Name, last.Args)
		}
	})

	t.Run("gitea goes through git clone", func(t *testing.T) {
		fake := &exec.FakeRunner{}
		c := &Client{Dir: tmp, run: fake}
		url := "ssh://git@git.sjo.lol:222/cameron/newgt.git"
		if _, err := c.Clone(context.Background(), Repo{
			Host: "gitea", Owner: "cameron", Name: "newgt", SSHURL: url,
		}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		last := fake.Last()
		joined := strings.Join(last.Args, " ")
		if last.Name != "git" || !strings.Contains(joined, "clone") || !strings.Contains(joined, url) {
			t.Errorf("expected a git clone of %s, got %q %v", url, last.Name, last.Args)
		}
	})
}

func TestClone_RejectsUnsafeName(t *testing.T) {
	tmp := t.TempDir()
	fake := &exec.FakeRunner{}
	c := &Client{Dir: tmp, run: fake}
	for _, name := range []string{"", ".", "..", "../escape", "a/b"} {
		if _, err := c.Clone(context.Background(), Repo{Host: "gitea", Name: name, SSHURL: "ssh://x"}); err == nil {
			t.Errorf("Clone(name=%q) should error on an unsafe name, got nil", name)
		}
	}
	if len(fake.Calls) != 0 {
		t.Errorf("no git command should run for an unsafe name; calls: %+v", fake.Calls)
	}
}

// originGitea answers `git remote get-url origin` with the gitea homeclaw URL —
// used to stand up an existing checkout at the collision path.
func originGitea(name string, args []string) (string, error) {
	if len(args) >= 5 && args[2] == "remote" && args[3] == "get-url" {
		return "ssh://git@git.sjo.lol:222/cameron/homeclaw.git", nil
	}
	return "", nil
}

func TestClone_SameNameDifferentHostErrors(t *testing.T) {
	tmp := t.TempDir()
	// A gitea homeclaw is already checked out at tmp/homeclaw.
	if err := os.MkdirAll(filepath.Join(tmp, "homeclaw"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &exec.FakeRunner{RunFunc: originGitea}
	c := &Client{Dir: tmp, run: fake}

	// Cloning the github homeclaw (same name, different host) must error, not
	// silently return the gitea checkout.
	_, err := c.Clone(context.Background(), Repo{
		Host: "github", Owner: "cameronsjo", Name: "homeclaw",
		SSHURL: "git@github.com:cameronsjo/homeclaw.git",
	})
	if err == nil {
		t.Fatal("expected a name-collision error, got nil (would open the wrong repo)")
	}
	if !strings.Contains(err.Error(), "collides") {
		t.Errorf("error should explain the collision, got: %v", err)
	}
	for _, call := range fake.Calls {
		if strings.Contains(strings.Join(call.Args, " "), "clone") {
			t.Errorf("no clone should run on a collision; ran: %v", call.Args)
		}
	}
}

func TestClone_SameRepoIsIdempotent(t *testing.T) {
	tmp := t.TempDir()
	dest := filepath.Join(tmp, "homeclaw")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &exec.FakeRunner{RunFunc: originGitea}
	c := &Client{Dir: tmp, run: fake}

	// Cloning the repo that's already there returns its path with no clone.
	got, err := c.Clone(context.Background(), Repo{
		Host: "gitea", Owner: "cameron", Name: "homeclaw",
		SSHURL: "ssh://git@git.sjo.lol:222/cameron/homeclaw.git",
	})
	if err != nil {
		t.Fatalf("idempotent clone of the same repo errored: %v", err)
	}
	if got != dest {
		t.Errorf("got %q, want existing dest %q", got, dest)
	}
	for _, call := range fake.Calls {
		if strings.Contains(strings.Join(call.Args, " "), "clone") {
			t.Errorf("no clone should run when the repo already exists; ran: %v", call.Args)
		}
	}
}
