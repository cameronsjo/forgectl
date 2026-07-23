package projects

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// worktreeRunFunc is a NON-MASKING FakeRunner body: every git/gh subcommand the
// worktree flow issues is mapped explicitly, and anything else returns an
// error. A lazy `return "", nil` default would make the default-branch and
// fallback assertions pass vacuously (an unmapped `remote show` would look like
// "no HEAD line" rather than a test-wiring gap), so the default is a hard fail.
//
//   - headBranch: what `remote show origin` reports as `HEAD branch:` ("" → no
//     HEAD line, so defaultBranch must fall back to "main").
//   - failFirstAdd: the first `worktree add` (no `-b`) errors, forcing the
//     origin/<branch> -b <branch> fallback path.
func worktreeRunFunc(headBranch string, failFirstAdd bool) func(string, []string) (string, error) {
	return func(name string, args []string) (string, error) {
		joined := strings.Join(args, " ")
		switch name {
		case "gh":
			if len(args) >= 2 && args[0] == "repo" && args[1] == "clone" {
				return "", nil
			}
		case "git":
			switch {
			case strings.Contains(joined, "config remote.origin.fetch"):
				return "", nil
			case strings.Contains(joined, "fetch origin"):
				return "", nil
			case strings.Contains(joined, "remote show origin"):
				if headBranch != "" {
					return "* remote origin\n  HEAD branch: " + headBranch + "\n  Remote branches:\n", nil
				}
				return "* remote origin\n  Fetch URL: git@example.com:x/y.git\n", nil
			case strings.Contains(joined, "worktree add"):
				// The fallback form carries "-b"; the first attempt does not.
				if failFirstAdd && !strings.Contains(joined, "-b") {
					return "", errors.New("fatal: invalid reference: branch")
				}
				return "", nil
			case strings.Contains(joined, "clone --bare"):
				return "", nil
			}
		}
		return "", fmt.Errorf("unexpected command: %s %s", name, joined)
	}
}

// findCall returns the first recorded Call satisfying pred.
func findCall(calls []exec.Call, pred func(exec.Call) bool) (exec.Call, bool) {
	for _, c := range calls {
		if pred(c) {
			return c, true
		}
	}
	return exec.Call{}, false
}

// hasCall reports whether any recorded Call's argv, joined, contains substr.
func hasCall(calls []exec.Call, name, substr string) bool {
	_, ok := findCall(calls, func(c exec.Call) bool {
		return c.Name == name && strings.Contains(strings.Join(c.Args, " "), substr)
	})
	return ok
}

func TestWorktree_CreatesBareAndWorktree(t *testing.T) {
	tmp := t.TempDir()
	fake := &exec.FakeRunner{RunFunc: worktreeRunFunc("", false)}
	c := &Client{Dir: tmp, run: fake}

	got, err := c.Worktree(context.Background(), Repo{
		Host: "github", Owner: "cameronsjo", Name: "forgectl",
	}, "mybranch")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	base := canonicalDest(tmp, "github", "cameronsjo", "forgectl")
	bareDir := filepath.Join(base, ".bare")

	// Bare clone landed at <base>/.bare via gh, with --bare forwarded.
	if !hasCall(fake.Calls, "gh", "repo clone cameronsjo/forgectl "+bareDir) {
		t.Errorf("expected `gh repo clone cameronsjo/forgectl %s`, calls: %+v", bareDir, fake.Calls)
	}
	if !hasCall(fake.Calls, "gh", "--bare") {
		t.Errorf("bare clone must forward --bare, calls: %+v", fake.Calls)
	}

	// .git pointer file written with the exact gitdir line.
	dotGit, err := os.ReadFile(filepath.Join(base, ".git"))
	if err != nil {
		t.Fatalf("reading base/.git: %v", err)
	}
	if string(dotGit) != "gitdir: ./.bare\n" {
		t.Errorf("base/.git = %q; want %q", string(dotGit), "gitdir: ./.bare\n")
	}

	if !hasCall(fake.Calls, "git", "config remote.origin.fetch +refs/heads/*:refs/remotes/origin/*") {
		t.Errorf("expected the widening fetch refspec config call, calls: %+v", fake.Calls)
	}
	if !hasCall(fake.Calls, "git", "-C "+bareDir+" fetch origin") {
		t.Errorf("expected `git -C %s fetch origin`, calls: %+v", bareDir, fake.Calls)
	}

	wantDir := filepath.Join(base, "mybranch")
	if !hasCall(fake.Calls, "git", "worktree add "+wantDir+" mybranch") {
		t.Errorf("expected `worktree add %s mybranch`, calls: %+v", wantDir, fake.Calls)
	}
	if got != wantDir {
		t.Errorf("returned path = %q; want %q", got, wantDir)
	}
}

