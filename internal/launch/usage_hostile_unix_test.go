//go:build unix

package launch

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// hostileTarget is the file an attacker hopes forgectl will write to, chmod,
// or truncate. Every refusal test asserts it comes back byte-for-byte and
// mode-for-mode unchanged — a refusal that still touched the target is not a
// refusal.
type hostileTarget struct {
	path    string
	content []byte
	mode    os.FileMode
}

func plantHostileTarget(t *testing.T, dir string) hostileTarget {
	t.Helper()
	target := hostileTarget{
		path:    filepath.Join(dir, "victim"),
		content: []byte("do not touch\n"),
		mode:    0o644,
	}
	if err := os.WriteFile(target.path, target.content, target.mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target.path, target.mode); err != nil {
		t.Fatal(err)
	}
	return target
}

func (h hostileTarget) assertUntouched(t *testing.T) {
	t.Helper()
	info, err := os.Lstat(h.path)
	if err != nil {
		t.Fatalf("attacker target vanished: %v", err)
	}
	if got := info.Mode().Perm(); got != h.mode {
		t.Fatalf("attacker target mode = %o, want %o — a refusal chmod'd it", got, h.mode)
	}
	got, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(h.content) {
		t.Fatalf("attacker target content = %q, want %q", got, h.content)
	}
}

func TestUsageNamespace_RefusesSubstitutedLeaf(t *testing.T) {
	base := usageScratch(t)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	target := plantHostileTarget(t, elsewhere)

	if err := os.Symlink(elsewhere, filepath.Join(base, "forgectl")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := recordOne(t, "2026-08-13T10:00:00Z")
	if !errors.Is(err, ErrUsageUnsafeStore) {
		t.Fatalf("RecordUsage through a symlinked leaf = %v, want ErrUsageUnsafeStore", err)
	}
	target.assertUntouched(t)
	if _, err := os.Lstat(filepath.Join(elsewhere, usageDataName)); !os.IsNotExist(err) {
		t.Fatal("a refused write still created the data file in the attacker's directory")
	}
}

func TestUsageNamespace_RefusesNonDirectoryLeaf(t *testing.T) {
	base := usageScratch(t)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "forgectl"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recordOne(t, "2026-08-13T10:00:00Z"); !errors.Is(err, ErrUsageUnsafeStore) {
		t.Fatalf("RecordUsage with a file where the leaf belongs = %v, want ErrUsageUnsafeStore", err)
	}
}

// leafFor prepares a real state leaf so a test can plant a hostile entry in it.
func leafFor(t *testing.T, base string) string {
	t.Helper()
	leaf := filepath.Join(base, "forgectl")
	if err := os.MkdirAll(leaf, 0o700); err != nil {
		t.Fatal(err)
	}
	return leaf
}

func TestUsageNamespace_RefusesLockAndDataAliases(t *testing.T) {
	for _, name := range []string{usageDataName, usageLockName} {
		for _, kind := range []string{"symlink", "fifo", "directory", "hardlink"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				base := usageScratch(t)
				leaf := leafFor(t, base)
				elsewhere := t.TempDir()
				target := plantHostileTarget(t, elsewhere)
				entry := filepath.Join(leaf, name)
				if name == usageLockName {
					// The reader short-circuits on an absent data file, so a
					// hostile lock is only reachable once real data exists.
					if err := os.WriteFile(filepath.Join(leaf, usageDataName), nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}

				switch kind {
				case "symlink":
					if err := os.Symlink(target.path, entry); err != nil {
						t.Skipf("symlinks unavailable: %v", err)
					}
				case "fifo":
					if err := unix.Mkfifo(entry, 0o600); err != nil {
						t.Skipf("fifos unavailable: %v", err)
					}
				case "directory":
					if err := os.Mkdir(entry, 0o700); err != nil {
						t.Fatal(err)
					}
				case "hardlink":
					if err := os.Link(target.path, entry); err != nil {
						t.Skipf("hardlinks unavailable: %v", err)
					}
				}

				err := recordOne(t, "2026-08-13T10:00:00Z")
				if !errors.Is(err, ErrUsageUnsafeStore) {
					t.Fatalf("RecordUsage over a %s %s = %v, want ErrUsageUnsafeStore", kind, name, err)
				}
				target.assertUntouched(t)

				// The reader and doctor must refuse the same fixture — a
				// policy only one of them enforces is not a policy.
				if _, readErr := ReadUsage(nil, time.Now()); readErr == nil {
					t.Fatalf("ReadUsage accepted a %s %s", kind, name)
				}
				status, inspectErr := InspectUsage()
				if inspectErr != nil {
					t.Fatalf("InspectUsage: %v", inspectErr)
				}
				if status.Refusal == nil {
					t.Fatalf("doctor reported a %s %s as healthy", kind, name)
				}
				target.assertUntouched(t)
			})
		}
	}
}

func TestUsageNamespace_BroadExistingLeafAndFilesNarrowOnlyAfterIdentity(t *testing.T) {
	base := usageScratch(t)
	leaf := leafFor(t, base)
	if err := os.Chmod(leaf, 0o777); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(leaf, usageDataName)
	if err := os.WriteFile(data, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := recordOne(t, "2026-08-13T10:00:00Z"); err != nil {
		t.Fatalf("RecordUsage over safely-owned broad objects: %v", err)
	}
	for path, want := range map[string]os.FileMode{leaf: 0o700, data: 0o600} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode = %o, want it narrowed to %o", path, got, want)
		}
	}
}

func TestUsageNamespace_HardlinkedDataIsRefusedBeforeChmod(t *testing.T) {
	base := usageScratch(t)
	leaf := leafFor(t, base)
	elsewhere := t.TempDir()
	target := plantHostileTarget(t, elsewhere)
	if err := os.Link(target.path, filepath.Join(leaf, usageDataName)); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	if err := recordOne(t, "2026-08-13T10:00:00Z"); !errors.Is(err, ErrUsageUnsafeStore) {
		t.Fatalf("RecordUsage over a hardlinked data file = %v, want ErrUsageUnsafeStore", err)
	}
	// The mode assertion is the load-bearing one: a chmod that ran before the
	// link check would have widened or narrowed the attacker's own file.
	target.assertUntouched(t)
}
