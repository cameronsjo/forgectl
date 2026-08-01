package projects

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

	// Local-only, git-inspected dir with no origin: cloned, TreeOK.
	if r, ok := findRepo(repos, "", "scratch"); !ok || !r.Cloned || r.Status.State != TreeOK {
		t.Errorf("local-only scratch should be present, cloned, and TreeOK: %+v (found=%v)", r, ok)
	}
	// Local-only, non-git dir: cloned, TreeNotRepo — must never read as clean.
	if r, ok := findRepo(repos, "", "notes"); !ok || !r.Cloned || r.Status.State != TreeNotRepo || r.Status.Label() != "" {
		t.Errorf("local-only notes (non-repo) should be present, cloned, TreeNotRepo, empty Label: %+v (found=%v)", r, ok)
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

// TestInventory_NonRepoDirIsNotReportedClean pins the headline regression at
// the Inventory boundary: a non-git directory sitting under Dir (e.g. a
// .mypy_cache left behind by tooling) must surface with an empty Label(),
// never "[clean]".
func TestInventory_NonRepoDirIsNotReportedClean(t *testing.T) {
	tmp := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmp, ".mypy_cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			switch name {
			case "gh":
				return "[]", nil
			case "tea":
				return "owner\tname\ttype\tssh\n", nil
			}
			return "", nil
		},
	}
	c := &Client{Dir: tmp, run: fake}

	repos, _, err := c.Inventory(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, ok := findRepo(repos, "", ".mypy_cache")
	if !ok {
		t.Fatalf(".mypy_cache not found in inventory: %+v", repos)
	}
	if r.Status.State != TreeNotRepo {
		t.Errorf(".mypy_cache Status.State = %q, want %q", r.Status.State, TreeNotRepo)
	}
	if r.Status.Label() != "" {
		t.Errorf(".mypy_cache Label() = %q, want empty — a non-repo dir must never read as clean", r.Status.Label())
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
		want := canonicalDest(tmp, "github", "cameronsjo", "newgh")
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

// TestListOrg_RejectsUnsafeLogin guards the caller-supplied `--org` value: an
// empty, traversal, or leading-'-' login must be refused before it becomes a
// `gh` argv (a '-'-leading value would be read as a flag, not a positional).
func TestListOrg_RejectsUnsafeLogin(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := &Client{Dir: t.TempDir(), run: fake}
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
	c := &Client{Dir: t.TempDir(), run: fake}
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
	c := &Client{Dir: tmp, run: fake}
	for _, name := range []string{"", ".", "..", "../escape", "a/b"} {
		if _, err := c.Clone(context.Background(), Repo{Host: "gitea", Owner: "cameron", Name: name, SSHURL: "ssh://x"}); err == nil {
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
	c := &Client{Dir: tmp, run: fake}
	cases := []struct{ host, owner string }{
		{"../escape", "cameron"},
		{"gitea", "../escape"},
		{"", "cameron"},
		{"gitea", ""},
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
	giteaDest := canonicalDest(tmp, "gitea", "cameron", "homeclaw")
	if err := os.MkdirAll(giteaDest, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &exec.FakeRunner{RunFunc: originGitea}
	c := &Client{Dir: tmp, run: fake}

	dest, err := c.Clone(context.Background(), Repo{
		Host: "github", Owner: "cameronsjo", Name: "homeclaw",
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
	dest := canonicalDest(tmp, "gitea", "cameron", "homeclaw")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if len(args) >= 5 && args[2] == "remote" && args[3] == "get-url" {
			return "ssh://git@git.sjo.lol:222/cameron/somethingelse.git", nil
		}
		return "", nil
	}}
	c := &Client{Dir: tmp, run: fake}

	_, err := c.Clone(context.Background(), Repo{
		Host: "gitea", Owner: "cameron", Name: "homeclaw",
		SSHURL: "ssh://git@git.sjo.lol:222/cameron/homeclaw.git",
	})
	if err == nil {
		t.Fatal("expected an origin-mismatch error, got nil (would open the wrong repo)")
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
	dest := canonicalDest(tmp, "gitea", "cameron", "homeclaw")
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

// mkCanonicalGitDir creates base/host/owner/name with a .git marker — a
// canonical-layout clone — and returns its full path.
func mkCanonicalGitDir(t *testing.T, base, host, owner, name string) string {
	t.Helper()
	dir := canonicalDest(base, host, owner, name)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
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
	canonDir := mkCanonicalGitDir(t, tmp, "github", "cameronsjo", "forgectl")
	mkGitDir(t, tmp, "homeclaw") // legacy flat clone, still on disk
	if err := os.Mkdir(filepath.Join(tmp, "notes"), 0o755); err != nil {
		t.Fatal(err) // plain non-git dir, no canonical structure beneath it
	}

	c := &Client{Dir: tmp, run: &exec.FakeRunner{}}
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
	// "github" (the host bucket) must not itself appear as a project — it was
	// walked into, not treated as a flat clone.
	if _, ok := byName["github"]; ok {
		t.Errorf("host bucket %q leaked into the project list: %+v", "github", projs)
	}
}

// TestDiscover_CanonicalHostBucketMultipleOwnersAndRepos exercises the walk
// beyond a single owner/repo pair — Inventory/pick/list all depend on every
// canonical clone surfacing, not just the first found.
func TestDiscover_CanonicalHostBucketMultipleOwnersAndRepos(t *testing.T) {
	tmp := t.TempDir()
	mkCanonicalGitDir(t, tmp, "gitea", "cameron", "homeclaw")
	mkCanonicalGitDir(t, tmp, "gitea", "cameron", "forgectl")
	mkCanonicalGitDir(t, tmp, "gitea", "otherowner", "sidecar")

	c := &Client{Dir: tmp, run: &exec.FakeRunner{}}
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
// TestDiscover_FanOutIsDeterministic guards against the fan-out phase turning
// Discover's result order into a race: SliceStable sorts on (Name, Dir), so
// two runs over the same fixture must return the identical ordered list even
// though the status-inspection goroutines complete in arbitrary order.
func TestDiscover_FanOutIsDeterministic(t *testing.T) {
	tmp := t.TempDir()
	for i := 0; i < 30; i++ {
		name := fmt.Sprintf("repo%02d", i)
		if i%3 == 0 {
			// Every third entry is a plain non-git dir.
			if err := os.Mkdir(filepath.Join(tmp, name), 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		mkGitDir(t, tmp, name)
	}

	c := &Client{Dir: tmp, run: &exec.FakeRunner{}}

	first, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("first Discover: %v", err)
	}
	second, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("second Discover: %v", err)
	}

	if len(first) != len(second) {
		t.Fatalf("result length differs: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Name != second[i].Name || first[i].Dir != second[i].Dir {
			t.Errorf("order differs at index %d: (%q,%q) vs (%q,%q)",
				i, first[i].Name, first[i].Dir, second[i].Name, second[i].Dir)
		}
	}
}

// TestDiscover_BoundsConcurrency proves the fan-out is actually concurrent
// (peak > 1) and bounded (peak <= discoverConcurrency()) — the whole point of
// the semaphore.
func TestDiscover_BoundsConcurrency(t *testing.T) {
	tmp := t.TempDir()
	for i := 0; i < 30; i++ {
		mkGitDir(t, tmp, fmt.Sprintf("repo%02d", i))
	}

	var inFlight, peak int64
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			cur := atomic.AddInt64(&inFlight, 1)
			for {
				p := atomic.LoadInt64(&peak)
				if cur <= p || atomic.CompareAndSwapInt64(&peak, p, cur) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt64(&inFlight, -1)
			return "", nil
		},
	}
	c := &Client{Dir: tmp, run: fake}

	if _, err := c.Discover(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := atomic.LoadInt64(&peak)
	if got <= 1 {
		t.Errorf("peak concurrent git calls = %d, want > 1 (no fan-out observed)", got)
	}
	if want := int64(discoverConcurrency()); got > want {
		t.Errorf("peak concurrent git calls = %d, want <= discoverConcurrency() (%d)", got, want)
	}
}

// TestFanOut_AppliesFnAndPreservesOrder exercises the fanOut generic helper
// in isolation, with no subprocess or filesystem dependency — Discover and
// localRepos only prove it indirectly through their own behavior. Each
// result must land at its item's index regardless of goroutine completion
// order, so fn deliberately sleeps longer for earlier items (most likely to
// finish last if index-tracking were broken).
func TestFanOut_AppliesFnAndPreservesOrder(t *testing.T) {
	items := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	got := fanOut(items, func(i int) int {
		time.Sleep(time.Duration(10-i) * time.Millisecond)
		return i * i
	})
	if len(got) != len(items) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(items))
	}
	for i, v := range got {
		want := i * i
		if v != want {
			t.Errorf("got[%d] = %d, want %d", i, v, want)
		}
	}
}

// TestFanOut_EmptyInput_ReturnsEmptySlice guards the zero-candidate path
// (e.g. an empty projects directory) against a panic or a nil-vs-empty
// mismatch propagating up to callers that range over the result.
func TestFanOut_EmptyInput_ReturnsEmptySlice(t *testing.T) {
	got := fanOut([]string{}, func(s string) int { return len(s) })
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}

// TestFanOut_SingleItem_NoWorkerPoolOverhead is the boundary companion to the
// concurrency-bound tests: one item must not deadlock or panic on the
// buffered semaphore of size discoverConcurrency().
func TestFanOut_SingleItem_NoWorkerPoolOverhead(t *testing.T) {
	got := fanOut([]int{7}, func(i int) int { return i + 1 })
	if len(got) != 1 || got[0] != 8 {
		t.Errorf("got = %v, want [8]", got)
	}
}

func TestCanonicalDest_LowercasesAndMirrorsKey(t *testing.T) {
	got := canonicalDest("/base", "GitHub", "CameronSjo", "Forgectl")
	want := filepath.Join("/base", "github", "cameronsjo", "forgectl")
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
	good := []string{"github", "cameronsjo", "git.sjo.lol", "forge-ctl"}
	for _, s := range good {
		if !validPathSegment(s) {
			t.Errorf("validPathSegment(%q) = false, want true", s)
		}
	}
}