func TestWorktree_DetectsDefaultBranch(t *testing.T) {
	tmp := t.TempDir()
	fake := &exec.FakeRunner{RunFunc: worktreeRunFunc("main", false)}
	c := &Client{Dir: tmp, run: fake}

	// No branch arg → defaultBranch parses `remote show origin`'s HEAD line.
	got, err := c.Worktree(context.Background(), Repo{
		Host: "github", Owner: "cameronsjo", Name: "forgectl",
	}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	base := canonicalDest(tmp, "github", "cameronsjo", "forgectl")
	wantDir := filepath.Join(base, "main")
	// The add must use the DETECTED branch, not a hardcoded fallback — proves
	// detection actually ran and was consulted.
	if !hasCall(fake.Calls, "git", "worktree add "+wantDir+" main") {
		t.Errorf("expected worktree add on the detected branch main, calls: %+v", fake.Calls)
	}
	if !hasCall(fake.Calls, "git", "remote show origin") {
		t.Errorf("default-branch detection must query `remote show origin`, calls: %+v", fake.Calls)
	}
	if got != wantDir {
		t.Errorf("returned path = %q; want %q", got, wantDir)
	}
}

func TestWorktree_DefaultBranchFallback(t *testing.T) {
	tmp := t.TempDir()
	// remote show origin output carries no HEAD line → fall back to "main".
	fake := &exec.FakeRunner{RunFunc: worktreeRunFunc("", false)}
	c := &Client{Dir: tmp, run: fake}

	if _, err := c.Worktree(context.Background(), Repo{
		Host: "github", Owner: "cameronsjo", Name: "forgectl",
	}, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	base := canonicalDest(tmp, "github", "cameronsjo", "forgectl")
	wantDir := filepath.Join(base, "main")
	if !hasCall(fake.Calls, "git", "worktree add "+wantDir+" main") {
		t.Errorf("expected fallback to main, calls: %+v", fake.Calls)
	}
}

func TestWorktree_WorktreeAddFallback(t *testing.T) {
	tmp := t.TempDir()
	fake := &exec.FakeRunner{RunFunc: worktreeRunFunc("", true)} // first add fails
	c := &Client{Dir: tmp, run: fake}

	if _, err := c.Worktree(context.Background(), Repo{
		Host: "github", Owner: "cameronsjo", Name: "forgectl",
	}, "feature/foo"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	base := canonicalDest(tmp, "github", "cameronsjo", "forgectl")
	wantDir := filepath.Join(base, "feature", "foo")
	if !hasCall(fake.Calls, "git", "worktree add "+wantDir+" origin/feature/foo -b feature/foo") {
		t.Errorf("expected the origin/<branch> -b <branch> fallback, calls: %+v", fake.Calls)
	}
}

func TestWorktree_RefusesExistingDir(t *testing.T) {
	tmp := t.TempDir()
	base := canonicalDest(tmp, "github", "cameronsjo", "forgectl")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := &exec.FakeRunner{RunFunc: worktreeRunFunc("", false)}
	c := &Client{Dir: tmp, run: fake}

	if _, err := c.Worktree(context.Background(), Repo{
		Host: "github", Owner: "cameronsjo", Name: "forgectl",
	}, "main"); err == nil {
		t.Fatal("expected an error when base already exists, got nil")
	}
	// Refuse-if-exists must short-circuit BEFORE any clone runs.
	if hasCall(fake.Calls, "gh", "clone") || hasCall(fake.Calls, "git", "clone") {
		t.Errorf("no clone should run when the base already exists; calls: %+v", fake.Calls)
	}
}

// TestWorktree_RefusesSymlinkedBase is the TOCTOU regression: a symlink
// pre-placed at the base path must be refused by the atomic os.Mkdir (which
// never follows a symlink) BEFORE any clone runs, and the .git pointer must
// never be written through it into the symlink's target.
func TestWorktree_RefusesSymlinkedBase(t *testing.T) {
	tmp := t.TempDir()
	base := canonicalDest(tmp, "github", "cameronsjo", "forgectl")
	decoy := filepath.Join(tmp, "decoy")
	if err := os.MkdirAll(decoy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(decoy, base); err != nil {
		t.Fatal(err)
	}
	fake := &exec.FakeRunner{RunFunc: worktreeRunFunc("", false)}
	c := &Client{Dir: tmp, run: fake}

	if _, err := c.Worktree(context.Background(), Repo{
		Host: "github", Owner: "cameronsjo", Name: "forgectl",
	}, "main"); err == nil {
		t.Fatal("expected a refusal when base is a pre-placed symlink, got nil")
	}
	if hasCall(fake.Calls, "gh", "clone") || hasCall(fake.Calls, "git", "clone") || hasCall(fake.Calls, "git", "worktree add") {
		t.Errorf("no clone/worktree command should run when base is a symlink; calls: %+v", fake.Calls)
	}
	// The .git pointer must NOT have been written through the symlink.
	if _, err := os.Lstat(filepath.Join(decoy, ".git")); err == nil {
		t.Error("the .git pointer was written through the symlink into the decoy target")
	}
}

func TestWorktree_RejectsUnsafeSegment(t *testing.T) {
	tmp := t.TempDir()
	fake := &exec.FakeRunner{RunFunc: worktreeRunFunc("", false)}
	c := &Client{Dir: tmp, run: fake}
	cases := []Repo{
		{Host: "../escape", Owner: "cameronsjo", Name: "forgectl"},
		{Host: "github", Owner: "..", Name: "forgectl"},
		{Host: "github", Owner: "cameronsjo", Name: ""},
		{Host: "github", Owner: "cameronsjo", Name: "a/b"},
	}
	for _, r := range cases {
		if _, err := c.Worktree(context.Background(), r, "main"); err == nil {
			t.Errorf("Worktree(%+v) should reject an unsafe path segment, got nil", r)
		}
	}
	if len(fake.Calls) != 0 {
		t.Errorf("no command should run for an unsafe segment; calls: %+v", fake.Calls)
	}
}

func TestWorktree_RejectsUnsafeBranch(t *testing.T) {
	for _, branch := range []string{"-x", "..", "a/../b", "feat/-x", "/abs", "a/"} {
		tmp := t.TempDir()
		fake := &exec.FakeRunner{RunFunc: worktreeRunFunc("", false)}
		c := &Client{Dir: tmp, run: fake}
		if _, err := c.Worktree(context.Background(), Repo{
			Host: "github", Owner: "cameronsjo", Name: "forgectl",
		}, branch); err == nil {
			t.Errorf("Worktree(branch=%q) should reject an unsafe branch, got nil", branch)
		}
		// The branch is validated before `worktree add`, so no add call fires.
		if hasCall(fake.Calls, "git", "worktree add") {
			t.Errorf("no worktree add should run for unsafe branch %q; calls: %+v", branch, fake.Calls)
		}
	}
}

func TestWorktree_LowercasesPath(t *testing.T) {
	tmp := t.TempDir()
	fake := &exec.FakeRunner{RunFunc: worktreeRunFunc("", false)}
	c := &Client{Dir: tmp, run: fake}

	// Uppercase host/owner/name (non-github so it takes the SSH bare-clone path)
	// must land at a lowercased canonical dest, mirroring Repo.Key().
	got, err := c.Worktree(context.Background(), Repo{
		Host: "Gitea", Owner: "Cameron", Name: "Homeclaw",
		SSHURL: "ssh://git@git.sjo.lol:222/cameron/homeclaw.git",
	}, "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(canonicalDest(tmp, "Gitea", "Cameron", "Homeclaw"), "main")
	if got != want {
		t.Errorf("returned path = %q; want lowercased %q", got, want)
	}
	if !strings.Contains(got, filepath.Join("gitea", "cameron", "homeclaw")) {
		t.Errorf("dest not lowercased: %q", got)
	}
}

func TestWorktree_NonGithubUsesBareGitClone(t *testing.T) {
	tmp := t.TempDir()
	fake := &exec.FakeRunner{RunFunc: worktreeRunFunc("", false)}
	c := &Client{Dir: tmp, run: fake}

	url := "ssh://git@git.sjo.lol:222/cameron/homeclaw.git"
	if _, err := c.Worktree(context.Background(), Repo{
		Host: "gitea", Owner: "cameron", Name: "homeclaw", SSHURL: url,
	}, "main"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	call, ok := findCall(fake.Calls, func(cc exec.Call) bool {
		return cc.Name == "git" && strings.Contains(strings.Join(cc.Args, " "), "clone --bare")
	})
	if !ok {
		t.Fatalf("expected a `git clone --bare`, calls: %+v", fake.Calls)
	}
	joined := strings.Join(call.Args, " ")
	// The server-controlled URL must ride the hardened transport guards.
	if !strings.Contains(joined, "protocol.ext.allow=never") || !strings.Contains(joined, "protocol.fd.allow=never") {
		t.Errorf("bare git clone must carry the ext/fd protocol guards: %v", call.Args)
	}
	if !strings.Contains(joined, "-- "+url) {
		t.Errorf("bare git clone must `--`-terminate before the URL: %v", call.Args)
	}
	// No gh path for a non-github host.
	if hasCall(fake.Calls, "gh", "clone") {
		t.Errorf("a non-github host must not clone through gh; calls: %+v", fake.Calls)
	}
}

func TestValidBranch(t *testing.T) {
	good := []string{"main", "feature/foo", "release/1.2"}
	for _, b := range good {
		if !validBranch(b) {
			t.Errorf("validBranch(%q) = false, want true", b)
		}
	}
	bad := []string{"", "..", "a/../b", "-x", "feat/-x", "/abs", "a/"}
	for _, b := range bad {
		if validBranch(b) {
			t.Errorf("validBranch(%q) = true, want false", b)
		}
	}
}
