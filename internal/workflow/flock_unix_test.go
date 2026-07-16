//go:build unix

package workflow

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAcquireRunLock_RefusesSymlinkedLockFile pins the SEC-2 hardening: with the
// .state dir a real directory, a pre-planted <name>.lock symlink is refused by
// O_NOFOLLOW rather than followed to open its target. Unix-only — O_NOFOLLOW is
// the guard, and the lock is a documented no-op on non-unix.
func TestAcquireRunLock_RefusesSymlinkedLockFile(t *testing.T) {
	redirectStateDir(t)

	lockPath, err := LockPath("demo")
	if err != nil {
		t.Fatalf("LockPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	target := filepath.Join(t.TempDir(), "evil.lock")
	if err := os.Symlink(target, lockPath); err != nil {
		t.Fatalf("plant symlink at lock path: %v", err)
	}

	if _, err := AcquireRunLock("demo"); err == nil {
		t.Fatal("O_NOFOLLOW must refuse a symlinked lock file")
	}
	if _, err := os.Stat(target); err == nil {
		t.Error("the symlink target must not be created (open must not have followed the link)")
	}
}
