//go:build unix

package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestStateDirOpenat_ClosesSymlinkSwapRace is the #128 regression: it stages the
// same-user TOCTOU (real .state swapped for a symlink at attacker-controlled
// timing) two ways in one test so the contrast is the assertion.
//
//   - Sub-test A DOCUMENTS the vulnerability the old path-based code had: when the
//     reopen happens AFTER the swap, os.CreateTemp+os.Rename follow the symlink and
//     the state file lands in the attacker's directory.
//   - Sub-test B proves the FIX: openStateDir() pins the real .state by fd BEFORE
//     the swap, and every openat-relative op (createTemp/rename/syncDir) through
//     that handle lands in the original inode — the later symlink is never
//     consulted, and the attacker's directory stays empty.
func TestStateDirOpenat_ClosesSymlinkSwapRace(t *testing.T) {
	t.Run("A_path_based_follows_the_swap", func(t *testing.T) {
		dir := redirectStateDir(t)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("create real state dir: %v", err)
		}
		attacker := t.TempDir()

		// Swap the real .state aside and plant a symlink to the attacker dir in
		// its place — the window the path-based reopen would race into.
		aside := dir + ".real"
		if err := os.Rename(dir, aside); err != nil {
			t.Fatalf("move real state dir aside: %v", err)
		}
		if err := os.Symlink(attacker, dir); err != nil {
			t.Fatalf("plant symlink at state dir: %v", err)
		}

		// The OLD behavior: reopen by path (CreateTemp + Rename) after the swap.
		tmp, err := os.CreateTemp(dir, "demo.state.*.tmp")
		if err != nil {
			t.Fatalf("path-based CreateTemp: %v", err)
		}
		if _, err := tmp.WriteString("payload"); err != nil {
			t.Fatalf("write temp: %v", err)
		}
		tmpName := tmp.Name()
		if err := tmp.Close(); err != nil {
			t.Fatalf("close temp: %v", err)
		}
		if err := os.Rename(tmpName, filepath.Join(dir, "demo.state.toml")); err != nil {
			t.Fatalf("path-based Rename: %v", err)
		}

		// The file followed the symlink into the attacker's directory — the vuln.
		if _, err := os.Stat(filepath.Join(attacker, "demo.state.toml")); err != nil {
			t.Fatalf("path-based ops should have followed the symlink into the attacker dir, but the file is not there: %v", err)
		}
	})

	t.Run("B_openat_refuses_the_swap", func(t *testing.T) {
		dir := redirectStateDir(t)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("create real state dir: %v", err)
		}
		attacker := t.TempDir()

		// Pin the REAL .state by fd while it is still a real directory.
		d, err := openStateDir()
		if err != nil {
			t.Fatalf("openStateDir: %v", err)
		}
		defer d.close() //nolint:errcheck

		// Now stage the swap: move the real dir aside, symlink the attacker dir
		// into .state's place. The held fd still names the real (now aside) inode.
		aside := dir + ".real"
		if err := os.Rename(dir, aside); err != nil {
			t.Fatalf("move real state dir aside: %v", err)
		}
		if err := os.Symlink(attacker, dir); err != nil {
			t.Fatalf("plant symlink at state dir: %v", err)
		}

		// Drive the full write through the pinned handle.
		tmp, tmpName, err := d.createTemp("demo.state.*.tmp")
		if err != nil {
			t.Fatalf("openat createTemp: %v", err)
		}
		if _, err := tmp.WriteString("payload"); err != nil {
			t.Fatalf("write temp: %v", err)
		}
		if err := tmp.Sync(); err != nil {
			t.Fatalf("sync temp: %v", err)
		}
		if err := tmp.Close(); err != nil {
			t.Fatalf("close temp: %v", err)
		}
		if err := d.rename(tmpName, "demo.state.toml"); err != nil {
			t.Fatalf("openat rename: %v", err)
		}
		if err := d.syncDir(); err != nil {
			t.Fatalf("openat syncDir: %v", err)
		}

		// The file landed in the ORIGINAL inode (reachable at the aside path the
		// pinned fd still refers to), NOT through the symlink.
		if _, err := os.Stat(filepath.Join(aside, "demo.state.toml")); err != nil {
			t.Errorf("openat ops must land in the original inode, but the file is not at the aside path: %v", err)
		}
		// And the attacker's directory must be untouched.
		entries, err := os.ReadDir(attacker)
		if err != nil {
			t.Fatalf("read attacker dir: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("openat ops must not follow the symlink into the attacker dir, found %v", entries)
		}
	})
}

// TestCreateTemp_ExhaustsAfterRepeatedCollisions forces every attempt to mint
// the same name by stubbing the random source to a constant fill, so createTemp
// must run out its bounded EEXIST retry loop and report exhaustion rather than
// spinning forever or silently succeeding on a stale name.
func TestCreateTemp_ExhaustsAfterRepeatedCollisions(t *testing.T) {
	dir := redirectStateDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}

	d, err := openStateDir()
	if err != nil {
		t.Fatalf("openStateDir: %v", err)
	}
	defer d.close() //nolint:errcheck

	orig := randRead
	randRead = func(b []byte) (int, error) {
		for i := range b {
			b[i] = 0
		}
		return len(b), nil
	}
	defer func() { randRead = orig }()

	// First call succeeds and plants the colliding name; keep the file so the
	// second call's every attempt hits the same EEXIST.
	first, _, err := d.createTemp("demo.state.*.tmp")
	if err != nil {
		t.Fatalf("first createTemp: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first temp: %v", err)
	}

	_, _, err = d.createTemp("demo.state.*.tmp")
	if err == nil {
		t.Fatal("second createTemp: expected exhaustion error, got nil")
	}
	if !strings.Contains(err.Error(), "exhausted 100 attempts") {
		t.Errorf("second createTemp: expected exhaustion error, got: %v", err)
	}
}

// TestAcquireRunLock_LockFdIsCloexec is the CLOEXEC regression from the security
// review: the run-lock fd is held for the whole workflow run while step
// subprocesses are spawned, so it MUST carry FD_CLOEXEC or it leaks into every
// child. A raw unix.Openat does NOT set close-on-exec implicitly the way
// os.OpenFile does — this test fails deterministically if the explicit
// O_CLOEXEC on openLock is ever dropped.
func TestAcquireRunLock_LockFdIsCloexec(t *testing.T) {
	redirectStateDir(t)

	lock, err := AcquireRunLock("demo")
	if err != nil {
		t.Fatalf("AcquireRunLock: %v", err)
	}
	defer lock.Release()

	flags, err := unix.FcntlInt(lock.f.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatalf("fcntl F_GETFD on lock fd: %v", err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Error("run-lock fd must have FD_CLOEXEC set so it is not inherited by spawned step subprocesses")
	}
}
