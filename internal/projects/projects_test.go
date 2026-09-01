package projects

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/githubauth"
)

// mkGitDir creates base/name with a .git marker so Discover treats it as a repo.
func mkGitDir(t *testing.T, base, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(base, name, ".git"), 0o750); err != nil {
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
			// With no [projects].owners configured, the inventory asks
			// GitHub.com who the operator is before listing anything.
			if len(args) >= 2 && args[0] == "api" && args[1] == "user" {
				return "cameronsjo", nil
			}
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
			// status --porcelain=v2 --branch → empty output is a clean tree
			// with no branch header, so 0 ahead.
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
	if err := os.Mkdir(filepath.Join(tmp, "notes"), 0o750); err != nil {
		t.Fatal(err) // non-git dir → local-only
	}

	fake := &exec.FakeRunner{RunFunc: inventoryRunFunc(tmp)}
	c := &Client{Dir: tmp, run: fake, gitBin: "git"}

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
	if r, ok := findRepo(repos, "github.com", "forgectl"); !ok || !r.Cloned || r.LocalPath == "" {
		t.Errorf("github/forgectl should be cloned with a local path: %+v (found=%v)", r, ok)
	}
	if r, ok := findRepo(repos, "git.sjo.lol", "homeclaw"); !ok || !r.Cloned || r.LocalPath == "" {
		t.Errorf("gitea/homeclaw should be cloned with a local path: %+v (found=%v)", r, ok)
	}

	// Cross-host: github/homeclaw is a DISTINCT, uncloned row (not collapsed into
	// the cloned gitea/homeclaw by bare name).
	if r, ok := findRepo(repos, "github.com", "homeclaw"); !ok || r.Cloned {
		t.Errorf("github/homeclaw should exist and be uncloned (cross-host): %+v (found=%v)", r, ok)
	}

	// Remote-only repos present and uncloned.
	if r, ok := findRepo(repos, "github.com", "newgh"); !ok || r.Cloned {
		t.Errorf("github/newgh should be uncloned: %+v (found=%v)", r, ok)
	}
	if r, ok := findRepo(repos, "git.sjo.lol", "newgt"); !ok || r.Cloned {
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
	c := &Client{Dir: tmp, run: fake, gitBin: "git"}

	repos, notes, err := c.Inventory(context.Background())
	if err != nil {
		t.Fatalf("a single host outage must not fail the call: %v", err)
	}
	if len(repos) != 1 || repos[0].Host != "git.sjo.lol" {
		t.Fatalf("expected the surviving gitea repo, got %+v", repos)
	}
	if len(notes) != 1 {
		t.Fatalf("expected one degradation note, got %v", notes)
	}
	// The note names the SOURCE (which enumerator failed), not a hostname —
	// a GitHub run that errors produced no rows, so it has no host to name.
	if !strings.Contains(notes[0], "github") {
		t.Errorf("note should name the failed source: %q", notes[0])
	}
}

func TestClone_DispatchesByHost(t *testing.T) {
	tmp := t.TempDir()

	t.Run("github goes through gh", func(t *testing.T) {
		fake := &exec.FakeRunner{}
		c := &Client{Dir: tmp, run: fake, gitBin: "git"}
		dest, err := c.Clone(context.Background(), Repo{
			Host: "github.com", Owner: "cameronsjo", Name: "newgh",
			SSHURL: "git@github.com:cameronsjo/newgh.git",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := canonicalDest(tmp, "github.com", "cameronsjo", "newgh")
		if dest != want {
			t.Errorf("dest = %q; want %q", dest, want)
		}
		last := fake.Last()
		if last.Name != "gh" || len(last.Args) < 3 || last.Args[0] != "repo" || last.Args[1] != "clone" || last.Args[2] != "cameronsjo/newgh" {
			t.Errorf("expected `gh repo clone cameronsjo/newgh`, got %q %v", last.Name, last.Args)
		}
	})

	t.Run("gitea goes through git clone", func(t *testing.T) {
		fake := &exec.FakeRunner{}
		c := &Client{Dir: tmp, run: fake, gitBin: "git"}
		url := "ssh://git@git.sjo.lol:222/cameron/newgt.git"
		if _, err := c.Clone(context.Background(), Repo{
			Host: "git.sjo.lol", Owner: "cameron", Name: "newgt", SSHURL: url,
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

// TestClone_PinsGhToGitHubComDespiteAmbientHost covers the clone leg's own
// host pin. A mislabeled listing row is a bad table cell; a clone REDIRECTED to
// an enterprise host by an ambient GH_HOST persists to disk at
// Dir/github/<owner>/<name>, where originMatches then disagrees with it on
// every later run. The literal is deliberate — asserting against
// githubauth.Host would agree with whatever that constant became.
func TestClone_PinsGhToGitHubComDespiteAmbientHost(t *testing.T) {
	t.Setenv("GH_HOST", "ghe.example.test")
	fake := &exec.FakeRunner{}
	c := &Client{Dir: t.TempDir(), run: fake, gitBin: "git"}

	if _, err := c.Clone(context.Background(), Repo{
		Host: "github.com", Owner: "cameronsjo", Name: "newgh",
	}); err != nil {
		t.Fatalf("Clone: %v", err)
	}

	last := fake.Last()
	if last.Name != "gh" {
		t.Fatalf("last command = %q, want gh", last.Name)
	}
	if got := last.Env["GH_HOST"]; got != "github.com" {
		t.Fatalf("GH_HOST = %q, want github.com — the clone must not follow an ambient host", got)
	}
}

// TestListOrg_RejectsUnsafeLogin guards the caller-supplied `--org` value: an
// empty, traversal, or leading-'-' login must be refused before it becomes a
// `gh` argv (a '-'-leading value would be read as a flag, not a positional).
func TestListOrg_RejectsUnsafeLogin(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := &Client{Dir: t.TempDir(), run: fake, gitBin: "git"}
	for _, org := range []string{"", ".", "..", "a/b", "-x", "--all"} {
		if _, err := c.ListOrg(context.Background(), org); err == nil {
			t.Errorf("ListOrg(%q) should reject an unsafe login, got nil", org)
		}
	}
	if len(fake.Calls) != 0 {
		t.Errorf("no gh command should run for an unsafe login; calls: %+v", fake.Calls)
	}
}

func TestListOrg_ValidLoginLists(t *testing.T) {
	out := `[{"isPrivate":false,"name":"claude-code","sshUrl":"git@github.com:anthropics/claude-code.git"}]`
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) { return out, nil }}
	c := &Client{Dir: t.TempDir(), run: fake, gitBin: "git"}
	repos, err := c.ListOrg(context.Background(), "anthropics")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(repos) != 1 || repos[0].Owner != "anthropics" {
		t.Errorf("ListOrg = %+v; want one repo owned by anthropics", repos)
	}
}

func TestClone_RejectsUnsafeName(t *testing.T) {
	tmp := t.TempDir()
	fake := &exec.FakeRunner{}
	c := &Client{Dir: tmp, run: fake, gitBin: "git"}
	for _, name := range []string{"", ".", "..", "../escape", "a/b"} {
		if _, err := c.Clone(context.Background(), Repo{Host: "git.sjo.lol", Owner: "cameron", Name: name, SSHURL: "ssh://x"}); err == nil {
			t.Errorf("Clone(name=%q) should error on an unsafe name, got nil", name)
		}
	}
	if len(fake.Calls) != 0 {
		t.Errorf("no git command should run for an unsafe name; calls: %+v", fake.Calls)
	}
}

// TestClone_RejectsUnsafeHostOrOwner extends the traversal guard to the two
// new path segments the canonical layout introduces: a malformed list row (or
// a hand-crafted Repo) with ".."/empty Host or Owner must not be joined into
// a filesystem path.
func TestClone_RejectsUnsafeHostOrOwner(t *testing.T) {
	tmp := t.TempDir()
	fake := &exec.FakeRunner{}
	c := &Client{Dir: tmp, run: fake, gitBin: "git"}
	cases := []struct{ host, owner string }{
		{"../escape", "cameron"},
		{"git.sjo.lol", "../escape"},
		{"", "cameron"},
		{"git.sjo.lol", ""},
		{"gitea/etc", "cameron"},
	}
	for _, tc := range cases {
		if _, err := c.Clone(context.Background(), Repo{Host: tc.host, Owner: tc.owner, Name: "repo", SSHURL: "ssh://x"}); err == nil {
			t.Errorf("Clone(host=%q, owner=%q) should error on an unsafe path segment, got nil", tc.host, tc.owner)
		}
	}
	if len(fake.Calls) != 0 {
		t.Errorf("no git command should run for an unsafe host/owner; calls: %+v", fake.Calls)
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

// TestClone_CrossHostDissolvesCollision shows the canonical layout structurally
// dissolves the flat-layout collision the old guard existed to catch:
// github/homeclaw and gitea/homeclaw now land at distinct dirs, so cloning one
// while the other is already checked out no longer errors.
func TestClone_CrossHostDissolvesCollision(t *testing.T) {
	tmp := t.TempDir()
	giteaDest := canonicalDest(tmp, "git.sjo.lol", "cameron", "homeclaw")
	if err := os.MkdirAll(giteaDest, 0o750); err != nil {
		t.Fatal(err)
	}
	fake := &exec.FakeRunner{RunFunc: originGitea}
	c := &Client{Dir: tmp, run: fake, gitBin: "git"}

	dest, err := c.Clone(context.Background(), Repo{
		Host: "github.com", Owner: "cameronsjo", Name: "homeclaw",
		SSHURL: "git@github.com:cameronsjo/homeclaw.git",
	})
	if err != nil {
		t.Fatalf("cross-host same-name clone should no longer collide: %v", err)
	}
	if dest == giteaDest {
		t.Fatalf("github/homeclaw must not land at the gitea dest: %q", dest)
	}
}

// TestClone_ExistingCanonicalDestWrongOriginErrors keeps the guard's original
// safety intent alive under the canonical layout: a dest that already exists
// but whose origin doesn't match r (hand-populated, or the upstream repo
// changed) must still error rather than silently be treated as "already
// cloned".
func TestClone_ExistingCanonicalDestWrongOriginErrors(t *testing.T) {
	tmp := t.TempDir()
	dest := canonicalDest(tmp, "git.sjo.lol", "cameron", "homeclaw")
	if err := os.MkdirAll(dest, 0o750); err != nil {
		t.Fatal(err)
	}
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if len(args) >= 5 && args[2] == "remote" && args[3] == "get-url" {
			return "ssh://git@git.sjo.lol:222/cameron/somethingelse.git", nil
		}
		return "", nil
	}}
	c := &Client{Dir: tmp, run: fake, gitBin: "git"}

	_, err := c.Clone(context.Background(), Repo{
		Host: "git.sjo.lol", Owner: "cameron", Name: "homeclaw",
		SSHURL: "ssh://git@git.sjo.lol:222/cameron/homeclaw.git",
	})
	if err == nil {
		t.Fatal("expected an origin-mismatch error, got nil (would open the wrong repo)")
	}
	// The message names the PATH (forgectl-composed from guarded segments) but
	// not the host/owner/name, which are server-supplied and would otherwise
	// reach a terminal unescaped.
	if !strings.Contains(err.Error(), "origin is a different repo") {
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
	dest := canonicalDest(tmp, "git.sjo.lol", "cameron", "homeclaw")
	if err := os.MkdirAll(dest, 0o750); err != nil {
		t.Fatal(err)
	}
	fake := &exec.FakeRunner{RunFunc: originGitea}
	c := &Client{Dir: tmp, run: fake, gitBin: "git"}

	// Cloning the repo that's already there returns its path with no clone.
	got, err := c.Clone(context.Background(), Repo{
		Host: "git.sjo.lol", Owner: "cameron", Name: "homeclaw",
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

// mkCanonicalGitDir creates base/host/owner/name with a .git marker — a
// canonical-layout clone — and returns its full path.
func mkCanonicalGitDir(t *testing.T, base, host, owner, name string) string {
	t.Helper()
	dir := canonicalDest(base, host, owner, name)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestDiscover_FindsBothCanonicalAndFlatLayouts is the core assertion for the
// walk-depth change: a canonical host/owner/repo clone and a legacy flat
// clone sitting side by side under the same Dir must BOTH surface, and a
// non-git flat dir (no canonical structure beneath it) must still surface as
// a flat, local-only project — not be swallowed by the canonical walk.
func TestDiscover_FindsBothCanonicalAndFlatLayouts(t *testing.T) {
	tmp := t.TempDir()
	canonDir := mkCanonicalGitDir(t, tmp, "github.com", "cameronsjo", "forgectl")
	mkGitDir(t, tmp, "homeclaw") // legacy flat clone, still on disk
	if err := os.Mkdir(filepath.Join(tmp, "notes"), 0o750); err != nil {
		t.Fatal(err) // plain non-git dir, no canonical structure beneath it
	}

	c := &Client{Dir: tmp, run: &exec.FakeRunner{}, gitBin: "git"}
	projs, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	byName := make(map[string]Project, len(projs))
	for _, p := range projs {
		byName[p.Name] = p
	}

	if p, ok := byName["forgectl"]; !ok || p.Dir != canonDir {
		t.Errorf("canonical clone not discovered correctly: %+v (found=%v), want dir %q", p, ok, canonDir)
	}
	if p, ok := byName["homeclaw"]; !ok || p.Dir != filepath.Join(tmp, "homeclaw") {
		t.Errorf("legacy flat clone not discovered correctly: %+v (found=%v)", p, ok)
	}
	if p, ok := byName["notes"]; !ok || p.Dir != filepath.Join(tmp, "notes") {
		t.Errorf("non-git flat dir not discovered correctly: %+v (found=%v)", p, ok)
	}
	// "github.com" (the host bucket) must not itself appear as a project — it was
	// walked into, not treated as a flat clone.
	if _, ok := byName["github.com"]; ok {
		t.Errorf("host bucket %q leaked into the project list: %+v", "github.com", projs)
	}
}

// TestDiscover_NonGitDir_StatusIsNotRepo reuses the three-way fixture from
// TestDiscover_FindsBothCanonicalAndFlatLayouts (a canonical clone, a flat
// clone, and a non-git dir) to assert the state each one lands on: the
// non-git dir must be StatusNotRepo, and — the control proving the fix
// didn't just blank the status for everyone — the real repo must be
// StatusOK.
func TestDiscover_NonGitDir_StatusIsNotRepo(t *testing.T) {
	tmp := t.TempDir()
	mkCanonicalGitDir(t, tmp, "github.com", "cameronsjo", "forgectl")
	mkGitDir(t, tmp, "homeclaw")
	if err := os.Mkdir(filepath.Join(tmp, "notes"), 0o750); err != nil {
		t.Fatal(err)
	}

	c := &Client{Dir: tmp, run: v2CleanRunner(), gitBin: "git"}
	projs, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	byName := make(map[string]Project, len(projs))
	for _, p := range projs {
		byName[p.Name] = p
	}

	if got := byName["notes"].Status.State; got != StatusNotRepo {
		t.Errorf("non-git dir: Status.State = %q, want %q", got, StatusNotRepo)
	}
	if got := byName["homeclaw"].Status.State; got != StatusOK {
		t.Errorf("control real repo: Status.State = %q, want %q", got, StatusOK)
	}
}

// TestInventory_StatusProcessBudget pins forgectl#216 end to end: a full
// Inventory row costs exactly two git processes per repository — one status
// probe and one origin lookup — whether the tree is clean or dirty. Before
// the porcelain-v2 collapse a clean row cost three, because learning the
// ahead count needed a second `rev-list` walk.
//
// Calls are filtered by binary, repo dir, and subcommand rather than by slice
// index: Inventory fans the status phase and the origin phase out across
// discoverConcurrency() workers each, so completion order is undefined and an
// index-based assertion would be asserting on the scheduler.
func TestInventory_StatusProcessBudget(t *testing.T) {
	cases := []struct {
		name      string
		statusOut string
		want      GitStatus
	}{
		{
			name:      "clean row keeps its ahead count without a second process",
			statusOut: v2Branch(2, 0),
			want:      GitStatus{State: StatusOK, Ahead: 2},
		},
		{
			name:      "dirty row reports counts and discards ahead",
			statusOut: v2Out(v2Branch(2, 0), v2Ordinary, v2Untracked),
			want:      GitStatus{State: StatusOK, Modified: 1, Untracked: 1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			mkGitDir(t, tmp, "forgectl")
			repo := filepath.Join(tmp, "forgectl")

			fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
				switch name {
				case "gh":
					if len(args) >= 2 && args[0] == "api" && args[1] == "user" {
						return "cameronsjo", nil
					}
					return `[{"name":"forgectl","sshUrl":"git@github.com:cameronsjo/forgectl.git","isPrivate":false}]`, nil
				case "tea":
					return "owner\tname\ttype\tssh\n", nil
				case "git":
					if len(args) >= 5 && args[0] == "-C" && args[2] == "remote" {
						return "git@github.com:cameronsjo/forgectl.git", nil
					}
					return tc.statusOut, nil
				}
				return "", nil
			}}
			c := &Client{Dir: tmp, run: fake, gitBin: "git"}

			repos, notes, err := c.Inventory(context.Background())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(notes) != 0 {
				t.Errorf("expected no degradation notes, got %v", notes)
			}

			r, ok := findRepo(repos, "github.com", "forgectl")
			if !ok {
				t.Fatalf("github/forgectl missing from inventory: %+v", repos)
			}
			if r.Status != tc.want {
				t.Errorf("Status = %+v, want %+v", r.Status, tc.want)
			}

			counts := countGitSubcommands(fake.Calls, repo)
			if counts["status"] != 1 {
				t.Errorf("status calls = %d, want exactly 1", counts["status"])
			}
			if counts["remote"] != 1 {
				t.Errorf("remote get-url calls = %d, want exactly 1", counts["remote"])
			}
			if counts["rev-list"] != 0 {
				t.Errorf("rev-list calls = %d, want 0", counts["rev-list"])
			}
			total := 0
			for _, n := range counts {
				total += n
			}
			if total != 2 {
				t.Errorf("git calls for %s = %d (%v), want exactly 2", repo, total, counts)
			}
		})
	}
}

// TestLocalRepos_NonRepo_SpawnsNoRemoteLookup pins claim 3 (no ancestor-origin
// escape) and the paired spawn reduction: a non-git dir must not trigger
// `git remote get-url origin` at all — that call would walk up past the
// non-repo dir and pick up an ancestor's origin. Filters fake.Calls by
// content (dir + "remote"), never by index, since ordering is not something
// this test should depend on.
func TestLocalRepos_NonRepo_SpawnsNoRemoteLookup(t *testing.T) {
	tmp := t.TempDir()
	mkGitDir(t, tmp, "realrepo")
	nonRepo := filepath.Join(tmp, "notes")
	if err := os.Mkdir(nonRepo, 0o750); err != nil {
		t.Fatal(err)
	}

	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		return "", nil
	}}
	c := &Client{Dir: tmp, run: fake, gitBin: "git"}

	if _, err := c.localRepos(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, call := range fake.Calls {
		if call.Name != "git" || len(call.Args) < 2 {
			continue
		}
		if call.Args[1] == nonRepo && strings.Contains(strings.Join(call.Args, " "), "remote") {
			t.Errorf("remote get-url was spawned for the non-repo dir: %v", call.Args)
		}
	}
}

// TestDiscover_CanonicalHostBucketMultipleOwnersAndRepos exercises the walk
// beyond a single owner/repo pair — Inventory/pick/list all depend on every
// canonical clone surfacing, not just the first found.
func TestDiscover_CanonicalHostBucketMultipleOwnersAndRepos(t *testing.T) {
	tmp := t.TempDir()
	mkCanonicalGitDir(t, tmp, "git.sjo.lol", "cameron", "homeclaw")
	mkCanonicalGitDir(t, tmp, "git.sjo.lol", "cameron", "forgectl")
	mkCanonicalGitDir(t, tmp, "git.sjo.lol", "otherowner", "sidecar")

	c := &Client{Dir: tmp, run: &exec.FakeRunner{}, gitBin: "git"}
	projs, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(projs) != 3 {
		t.Fatalf("got %d projects, want 3: %+v", len(projs), projs)
	}
}

// TestCanonicalDest_LowercasesAndMirrorsKey confirms the filesystem tree
// matches Repo.Key()'s case-insensitive identity.
func TestCanonicalDest_LowercasesAndMirrorsKey(t *testing.T) {
	got := canonicalDest("/base", "GitHub.COM", "CameronSjo", "Forgectl")
	want := filepath.Join("/base", "github.com", "cameronsjo", "forgectl")
	if got != want {
		t.Errorf("canonicalDest = %q; want %q", got, want)
	}
}

// TestValidPathSegment_RejectsTraversalAndSeparators is the pure-logic
// companion to TestClone_RejectsUnsafeHostOrOwner — asserts the guard
// directly rather than only through Clone's side effects.
func TestValidPathSegment_RejectsTraversalAndSeparators(t *testing.T) {
	bad := []string{"", ".", "..", "../escape", "a/b", `a\b`, "-flag", "--org"}
	for _, s := range bad {
		if validPathSegment(s) {
			t.Errorf("validPathSegment(%q) = true, want false", s)
		}
	}
	good := []string{"github.com", "cameronsjo", "git.sjo.lol", "forge-ctl"}
	for _, s := range good {
		if !validPathSegment(s) {
			t.Errorf("validPathSegment(%q) = false, want true", s)
		}
	}
}

// mkMixedFanOutFixture populates tmp with a mix of canonical (host/owner/repo)
// and flat (top-level) clones — 30 total — so the fan-out phase actually has
// enough candidates in play to expose a nondeterministic ordering, were one
// present. Ten owners get a same-named "tool" repo under BOTH the github and
// gitea host buckets, so Name collisions actually occur and the sort
// comparator's Dir tie-break branch is exercised by a real Discover() call
// rather than sitting dead in this test. (It does not, by itself, prove the
// tie-break is load-bearing — see TestSortProjects_TieBreaksOnDir for that.)
func mkMixedFanOutFixture(t *testing.T, tmp string) {
	t.Helper()
	for i := 0; i < 10; i++ {
		mkGitDir(t, tmp, "flat-"+strconv.Itoa(i))
	}
	for i := 0; i < 10; i++ {
		owner := "owner-" + strconv.Itoa(i)
		mkCanonicalGitDir(t, tmp, "github.com", owner, "tool")
		mkCanonicalGitDir(t, tmp, "git.sjo.lol", owner, "tool")
	}
}

// nameDir is the (Name, Dir) projection asserted by the determinism test —
// Status is excluded since it isn't what ordering depends on.
type nameDir struct{ Name, Dir string }

func projectNameDirs(projs []Project) []nameDir {
	out := make([]nameDir, len(projs))
	for i, p := range projs {
		out[i] = nameDir{p.Name, p.Dir}
	}
	return out
}

// TestDiscover_FanOutIsRepeatable is an end-to-end regression check: two
// Discover() calls over the same ~30-entry mixed fixture must produce
// byte-identical, complete, ordered sequences. Renamed from
// TestDiscover_FanOutIsDeterministic (which claimed to pin the sort's Dir
// tie-break) after an Opus review of PR #235 proved that claim false by
// mutation: even with the tie-break hard-removed, this run-twice-and-compare
// style of check stays green, because it can only ever observe whatever the
// production pipeline happens to do — it asserts repeatability, not a
// specific ordering rule.
//
// This test earns its keep on repeatability alone (a real regression class:
// a future change introducing a genuinely nondeterministic step — a map
// iteration, an unseeded random tiebreak — would be caught here) but proves
// nothing about the tie-break specifically. See two other tests for that
// claim, run independently:
//   - TestDiscover_CollidingNamesOrderByDir below asserts the ordering
//     PROPERTY directly (dir-ascending among same-Name entries) through this
//     same Discover() path. It is included for completeness but is, by
//     measurement, EQUALLY unable to falsify the tie-break — see its own
//     comment for why, and don't mistake its presence for coverage this
//     test lacks.
//   - TestSortProjects_TieBreaksOnDir is the one that actually goes red
//     without the tie-break: it calls sortProjects directly with a
//     deliberately non-Dir-sorted input, bypassing the filesystem walk that
//     makes both Discover-routed tests structurally blind to this mutation.
func TestDiscover_FanOutIsRepeatable(t *testing.T) {
	tmp := t.TempDir()
	mkMixedFanOutFixture(t, tmp)
	c := &Client{Dir: tmp, run: &exec.FakeRunner{}, gitBin: "git"}

	first, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(first) != 30 {
		t.Fatalf("got %d projects, want 30", len(first))
	}
	if !reflect.DeepEqual(projectNameDirs(first), projectNameDirs(second)) {
		t.Errorf("Discover order is nondeterministic:\nfirst:  %+v\nsecond: %+v", projectNameDirs(first), projectNameDirs(second))
	}
}

// TestDiscover_CollidingNamesOrderByDir asserts the ordering PROPERTY the
// tie-break is meant to guarantee — directly, not via a two-run comparison —
// over the same mkMixedFanOutFixture used above: for every pair of entries
// that share a Name, the earlier one in Discover's result must have the
// lexicographically smaller Dir.
//
// Measured by mutation (comparator collapsed to Name-only, same as
// TestSortProjects_TieBreaksOnDir's mutation): this assertion STILL PASSES.
// The reason is structural, not a test-design gap: os.ReadDir returns
// filename-sorted entries at every level of the walk, so phase 1 always
// hands phase 2 its candidates in an order that already agrees with
// Dir-lexicographic order for any fixture built from real directories —
// tie-break or not, the colliding entries arrive at the final sort already
// in the order this test checks for. This test is kept anyway because the
// property itself (colliding entries ARE dir-ordered in the shipped output)
// is real and worth pinning as a regression check; it is not a substitute
// for TestSortProjects_TieBreaksOnDir, which is the one that can actually
// fail this mutation.
func TestDiscover_CollidingNamesOrderByDir(t *testing.T) {
	tmp := t.TempDir()
	mkMixedFanOutFixture(t, tmp)
	c := &Client{Dir: tmp, run: &exec.FakeRunner{}, gitBin: "git"}

	projs, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	lastDirByName := map[string]string{}
	sawCollision := false
	for _, p := range projs {
		if prev, ok := lastDirByName[p.Name]; ok {
			sawCollision = true
			if prev >= p.Dir {
				t.Errorf("Name %q: Dir %q did not sort after previous Dir %q", p.Name, p.Dir, prev)
			}
		}
		lastDirByName[p.Name] = p.Dir
	}
	if !sawCollision {
		t.Fatal("fixture produced no Name collisions — test asserts nothing")
	}
}

// TestDiscover_BoundsConcurrency proves discoverDir's phase 2 actually runs
// concurrently AND stays within discoverConcurrency()'s bound. Both halves
// are load-bearing: without the peak>1 assertion this test would also pass
// against fully-serial code.
func TestDiscover_BoundsConcurrency(t *testing.T) {
	tmp := t.TempDir()
	mkMixedFanOutFixture(t, tmp)

	var inFlight, peak int64
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		cur := atomic.AddInt64(&inFlight, 1)
		defer atomic.AddInt64(&inFlight, -1)
		for {
			p := atomic.LoadInt64(&peak)
			if cur <= p || atomic.CompareAndSwapInt64(&peak, p, cur) {
				break
			}
		}
		// A near-instant fake call finishes before the scheduler fans the rest
		// of the batch out, so real concurrency would go unobserved without
		// this: hold the call open briefly so overlapping goroutines actually
		// overlap in wall-clock time rather than running effectively serially.
		time.Sleep(5 * time.Millisecond)
		// git status --porcelain=v2 --branch output; empty parses as clean,
		// which is all this test needs — it counts spawns, not states.
		return "", nil
	}}
	c := &Client{Dir: tmp, run: fake, gitBin: "git"}

	if _, err := c.Discover(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := atomic.LoadInt64(&peak)
	if got <= 1 {
		t.Errorf("peak in-flight calls = %d, want > 1 (fan-out never ran concurrently)", got)
	}
	// Comparing against discoverConcurrency() itself is a shape check, not a
	// value check: it catches an unbounded fan-out (peak growing past
	// whatever the bound is), but it cannot catch the bound being wrong —
	// e.g. a typo'd discoverConcurrency() returning 1000 would still pass
	// this assertion.
	if want := int64(discoverConcurrency()); got > want {
		t.Errorf("peak in-flight calls = %d, want <= discoverConcurrency() = %d", got, want)
	}
}

// TestSortProjects_TieBreaksOnDir unit-tests sortProjects directly, bypassing
// discoverDir's filesystem walk entirely. That matters: routing a fixture
// through Discover can never falsify the tie-break (see the comments on
// TestDiscover_FanOutIsRepeatable and TestDiscover_CollidingNamesOrderByDir,
// both proven unable to catch it by the same mutation) because os.ReadDir's
// per-level sorted traversal always hands phase 1 its candidates in an order
// that already agrees with Dir order. Building the input slice directly here
// sidesteps that constraint — the two colliding "tool" entries are given
// deliberately Dir-DESCENDING input order, so only an explicit tie-break,
// not incidental pre-sort ordering, can produce the wanted ascending
// result. This is the ONLY test in this file proven (by mutation) to fail
// without the tie-break.
func TestSortProjects_TieBreaksOnDir(t *testing.T) {
	projects := []Project{
		{Name: "tool", Dir: "/z/tool"},
		{Name: "alpha", Dir: "/x/alpha"},
		{Name: "tool", Dir: "/a/tool"},
	}
	sortProjects(projects)

	want := []nameDir{
		{"alpha", "/x/alpha"},
		{"tool", "/a/tool"},
		{"tool", "/z/tool"},
	}
	if got := projectNameDirs(projects); !reflect.DeepEqual(got, want) {
		t.Errorf("sortProjects order = %+v, want %+v", got, want)
	}
}

// TestFanOut_PreservesOrder unit-tests the helper directly: results must land
// at their input index regardless of completion order, including the empty
// and single-item edge cases.
func TestFanOut_PreservesOrder(t *testing.T) {
	t.Run("many items", func(t *testing.T) {
		in := make([]int, 50)
		for i := range in {
			in[i] = i
		}
		out := fanOut(in, func(i int) string {
			return fmt.Sprintf("v%d", i)
		})
		if len(out) != len(in) {
			t.Fatalf("got %d results, want %d", len(out), len(in))
		}
		for i, v := range out {
			want := fmt.Sprintf("v%d", i)
			if v != want {
				t.Errorf("out[%d] = %q, want %q", i, v, want)
			}
		}
	})

	t.Run("empty input", func(t *testing.T) {
		out := fanOut([]int{}, func(i int) int { return i * 2 })
		if len(out) != 0 {
			t.Errorf("got %d results, want 0", len(out))
		}
	})

	t.Run("single item", func(t *testing.T) {
		out := fanOut([]int{7}, func(i int) int { return i * 2 })
		if len(out) != 1 || out[0] != 14 {
			t.Errorf("got %v, want [14]", out)
		}
	})
}

// TestInventory_GHEHostPinsAndRemovesTokens: WithGitHubHost threads all the way to
// the gh subprocess env — the pin carries the configured host and, because it
// is non-default, the token variables are removed. The observable
// consequence (finding 4): gh cannot use an ambient credential; only its
// hosts.yml credential for that host remains.
func TestInventory_GHEHostPinsAndRemovesTokens(t *testing.T) {
	t.Setenv("GH_HOST", "ambient.example.test")
	t.Setenv("GH_TOKEN", "ambient-token")
	fake := &exec.FakeRunner{RunFunc: func(name string, _ []string) (string, error) {
		if name == "gh" {
			return `[]`, nil
		}
		return "", nil
	}}
	c := New(fake, WithGitHubOwners([]string{"acme"}), WithGitHubHost("github.example.com"))

	if _, err := c.ListOrg(t.Context(), "acme"); err != nil {
		t.Fatalf("ListOrg: %v", err)
	}

	var ghCall *exec.Call
	for i := range fake.Calls {
		if fake.Calls[i].Name == "gh" {
			ghCall = &fake.Calls[i]
			break
		}
	}
	if ghCall == nil {
		t.Fatalf("no gh call recorded: %+v", fake.Calls)
	}
	if got := ghCall.Env["GH_HOST"]; got != "github.example.com" {
		t.Errorf("GH_HOST = %q, want github.example.com", got)
	}
	removals := make(map[string]int, len(ghCall.UnsetEnv))
	for _, key := range ghCall.UnsetEnv {
		removals[key]++
	}
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
		if value, present := ghCall.Env[key]; present {
			t.Errorf("%s remains in overrides with value %q, want absent", key, value)
		}
		if removals[key] != 1 {
			t.Errorf("%s removal count = %d in %v, want exactly 1", key, removals[key], ghCall.UnsetEnv)
		}
	}
}

// TestClone_ForgedHostTokenDoesNotReachGh is the dispatch half of the
// forgeable-token defect. Before hostnames replaced the short tokens, a remote
// whose bare hostname was literally "github" came out of canonicalHost's
// UNTRUSTED arm holding the TRUSTED arm's value, so `clone
// https://github/evil/repo` ran `gh repo clone evil/repo` against public
// github.com — cloning an attacker-chosen repo from a server the URL never
// named.
//
// The zero-value Client here is deliberate and load-bearing twice over: it
// carries no gitHubHost, so it also proves effectiveGitHubHost's default is
// what the dispatch compares against. Without that default, this test would
// pass for the wrong reason — every host would miss the gh branch.
func TestClone_ForgedHostTokenDoesNotReachGh(t *testing.T) {
	for _, host := range []string{"github", "GITHUB", "gitea", "github.com.attacker.net"} {
		t.Run(host, func(t *testing.T) {
			fake := &exec.FakeRunner{}
			c := &Client{Dir: t.TempDir(), run: fake, gitBin: "git"}
			url := "https://" + host + "/evil/repo"
			if _, err := c.Clone(context.Background(), Repo{
				Host: host, Owner: "evil", Name: "repo", SSHURL: url,
			}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if last := fake.Last(); last.Name == "gh" {
				t.Fatalf("host %q reached gh (%v) — it must clone its own URL as a plain git clone", host, last.Args)
			}
		})
	}
}

// TestClone_ZeroValueClientStillDispatchesGitHubThroughGh pins the other side
// of the same default. A struct-literal Client carries no host; if the
// dispatch compared against that empty field directly, a genuine github.com
// repo would fall to the else arm and be cloned from a server-supplied URL
// with no GH_HOST pin and no token scrub.
func TestClone_ZeroValueClientStillDispatchesGitHubThroughGh(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := &Client{Dir: t.TempDir(), run: fake, gitBin: "git"}
	if _, err := c.Clone(context.Background(), Repo{
		Host: githubauth.DefaultHost, Owner: "cameronsjo", Name: "forgectl",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if last := fake.Last(); last.Name != "gh" {
		t.Errorf("zero-value client dispatched %q, want gh — the default host was not applied", last.Name)
	}
}

// canonicalDest is a test-only shim preserving the pre-Placement call shape,
// so the many dest assertions read as (root, host, owner, name) rather than
// constructing a Repo each time. It exercises the host-tree branch only —
// wing placement has its own tests.
func canonicalDest(root, host, owner, name string) string {
	p, err := Placement(root, Repo{Host: host, Owner: owner, Name: name}, "")
	if err != nil {
		panic("canonicalDest test shim: " + err.Error())
	}
	return p
}

func TestPlacement_HostTreeMirrorsKey(t *testing.T) {
	r := Repo{Host: "GitHub.COM", Owner: "CameronSjo", Name: "Forgectl"}
	got, err := Placement("/base", r, "")
	if err != nil {
		t.Fatalf("Placement: %v", err)
	}
	want := filepath.Join("/base", "github.com", "cameronsjo", "forgectl")
	if got != want {
		t.Errorf("Placement = %q; want %q", got, want)
	}
	// The tree must mirror the dedup identity: same lowercasing, same order.
	if !strings.HasSuffix(strings.ToLower(got), r.Key()) {
		t.Errorf("Placement %q does not mirror Key() %q", got, r.Key())
	}
}

// TestPlacement_WingDropsTheOwnerLevel pins estate rule 1: a wing member sits
// one level under the root, not two. This is exactly why discovery has to look
// at depth 2 as well — the level the owner would have occupied is gone.
func TestPlacement_WingDropsTheOwnerLevel(t *testing.T) {
	r := Repo{Host: "github.com", Owner: "CameronSjo", Name: "Forgectl"}
	got, err := Placement("/base", r, "Cadence-Ecosystem")
	if err != nil {
		t.Fatalf("Placement: %v", err)
	}
	want := filepath.Join("/base", "cadence-ecosystem", "forgectl")
	if got != want {
		t.Errorf("Placement = %q; want %q", got, want)
	}
	if strings.Contains(got, "cameronsjo") || strings.Contains(got, "github.com") {
		t.Errorf("wing placement kept a host or owner level: %q", got)
	}
}

// TestPlacement_HostSegmentGuardRejectsWhatTraversalMisses is the two-tier
// guard's whole reason for existing. Every value here passes validPathSegment
// — the guard the host segment used to get — and every one of them is a
// hostname a remote URL could produce.
func TestPlacement_HostSegmentGuardRejectsWhatTraversalMisses(t *testing.T) {
	for _, host := range []string{
		"::1",                    // IPv6 literal: ':' is APFS-legal, Finder renders it as '/'
		"host:8443",              // a colon by any other route
		".hidden",                // a host tree invisible to ls
		"hostname.",              // trailing root label; canonicalHost strips it, this catches a bypass
		"host name",              // whitespace
		"host\x1b[31m",           // ANSI escape
		"hоst",                   // Cyrillic 'о' homoglyph
		"-host",                  // flag injection
		strings.Repeat("a", 254), // past NAME_MAX, and past the DNS limit
	} {
		t.Run(host, func(t *testing.T) {
			if _, err := Placement("/base", Repo{Host: host, Owner: "o", Name: "n"}, ""); err == nil {
				t.Errorf("host %q was accepted as a path segment", host)
			}
		})
	}
}

func TestPlacement_RejectsUnsafeOwnerNameAndWing(t *testing.T) {
	base := Repo{Host: "github.com", Owner: "o", Name: "n"}
	for _, tc := range []struct {
		name string
		r    Repo
		wing string
	}{
		{"empty name", Repo{Host: "github.com", Owner: "o"}, ""},
		{"traversal name", Repo{Host: "github.com", Owner: "o", Name: ".."}, ""},
		{"slash in name", Repo{Host: "github.com", Owner: "o", Name: "a/b"}, ""},
		{"flag name", Repo{Host: "github.com", Owner: "o", Name: "-n"}, ""},
		{"empty owner", Repo{Host: "github.com", Name: "n"}, ""},
		{"traversal owner", Repo{Host: "github.com", Owner: "..", Name: "n"}, ""},
		{"traversal wing", base, ".."},
		{"slash in wing", base, "a/b"},
		{"flag wing", base, "-w"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Placement("/base", tc.r, tc.wing); err == nil {
				t.Error("accepted an unsafe segment")
			}
		})
	}
}

// TestPlacement_WingSkipsTheHostGuard documents a deliberate asymmetry: a wing
// member's path never contains the host, so a repo on an unplaceable host is
// still placeable into a wing. The host is only guarded where it is used.
func TestPlacement_WingSkipsTheHostGuard(t *testing.T) {
	got, err := Placement("/base", Repo{Host: "::1", Owner: "o", Name: "n"}, "w")
	if err != nil {
		t.Fatalf("a wing member should not be blocked by its host: %v", err)
	}
	if want := filepath.Join("/base", "w", "n"); got != want {
		t.Errorf("Placement = %q, want %q", got, want)
	}
}

// TestClone_WingMemberLandsInTheWing is the end-to-end half: the table on the
// Client, not a flag, is what routes it — so `Worktree`, which takes no flag,
// gets the same answer.
func TestClone_WingMemberLandsInTheWing(t *testing.T) {
	tmp := t.TempDir()
	table, err := ResolveWings("github.com", []Wing{
		{Name: "cadence-ecosystem", Repos: []string{"cameronsjo/forgectl"}},
	})
	if err != nil {
		t.Fatalf("ResolveWings: %v", err)
	}
	fake := &exec.FakeRunner{}
	c := &Client{Dir: tmp, run: fake, gitBin: "git", wings: table}

	dest, err := c.Clone(context.Background(), Repo{
		Host: "github.com", Owner: "cameronsjo", Name: "forgectl",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := filepath.Join(tmp, "cadence-ecosystem", "forgectl"); dest != want {
		t.Errorf("dest = %q; want %q", dest, want)
	}

	// A repo NOT in the table keeps the host tree — estate rule 2 verbatim.
	dest, err = c.Clone(context.Background(), Repo{
		Host: "github.com", Owner: "cameronsjo", Name: "unlisted",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := filepath.Join(tmp, "github.com", "cameronsjo", "unlisted"); dest != want {
		t.Errorf("unlisted dest = %q; want the host tree %q", dest, want)
	}
}

// TestClone_DuplicateAcrossLayoutsIsANoOp covers the cross-tree probe. A repo
// can be filed two ways — its wing or the host tree — and a clone routed to
// one while already checked out at the other would mint the duplicate the
// 2026-08-04 estate reorganization spent real effort removing.
//
// Both directions are tested, because the failure is symmetric and a one-sided
// guard reads as working right up until the day the wing table changes.
func TestClone_DuplicateAcrossLayoutsIsANoOp(t *testing.T) {
	table, err := ResolveWings("github.com", []Wing{
		{Name: "cadence-ecosystem", Repos: []string{"cameronsjo/forgectl"}},
	})
	if err != nil {
		t.Fatalf("ResolveWings: %v", err)
	}
	r := Repo{Host: "github.com", Owner: "cameronsjo", Name: "forgectl"}
	const origin = "git@github.com:cameronsjo/forgectl.git"

	for _, tc := range []struct {
		name     string
		existing []string // path segments under tmp
	}{
		{"already in the host tree, routed to the wing", []string{"github.com", "cameronsjo", "forgectl"}},
		{"already in the wing, routed to the host tree", []string{"cadence-ecosystem", "forgectl"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			existing := filepath.Join(append([]string{tmp}, tc.existing...)...)
			if err := os.MkdirAll(filepath.Join(existing, ".git"), 0o750); err != nil {
				t.Fatal(err)
			}
			fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
				if name == "git" && len(args) >= 4 && args[2] == "remote" {
					return origin, nil
				}
				return "", nil
			}}
			c := &Client{Dir: tmp, run: fake, gitBin: "git", wings: table}

			// The wing-routed call goes through Clone (which consults the
			// table); the host-tree-routed call is the --wing "" override.
			var dest string
			if tc.existing[0] == "github.com" {
				dest, err = c.Clone(context.Background(), r)
			} else {
				dest, err = c.CloneInto(context.Background(), r, "")
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if dest != existing {
				t.Errorf("dest = %q; want the EXISTING checkout %q — the stdout contract is one usable path", dest, existing)
			}
			for _, call := range fake.Calls {
				if strings.Contains(strings.Join(call.Args, " "), "clone") {
					t.Errorf("a clone ran despite the repo already being checked out: %q %v", call.Name, call.Args)
				}
			}
		})
	}
}

// TestClone_DifferentRepoAtOtherLayoutDoesNotSuppress is why the probe is
// originMatches and not a bare os.Stat. A same-NAMED but different repo
// sitting at the other path must not suppress a legitimate clone — that would
// be a silent wrong answer, which is worse than the duplicate.
func TestClone_DifferentRepoAtOtherLayoutDoesNotSuppress(t *testing.T) {
	tmp := t.TempDir()
	table, err := ResolveWings("github.com", []Wing{
		{Name: "cadence-ecosystem", Repos: []string{"cameronsjo/forgectl"}},
	})
	if err != nil {
		t.Fatalf("ResolveWings: %v", err)
	}
	// Someone else's forgectl, checked out in the host tree.
	decoy := filepath.Join(tmp, "github.com", "cameronsjo", "forgectl")
	if err := os.MkdirAll(filepath.Join(decoy, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "git" && len(args) >= 4 && args[2] == "remote" {
			return "git@github.com:someone-else/forgectl.git", nil
		}
		return "", nil
	}}
	c := &Client{Dir: tmp, run: fake, gitBin: "git", wings: table}

	dest, err := c.Clone(context.Background(), Repo{
		Host: "github.com", Owner: "cameronsjo", Name: "forgectl",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := filepath.Join(tmp, "cadence-ecosystem", "forgectl"); dest != want {
		t.Errorf("dest = %q; want %q — a DIFFERENT repo must not suppress the clone", dest, want)
	}
	var cloned bool
	for _, call := range fake.Calls {
		if strings.Contains(strings.Join(call.Args, " "), "clone") {
			cloned = true
		}
	}
	if !cloned {
		t.Error("no clone ran; a different repo at the other path suppressed a legitimate clone")
	}
}

// TestDiscover_FindsWingMembers is the acceptance test in miniature. The wing
// directory here is ITSELF a checkout — the cadence-ecosystem wing is, on the
// real estate — which is exactly the case that used to hide every member: the
// isGitRepo shortcut took the flat branch and never looked inside. Ordering
// the wing pass first is the whole fix, so this test must keep the
// wing-is-also-a-repo shape or it stops covering the defect.
func TestDiscover_FindsWingMembers(t *testing.T) {
	tmp := t.TempDir()
	mk := func(parts ...string) {
		if err := os.MkdirAll(filepath.Join(append(append([]string{tmp}, parts...), ".git")...), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	mk("cadence-ecosystem")             // the wing is a repo itself
	mk("cadence-ecosystem", "forgectl") // …and holds members
	mk("cadence-ecosystem", "cadence")
	mk("mcp", "some-mcp")                     // a wing that is NOT a repo
	mk("github.com", "cameronsjo", "quickmd") // host tree, untouched
	mk("flat-repo")                           // legacy flat, untouched
	if err := os.MkdirAll(filepath.Join(tmp, "notes"), 0o750); err != nil {
		t.Fatal(err) // a plain non-git dir stays a flat project
	}

	c := &Client{Dir: tmp, run: &exec.FakeRunner{}, gitBin: ""}
	projs, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	got := make(map[string]string, len(projs))
	for _, p := range projs {
		got[p.Name] = p.Dir
	}
	for name, want := range map[string]string{
		"forgectl":          filepath.Join(tmp, "cadence-ecosystem", "forgectl"),
		"cadence":           filepath.Join(tmp, "cadence-ecosystem", "cadence"),
		"some-mcp":          filepath.Join(tmp, "mcp", "some-mcp"),
		"quickmd":           filepath.Join(tmp, "github.com", "cameronsjo", "quickmd"),
		"flat-repo":         filepath.Join(tmp, "flat-repo"),
		"notes":             filepath.Join(tmp, "notes"),
		"cadence-ecosystem": filepath.Join(tmp, "cadence-ecosystem"),
	} {
		if got[name] != want {
			t.Errorf("%s = %q; want %q", name, got[name], want)
		}
	}
	// The wing that is NOT itself a repo must not be listed as a project.
	if dir, ok := got["mcp"]; ok {
		t.Errorf("a non-repo wing directory was listed as a project: %q", dir)
	}
}

// TestDiscover_HostTreesAreNotMistakenForWings pins the boundary between the
// two layouts. A host bucket's children are OWNER directories with no .git of
// their own, so the wing pass scores zero on them and the host pass still
// runs. Verified against the live estate, where github.com and git.sjo.lol
// both score 0 while all seven wings score 4–28.
func TestDiscover_HostTreesAreNotMistakenForWings(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "github.com", "cameronsjo", "quickmd", ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	if got := discoverWingCandidates(filepath.Join(tmp, "github.com")); len(got) != 0 {
		t.Errorf("a host bucket scored %d wing members: %+v", len(got), got)
	}
	c := &Client{Dir: tmp, run: &exec.FakeRunner{}, gitBin: ""}
	projs, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(projs) != 1 || projs[0].Name != "quickmd" {
		t.Errorf("host tree discovery = %+v; want just quickmd", projs)
	}
}

// TestPlacement_RejectsDotLeadingRepoNames is the "re-armed through a
// different door" case. ParseCloneTarget's URL branch takes the last two path
// segments with no charset check of its own, so a clone target can carry a
// name validPathSegment happily accepts. A leaf named ".git" makes isGitRepo
// report the PARENT directory as a repo, hiding every sibling from the
// inventory — the exact defect this branch exists to fix.
func TestPlacement_RejectsDotLeadingRepoNames(t *testing.T) {
	for _, tc := range []struct {
		what string
		r    Repo
		wing string
	}{
		{".git as the name, host tree", Repo{Host: "github.com", Owner: "o", Name: ".git"}, ""},
		{".git as the name, wing", Repo{Host: "github.com", Owner: "o", Name: ".git"}, "w"},
		{".bare as the name", Repo{Host: "github.com", Owner: "o", Name: ".bare"}, ""},
		{".git as the owner", Repo{Host: "github.com", Owner: ".git", Name: "n"}, ""},
		{"dot-leading owner on the WING path", Repo{Host: "github.com", Owner: ".hidden", Name: "n"}, "w"},
		{"colon in the name", Repo{Host: "github.com", Owner: "o", Name: "a:b"}, ""},
		{"ANSI escape in the name", Repo{Host: "github.com", Owner: "o", Name: "a\x1b[31mb"}, ""},
		{"over-long name", Repo{Host: "github.com", Owner: "o", Name: strings.Repeat("a", 101)}, ""},
	} {
		t.Run(tc.what, func(t *testing.T) {
			// validPathSegment — the guard this used to use — accepts most of
			// these, which is why the stronger one is load-bearing.
			if _, err := Placement("/base", tc.r, tc.wing); err == nil {
				t.Error("accepted a repo name/owner that must not become a directory")
			}
		})
	}
}

// TestPlacement_WingSegmentTakesTheHostGuard: the --wing flag is a second
// entry point into the depth-1 namespace and does not go through ResolveWings,
// so Placement itself has to hold the line.
func TestPlacement_WingSegmentTakesTheHostGuard(t *testing.T) {
	r := Repo{Host: "github.com", Owner: "o", Name: "n"}
	for _, wing := range []string{".hidden", "wing:8443", "wing space", "wíng", strings.Repeat("a", 254)} {
		t.Run(wing, func(t *testing.T) {
			if _, err := Placement("/base", r, wing); err == nil {
				t.Errorf("wing %q was accepted as a directory under the projects root", wing)
			}
		})
	}
}

// TestClone_OverrideWingStillFindsTheConfiguredWing covers the three-candidate
// case. `--wing foo` on a repo the table files under `bar` makes foo the dest,
// the host tree one alternative, and bar — where the repo most likely actually
// is — the other. Probing only "the other one" misses exactly the case an
// override creates.
func TestClone_OverrideWingStillFindsTheConfiguredWing(t *testing.T) {
	tmp := t.TempDir()
	table, err := ResolveWings("github.com", []Wing{
		{Name: "bar", Repos: []string{"cameronsjo/forgectl"}},
	})
	if err != nil {
		t.Fatalf("ResolveWings: %v", err)
	}
	existing := filepath.Join(tmp, "bar", "forgectl")
	if err := os.MkdirAll(filepath.Join(existing, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "git" && len(args) >= 4 && args[2] == "remote" {
			return "git@github.com:cameronsjo/forgectl.git", nil
		}
		return "", nil
	}}
	c := &Client{Dir: tmp, run: fake, gitBin: "git", wings: table}

	dest, err := c.CloneInto(context.Background(), Repo{
		Host: "github.com", Owner: "cameronsjo", Name: "forgectl",
	}, "foo")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dest != existing {
		t.Errorf("dest = %q; want the existing checkout at %q", dest, existing)
	}
	for _, call := range fake.Calls {
		if strings.Contains(strings.Join(call.Args, " "), "clone") {
			t.Error("a duplicate clone ran despite the repo living in its configured wing")
		}
	}
}

// TestClone_ProbeDoesNotWalkUpToAnAncestorRepo: `git -C <dir>` walks UP to
// find a repository, so probing a same-named NON-repo directory nested under
// an ancestor checkout would return that ancestor's origin — and a match there
// would skip a legitimate clone and hand back the wrong path.
func TestClone_ProbeDoesNotWalkUpToAnAncestorRepo(t *testing.T) {
	tmp := t.TempDir()
	table, err := ResolveWings("github.com", []Wing{
		{Name: "wing", Repos: []string{"cameronsjo/forgectl"}},
	})
	if err != nil {
		t.Fatalf("ResolveWings: %v", err)
	}
	// A plain directory at the host-tree path, with NO .git of its own.
	decoy := filepath.Join(tmp, "github.com", "cameronsjo", "forgectl")
	if err := os.MkdirAll(decoy, 0o750); err != nil {
		t.Fatal(err)
	}
	// A runner that answers every `remote get-url` with the matching origin —
	// i.e. the worst case, an ancestor repo that really is this repo.
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "git" && len(args) >= 4 && args[2] == "remote" {
			return "git@github.com:cameronsjo/forgectl.git", nil
		}
		return "", nil
	}}
	c := &Client{Dir: tmp, run: fake, gitBin: "git", wings: table}

	dest, err := c.Clone(context.Background(), Repo{
		Host: "github.com", Owner: "cameronsjo", Name: "forgectl",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dest == decoy {
		t.Fatalf("the probe accepted a non-repo directory at %q; git walked up to an ancestor's origin", decoy)
	}
	if want := filepath.Join(tmp, "wing", "forgectl"); dest != want {
		t.Errorf("dest = %q; want %q", dest, want)
	}
}

// TestDiscover_WingNamedAfterAHostStillShowsTheHostTree: all three discovery
// passes run, none short-circuits the others. A wing whose name collided with
// a host would otherwise hide that host's entire tree behind an early return.
func TestDiscover_WingNamedAfterAHostStillShowsTheHostTree(t *testing.T) {
	tmp := t.TempDir()
	// git.sjo.lol is simultaneously a host bucket (owner/repo below it) and,
	// structurally, a wing (a direct child that is a repo).
	for _, p := range [][]string{
		{"git.sjo.lol", "wing-shaped-member"},
		{"git.sjo.lol", "cameron", "real-host-tree-repo"},
	} {
		if err := os.MkdirAll(filepath.Join(append(append([]string{tmp}, p...), ".git")...), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	c := &Client{Dir: tmp, run: &exec.FakeRunner{}, gitBin: ""}
	projs, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	got := make(map[string]bool, len(projs))
	for _, p := range projs {
		got[p.Name] = true
	}
	for _, want := range []string{"wing-shaped-member", "real-host-tree-repo"} {
		if !got[want] {
			t.Errorf("%q missing; one pass short-circuited the other (got %v)", want, got)
		}
	}
}
