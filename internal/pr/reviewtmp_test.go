package pr

// Test plan for reviewtmp.go (forgectl#907)
//
// reviewTmpDirFor (Classification: path derivation)
//   [x] A sibling of the workspace, named from its random suffix, never the
//       workspace prefix
//   [x] Refuses a relative workspace and one without the sandbox prefix
// createReviewTmpDir (Classification: security control, filesystem)
//   [x] Makes the dir 0700, even under a permissive umask's leftovers
//   [x] Refuses a dir that already exists, and a symlink in its place
//   [x] Reports, on Linux, a path too long for the sandbox's bridge sockets
// removeReviewTmpDir / teardownWorkspace (Classification: cleanup)
//   [x] A missing dir is not an error
//   [x] A symlink in its place is refused and its target left alone
//   [x] teardownWorkspace removes the workspace and then the private dir
// pinReviewTmpEnv
//   [x] Replaces ambient TMPDIR / CLAUDE_CODE_TMPDIR entries
// launchInline wiring
//   [x] The window env points both variables at a fresh 0700 dir
//   [x] A dispatch refused before the window opens removes the dir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/sandbox"
)

func TestReviewTmpDirFor_IsANamedSiblingOfTheWorkspace(t *testing.T) {
	got, err := reviewTmpDirFor("/tmp/" + sandbox.WorkspacePrefix + "123")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/"+reviewTmpPrefix+"123" {
		t.Errorf("reviewTmpDirFor = %q, want the sibling /tmp/%s123", got, reviewTmpPrefix)
	}
	if strings.HasPrefix(filepath.Base(got), sandbox.WorkspacePrefix) {
		t.Errorf("%q carries the workspace prefix, so it would read as a workspace", got)
	}
	for _, ws := range []string{"", "relative/" + sandbox.WorkspacePrefix + "1", "/tmp/not-a-workspace", "/tmp/" + sandbox.WorkspacePrefix} {
		if _, err := reviewTmpDirFor(ws); err == nil {
			t.Errorf("reviewTmpDirFor(%q) derived a dir, want a refusal", ws)
		}
	}
}

// fakeWorkspaceIn makes a workspace-shaped dir under parent. A t.TempDir()
// parent is longer than the Linux socket budget allows, so the length check
// (tested on its own) is switched off to keep its warning out of the log.
func fakeWorkspaceIn(t *testing.T, parent string) string {
	t.Helper()
	orig := reviewTmpGOOS
	reviewTmpGOOS = "none"
	t.Cleanup(func() { reviewTmpGOOS = orig })
	ws, err := os.MkdirTemp(parent, sandbox.WorkspacePrefix+"*")
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestCreateReviewTmpDir_MakesAPrivateDir(t *testing.T) {
	ws := fakeWorkspaceIn(t, t.TempDir())
	dir, err := createReviewTmpDir(ws)
	if err != nil {
		t.Fatalf("createReviewTmpDir: %v", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("private temp dir mode = %o, want 0700", perm)
	}
}

func TestCreateReviewTmpDir_RefusesAnExistingPath(t *testing.T) {
	t.Run("dir", func(t *testing.T) {
		ws := fakeWorkspaceIn(t, t.TempDir())
		dir, _ := reviewTmpDirFor(ws)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		planted := filepath.Join(dir, "planted")
		if err := os.WriteFile(planted, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := createReviewTmpDir(ws); err == nil {
			t.Fatal("createReviewTmpDir reused a dir it did not make")
		}
		if _, err := os.Stat(planted); err != nil {
			t.Errorf("the refusal touched the existing dir: %v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		parent := t.TempDir()
		ws := fakeWorkspaceIn(t, parent)
		dir, _ := reviewTmpDirFor(ws)
		target := filepath.Join(parent, "elsewhere")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, dir); err != nil {
			t.Fatal(err)
		}
		if _, err := createReviewTmpDir(ws); err == nil {
			t.Fatal("createReviewTmpDir accepted a symlink in its place")
		}
	})
}

func TestCheckReviewTmpDirLength(t *testing.T) {
	orig := reviewTmpGOOS
	t.Cleanup(func() { reviewTmpGOOS = orig })

	// 80 bytes: the length measured to break the Linux sandbox's sockets.
	long := "/" + strings.Repeat("d", 79)
	short := "/tmp/" + reviewTmpPrefix + "1234567890"

	reviewTmpGOOS = "linux"
	if err := checkReviewTmpDirLength(long); err == nil || !strings.Contains(err.Error(), "TMPDIR") {
		t.Errorf("an 80-byte dir on Linux = %v, want a refusal naming TMPDIR", err)
	}
	if err := checkReviewTmpDirLength(short); err != nil {
		t.Errorf("a /tmp dir on Linux was refused: %v", err)
	}
	reviewTmpGOOS = "darwin"
	if err := checkReviewTmpDirLength(long); err != nil {
		t.Errorf("macOS is not checked, but got %v", err)
	}
}

func TestRemoveReviewTmpDir(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		ws := fakeWorkspaceIn(t, t.TempDir())
		if err := removeReviewTmpDir(ws); err != nil {
			t.Errorf("removing a missing private dir = %v, want nil", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		parent := t.TempDir()
		ws := fakeWorkspaceIn(t, parent)
		dir, _ := reviewTmpDirFor(ws)
		target := filepath.Join(parent, "elsewhere")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		keep := filepath.Join(target, "keep")
		if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, dir); err != nil {
			t.Fatal(err)
		}
		if err := removeReviewTmpDir(ws); err == nil {
			t.Error("removeReviewTmpDir accepted a symlink in its place")
		}
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("the symlink's target lost a file: %v", err)
		}
	})
}

func TestTeardownWorkspace_RemovesThePrivateTmpDir(t *testing.T) {
	ws := fakeWorkspaceIn(t, t.TempDir())
	dir, err := createReviewTmpDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claude-settings-x.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := teardownWorkspace(context.Background(), &exec.FakeRunner{}, ws); err != nil {
		t.Fatalf("teardownWorkspace: %v", err)
	}
	for _, p := range []string{ws, dir} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived teardown (%v)", p, err)
		}
	}
}

