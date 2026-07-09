package pr

// Test plan for session.go
//
// Prepare (Classification: ops layer, hostile-input git dispatch)
//   [x] Dry-run: resolves head via `gh pr view` and creates NOTHING
//       (no git clone, no tmux, no breadcrumb) — the only Runner call is the
//       read-only gh pr view
//   [x] Dry-run: Session carries the resolved head metadata
//   [x] Real: `gh pr view` argv (positional N, --repo slug, --json fields)
//   [x] Real: `git clone` argv uses --branch <headRef> and a -- terminator
//   [x] Real: writes a breadcrumb + allowlist; workspace exists
//   [x] Incomplete ref (bare, unresolved) is refused before any Runner call

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

const ghViewJSON = `{"headRefName":"feature-x","headRefOid":"deadbeef","headRepositoryOwner":{"login":"contributor"},"headRepository":{"name":"forgectl"}}`

func ghViewRunner() *exec.FakeRunner {
	return &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "gh" {
				return ghViewJSON, nil
			}
			return "", nil // git clone / tmux succeed as no-ops
		},
	}
}

func testClient(t *testing.T, fake *exec.FakeRunner) *Client {
	t.Helper()
	return New(fake,
		WithSessionsDir(t.TempDir()),
		WithApprover(func(string) (bool, error) { return false, nil }),
		WithTTYCheck(func() bool { return false }),
	)
}

func findCall(calls []exec.Call, name string) (exec.Call, bool) {
	for _, c := range calls {
		if c.Name == name {
			return c, true
		}
	}
	return exec.Call{}, false
}

func TestPrepare_DryRunCreatesNothing(t *testing.T) {
	fake := ghViewRunner()
	c := testClient(t, fake)
	ref := Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}

	sess, err := c.Prepare(context.Background(), ref, PrepareOpts{DryRun: true})
	if err != nil {
		t.Fatalf("Prepare dry-run: %v", err)
	}
	if sess.Workspace != "" || sess.Path != "" {
		t.Errorf("dry-run created state: workspace=%q path=%q", sess.Workspace, sess.Path)
	}
	if sess.HeadRef != "feature-x" || sess.HeadOid != "deadbeef" || sess.HeadRepo != "contributor/forgectl" {
		t.Errorf("head metadata not resolved: %+v", sess)
	}
	// Only the read-only gh pr view should have run.
	if len(fake.Calls) != 1 || fake.Calls[0].Name != "gh" {
		t.Fatalf("dry-run should issue exactly one gh call; got %+v", fake.Calls)
	}
	if _, ok := findCall(fake.Calls, "git"); ok {
		t.Error("dry-run must not run git")
	}
	if _, ok := findCall(fake.Calls, "tmux"); ok {
		t.Error("dry-run must not run tmux")
	}
	// No breadcrumb written.
	entries, _ := os.ReadDir(c.SessionsDir())
	if len(entries) != 0 {
		t.Errorf("dry-run wrote breadcrumbs: %v", entries)
	}
}

func TestPrepare_RealDispatch(t *testing.T) {
	fake := ghViewRunner()
	c := testClient(t, fake)
	ref := Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}

	sess, err := c.Prepare(context.Background(), ref, PrepareOpts{Agent: "claude"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(sess.Workspace) })

	// gh pr view argv.
	gh, ok := findCall(fake.Calls, "gh")
	if !ok {
		t.Fatal("no gh call")
	}
	wantGh := []string{"pr", "view", "42", "--repo", "cameronsjo/forgectl", "--json", "headRefName,headRefOid,headRepositoryOwner,headRepository"}
	if !equalArgs(gh.Args, wantGh) {
		t.Errorf("gh args = %v, want %v", gh.Args, wantGh)
	}

	// git clone argv: --branch <headRef> and a -- terminator guarding positionals.
	git, ok := findCall(fake.Calls, "git")
	if !ok {
		t.Fatal("no git call")
	}
	if git.Args[0] != "clone" || !contains(git.Args, "--branch") || !contains(git.Args, "feature-x") || !contains(git.Args, "--") {
		t.Errorf("git clone args missing --branch/headRef/--: %v", git.Args)
	}
	if !contains(git.Args, "https://github.com/contributor/forgectl") {
		t.Errorf("git clone should target the head repo URL: %v", git.Args)
	}

	// Side effects: workspace exists, allowlist + breadcrumb written.
	if _, err := os.Stat(sess.Workspace); err != nil {
		t.Errorf("workspace missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sess.Workspace, ".claude", "settings.local.json")); err != nil {
		t.Errorf("allowlist missing: %v", err)
	}
	if _, err := os.Stat(sess.Path); err != nil {
		t.Errorf("breadcrumb missing: %v", err)
	}
}

func TestPrepare_IncompleteRefRefused(t *testing.T) {
	fake := &exec.FakeRunner{}
	c := testClient(t, fake)
	if _, err := c.Prepare(context.Background(), Ref{Number: 42}, PrepareOpts{}); err == nil {
		t.Error("expected refusal for an incomplete (bare) ref")
	}
	if len(fake.Calls) != 0 {
		t.Errorf("incomplete ref should not shell out; got %+v", fake.Calls)
	}
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
