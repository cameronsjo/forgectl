//go:build unix

package env

// Test plan for the lock file's own containment (lock_unix.go)
//
// The lock path is DERIVED (target + ".lock"), not resolved, so it never went
// through ResolveTarget and had no containment check of its own. A plain
// os.OpenFile follows symlinks, so a repo shipping `.env.lock` as a symlink
// made `env set` create a file outside the repository while reporting an
// ordinary success. Git stores a symlink as mode 120000, so a hostile repo
// delivers this by being cloned — there is no local step to notice.
//
//   [x] Refused: .env.lock is a symlink pointing outside the repo, and the
//       outside path is NOT created
//   [x] Refused: .env.lock is a symlink pointing inside the repo (the flag
//       refuses the link itself; where it points is not the question)
//   [x] Refused: .env.lock exists as a non-regular file (a FIFO would
//       otherwise block the open forever)
//   [x] Happy: an ordinary run still creates a plain 0600 lock file and the
//       write lands

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/cameronsjo/forgectl/internal/clip"
	"github.com/cameronsjo/forgectl/internal/exec"
)

func lockTestClient() *Client {
	return NewClient(clip.New(&exec.FakeRunner{}, clip.WithGOOS("darwin")))
}

func TestWithFileLock_SymlinkedLockEscape_Refused(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	outside := t.TempDir()
	planted := filepath.Join(outside, "PLANTED")

	if err := os.Symlink(planted, filepath.Join(repo, ".env.lock")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	target := mustTarget(t, ".env", repo)
	_, err := lockTestClient().SetValue(target, "KEY", "value")
	if err == nil {
		t.Fatal("SetValue through a symlinked lock file returned nil error, want a refusal")
	}

	// The assertion that matters: the out-of-repo path was never created.
	// A refusal that still touched it would be a refusal in name only.
	if _, statErr := os.Stat(planted); !os.IsNotExist(statErr) {
		t.Errorf("the out-of-repo path %s exists, want it never created", planted)
	}
	// And the target itself must be untouched.
	if _, statErr := os.Stat(filepath.Join(repo, ".env")); !os.IsNotExist(statErr) {
		t.Error(".env was written despite the lock refusal")
	}
}

func TestWithFileLock_SymlinkedLockInsideRepo_Refused(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	decoy := filepath.Join(repo, "decoy")
	if err := os.WriteFile(decoy, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(decoy, filepath.Join(repo, ".env.lock")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// Refused even though the link's target is inside the repo. The rule is
	// about the lock path being a symlink at all — a containment check on
	// where it points would pass here, and then the next repo would point it
	// somewhere that check happened to allow. O_NOFOLLOW asks the narrower,
	// answerable question.
	target := mustTarget(t, ".env", repo)
	if _, err := lockTestClient().SetValue(target, "KEY", "value"); err == nil {
		t.Fatal("SetValue through an in-repo symlinked lock file returned nil error, want a refusal")
	}
}

func TestWithFileLock_LockIsFIFO_Refused(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	if err := syscall.Mkfifo(filepath.Join(repo, ".env.lock"), 0o600); err != nil {
		t.Skipf("Mkfifo unsupported in this environment: %v", err)
	}

	// A FIFO's open blocks until a writer appears, so without this refusal
	// `env set` hangs forever on a repo that shipped one — a denial of
	// service with no error to read.
	target := mustTarget(t, ".env", repo)
	if _, err := lockTestClient().SetValue(target, "KEY", "value"); err == nil {
		t.Fatal("SetValue against a FIFO lock file returned nil error, want a refusal")
	}
}

func TestWithFileLock_OrdinaryRun_CreatesPlain0600Lock(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)

	target := mustTarget(t, ".env", repo)
	if _, err := lockTestClient().SetValue(target, "KEY", "value"); err != nil {
		t.Fatalf("SetValue: %v", err)
	}

	// Proves the three refusals above did not simply break the happy path —
	// the check that cannot go green on correct input is worth nothing.
	fi, err := os.Lstat(filepath.Join(repo, ".env.lock"))
	if err != nil {
		t.Fatalf("Lstat lock file: %v", err)
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("lock file mode = %v, want a regular file", fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("lock file perm = %04o, want 0600", perm)
	}
	got, err := os.ReadFile(filepath.Join(repo, ".env")) //nolint:gosec // G304: a path this test built inside its own t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile .env: %v", err)
	}
	if want := "KEY=value\n"; string(got) != want {
		t.Errorf(".env = %q, want %q", got, want)
	}
}
