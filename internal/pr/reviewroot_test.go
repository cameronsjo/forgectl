package pr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// Test plan for reviewroot.go (forgectl#907)
//
// reviewWorkspaceRoot (Classification: security control, path derivation)
//   [x] An ordinary TMPDIR is the root
//   [x] TMPDIR that is, or is inside, claude-<uid>: the parent of that dir
//   [x] Another uid's claude-<n> dir is not the operator's shared one
//   [x] A parent that is not a directory refuses
// Wiring (Classification: integration)
//   [x] A local review made with TMPDIR=<root>/claude-<uid> gets its
//       workspace, private dir and settings dir in <root>, the sandbox denies
//       <root>/claude-<uid>, and teardown finds and removes all three

func stubReviewUID(t *testing.T, uid int) {
	t.Helper()
	orig := reviewUID
	reviewUID = func() int { return uid }
	t.Cleanup(func() { reviewUID = orig })
}

func TestReviewWorkspaceRoot(t *testing.T) {
	stubReviewUID(t, 501)
	base := t.TempDir()
	shared := filepath.Join(base, "claude-501")
	if err := os.MkdirAll(filepath.Join(shared, "deeper"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ tmpdir, want string }{
		{base, base},
		{shared, base},
		{filepath.Join(shared, "deeper"), base},
		{filepath.Join(base, "claude-502"), filepath.Join(base, "claude-502")},
	} {
		t.Setenv("TMPDIR", tc.tmpdir)
		got, err := reviewWorkspaceRoot()
		if err != nil || got != tc.want {
			t.Errorf("TMPDIR=%s: reviewWorkspaceRoot = %q, %v, want %q", tc.tmpdir, got, err, tc.want)
		}
	}

	// A file where the parent should be: refuse rather than use the shared dir.
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(file, "claude-501"))
	if got, err := reviewWorkspaceRoot(); err == nil {
		t.Errorf("an unusable parent gave root %q, want a refusal", got)
	}
}

// TestReviewDirs_InsideAClaudeSessionStayOutOfTheSharedDir is
// forgectl run from a Claude Code session's Bash, whose TMPDIR is the shared
// claude-<uid> dir.
func TestReviewDirs_InsideAClaudeSessionStayOutOfTheSharedDir(t *testing.T) {
	base := t.TempDir()
	stubReviewUID(t, 501)
	shared := filepath.Join(base, "claude-501")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", shared)

	root, err := reviewWorkspaceRoot()
	if err != nil {
		t.Fatal(err)
	}
	ws, err := os.MkdirTemp(root, sandboxPrefix+"*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removeReviewTmpDir(ws); _ = os.RemoveAll(ws) })
	tmp, err := createReviewTmpDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	_, settingsDir, err := writeReviewSettings(ws, "{}")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{ws, settingsDir} {
		if filepath.Dir(p) != base {
			t.Errorf("%s is not in %s, outside the shared dir", p, base)
		}
	}
	// The private temp dir goes wherever Claude Code will use it, which is
	// never inside the shared dir.
	if pathWithin(tmp, shared) {
		t.Errorf("private temp dir %s is inside the shared dir %s", tmp, shared)
	}
	deny, err := reviewDenyWrite(ws)
	if err != nil {
		t.Fatalf("reviewDenyWrite: %v", err)
	}
	if !strings.Contains(strings.Join(deny, "\n"), shared) {
		t.Errorf("denyWrite %v does not deny the shared dir %s", deny, shared)
	}
	if err := teardownWorkspace(context.Background(), &exec.FakeRunner{}, ws); err != nil {
		t.Fatalf("teardownWorkspace: %v", err)
	}
	for _, p := range []string{ws, tmp, settingsDir} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived teardown: %v", p, err)
		}
	}
}

// TestPrepareLocal_MakesTheWorkspaceOutsideTheSharedClaudeDir: the wiring,
// through PrepareLocal's sandbox step.
func TestPrepareLocal_MakesTheWorkspaceOutsideTheSharedClaudeDir(t *testing.T) {
	fake := localGitRunner()
	c := testClient(t, fake)
	path := t.TempDir()
	base := t.TempDir()
	shared := filepath.Join(base, "claude-501")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	stubReviewUID(t, 501)
	t.Setenv("TMPDIR", shared)

	sess, err := c.PrepareLocal(context.Background(), path, PrepareLocalOpts{Agent: "claude"})
	if err != nil {
		t.Fatalf("PrepareLocal: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(sess.Workspace)
		_ = os.RemoveAll(sess.FindingsDir)
	})
	if filepath.Dir(sess.Workspace) != base {
		t.Errorf("workspace %s was not made in %s, beside the shared dir", sess.Workspace, base)
	}
}
