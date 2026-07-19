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
	"errors"
	"os"
	"path/filepath"
	"strings"
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
		WithFindingsDir(t.TempDir()),
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

// TestSandboxAndQuarantine_SandboxFailureWrapped covers the extracted helper's
// first failure branch (Prepare and PrepareLocal now share this code, so a
// regression here breaks both callers identically). A `git worktree add`
// failure must surface as "sandbox: …" and never reach quarantine.
func TestSandboxAndQuarantine_SandboxFailureWrapped(t *testing.T) {
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "git" && contains(args, "worktree") {
				return "", errors.New("boom")
			}
			return "", nil
		},
	}
	c := testClient(t, fake)

	ws, err := c.sandboxAndQuarantine(context.Background(), t.TempDir(), "HEAD", false)
	if err == nil {
		t.Fatal("expected an error from a failing git worktree add")
	}
	if ws != "" {
		t.Errorf("workspace = %q, want empty on failure", ws)
	}
	if !strings.HasPrefix(err.Error(), "sandbox:") {
		t.Errorf("error = %q, want a \"sandbox:\" prefix", err.Error())
	}
	if strings.Contains(err.Error(), "quarantine") {
		t.Errorf("error = %q, must not mention quarantine — Sandbox failed before quarantine ran", err.Error())
	}
}

// TestSandboxAndQuarantine_QuarantineFailureTearsDownWorkspace covers the
// second failure branch: when quarantine.Hide errors, sandboxAndQuarantine
// must tear the freshly created workspace back down and return the wrapped
// error, not leak a half-quarantined directory to either caller.
func TestSandboxAndQuarantine_QuarantineFailureTearsDownWorkspace(t *testing.T) {
	var capturedDir string
	fake := &exec.FakeRunner{
		RunFunc: func(name string, args []string) (string, error) {
			if name == "git" && contains(args, "worktree") {
				for i, a := range args {
					if a == "--" && i+1 < len(args) {
						capturedDir = args[i+1]
					}
				}
				// Pre-seed both CLAUDE.md and its already-quarantined destination so
				// quarantine.Hide refuses to clobber and returns an error, without
				// relying on OS-specific rename-onto-nonempty-dir semantics.
				if err := os.WriteFile(filepath.Join(capturedDir, "CLAUDE.md"), []byte("x"), 0o644); err != nil {
					return "", err
				}
				if err := os.WriteFile(filepath.Join(capturedDir, "CLAUDE.md.quarantined"), []byte("x"), 0o644); err != nil {
					return "", err
				}
			}
			return "", nil
		},
	}
	c := testClient(t, fake)

	ws, err := c.sandboxAndQuarantine(context.Background(), t.TempDir(), "HEAD", false)
	if err == nil {
		t.Fatalf("expected a quarantine failure, got workspace %q", ws)
	}
	if ws != "" {
		t.Errorf("workspace = %q, want empty on failure", ws)
	}
	if !strings.Contains(err.Error(), "quarantine workspace") {
		t.Errorf("error = %q, want it to mention quarantine workspace", err.Error())
	}
	if capturedDir == "" {
		t.Fatal("test bug: never captured the sandboxed workspace dir")
	}
	if _, statErr := os.Stat(capturedDir); !os.IsNotExist(statErr) {
		t.Errorf("workspace %q should be torn down after a quarantine failure; stat err = %v", capturedDir, statErr)
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