func TestPinReviewTmpEnv_ReplacesAmbientEntries(t *testing.T) {
	env := []string{"TMPDIR=/shared", "PATH=/bin", "CLAUDE_CODE_TMPDIR=/shared2"}
	got := pinReviewTmpEnv(env, "/private")
	want := []string{"PATH=/bin", "TMPDIR=/private", "CLAUDE_CODE_TMPDIR=/private"}
	if !slices.Equal(got, want) {
		t.Errorf("pinReviewTmpEnv = %v, want %v", got, want)
	}
	if env[0] != "TMPDIR=/shared" {
		t.Error("pinReviewTmpEnv modified its input")
	}
}

func TestLaunchInline_GivesTheWindowAPrivateTmpDir(t *testing.T) {
	t.Setenv("FORGECTL_CLAUDE_BIN", fakeHarnessBin(t, "claude"))
	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"),
		WithWindowEnv(func() ([]string, error) { return []string{"TMPDIR=/tmp"}, nil }))
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: fakeWorkspace(t), Agent: "claude"}

	if _, err := c.Launch(context.Background(), sess, config.Config{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	dir, err := reviewTmpDirFor(sess.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	args := fake.Last().Args
	for _, key := range reviewTmpEnvKeys {
		if !argPair(args, "-e", key+"="+dir) {
			t.Errorf("tmux argv does not pin %s to the private dir %s: %v", key, dir, args)
		}
	}
	if argPair(args, "-e", "TMPDIR=/tmp") {
		t.Errorf("the ambient TMPDIR survived beside the pin: %v", args)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("the private dir does not outlive the dispatch: %v", err)
	}
	if perm := info.Mode().Perm(); !info.IsDir() || perm != 0o700 {
		t.Errorf("private dir mode = %v, want a 0700 directory", info.Mode())
	}
}

func TestLaunchInline_RefusedDispatchRemovesThePrivateTmpDir(t *testing.T) {
	t.Setenv("FORGECTL_CLAUDE_BIN", fakeHarnessBin(t, "claude"))
	// tmux -V fails, so CheckDispatchCapability refuses after the private
	// dir was made and before any window opens.
	fake := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "tmux" {
			return "", errors.New("no tmux here")
		}
		return "", nil
	}}
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	sess := Session{Ref: Ref{Owner: "o", Repo: "r", Number: 42}, Workspace: fakeWorkspace(t), Agent: "claude"}

	if _, err := c.Launch(context.Background(), sess, config.Config{}); err == nil {
		t.Fatal("Launch succeeded without tmux")
	}
	dir, err := reviewTmpDirFor(sess.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused dispatch left its private dir behind (%v)", err)
	}
}
