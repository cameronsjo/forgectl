//go:build unix

package config

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func prepareBackupSnapshot(t *testing.T, body []byte) (*LegacyMigrationBoundary, string) {
	t.Helper()
	env, _, legacyPath := boundaryFixture(t, body)
	b, err := PrepareLegacyMigrationBoundary(env, NativeMigrationFS())
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != BoundaryMigratable {
		b.Close() //nolint:errcheck
		t.Fatalf("boundary status = %v, refusal %v", b.Status, b.Refusal)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, legacyPath
}

func writeAndValidateBackup(t *testing.T, source *LegacySnapshot) *BackupAllocation {
	t.Helper()
	b, err := source.AllocateBackup()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Write(source.Data); err != nil {
		t.Fatal(err)
	}
	if err := b.SetPrivateMode(source.Mode); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncFile(); err != nil {
		t.Fatal(err)
	}
	if err := b.CloseWriter(); err != nil {
		t.Fatal(err)
	}
	if err := b.SyncParent(); err != nil {
		t.Fatal(err)
	}
	if err := b.Validate(source.Data); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBackupIdentity_NoClobberAndExactCapturedBytes(t *testing.T) {
	body := []byte("[defaults]\nmodel = \"sonnet\"\n")
	boundary, legacyPath := prepareBackupSnapshot(t, body)
	stable := legacyPath + ".bak"
	if err := os.WriteFile(stable, []byte("prior backup"), 0o600); err != nil {
		t.Fatal(err)
	}

	backup := writeAndValidateBackup(t, boundary.Source)
	defer backup.Close() //nolint:errcheck
	if backup.Path == stable || !strings.HasPrefix(backup.Path, stable+".") {
		t.Fatalf("backup path = %q, want exclusive unique fallback after occupied stable name", backup.Path)
	}
	if got, _ := os.ReadFile(stable); string(got) != "prior backup" {
		t.Fatalf("occupied stable backup changed to %q", got)
	}
	if got, _ := os.ReadFile(backup.Path); string(got) != string(body) {
		t.Fatalf("backup bytes = %q, want captured %q", got, body)
	}
	if info, _ := os.Stat(backup.Path); info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("backup mode = %04o, want private", info.Mode().Perm())
	}
}

func TestBackupIdentity_ReplacementBeforeValidationRetainsSourceAndOccupant(t *testing.T) {
	boundary, legacyPath := prepareBackupSnapshot(t, []byte("[defaults]\nmodel = \"sonnet\"\n"))
	backup, err := boundary.Source.AllocateBackup()
	if err != nil {
		t.Fatal(err)
	}
	if err := backup.Write(boundary.Source.Data); err != nil {
		t.Fatal(err)
	}
	if err := backup.SetPrivateMode(boundary.Source.Mode); err != nil {
		t.Fatal(err)
	}
	if err := backup.SyncFile(); err != nil {
		t.Fatal(err)
	}
	if err := backup.CloseWriter(); err != nil {
		t.Fatal(err)
	}
	if err := backup.SyncParent(); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(filepath.Dir(legacyPath), "replacement")
	if err := os.WriteFile(replacement, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, backup.Path); err != nil {
		t.Fatal(err)
	}
	if err := backup.Validate(boundary.Source.Data); !errors.Is(err, ErrBackupDrift) {
		t.Fatalf("Validate() error = %v, want ErrBackupDrift", err)
	}
	if got, _ := os.ReadFile(legacyPath); len(got) == 0 {
		t.Fatal("source was removed after backup drift")
	}
	if got, _ := os.ReadFile(backup.Path); string(got) != "replacement" {
		t.Fatalf("replacement occupant changed to %q", got)
	}
}

func TestBackupCleanupIdentity_DoesNotRemoveReplacement(t *testing.T) {
	boundary, _ := prepareBackupSnapshot(t, []byte("[defaults]\nmodel = \"sonnet\"\n"))
	backup, err := boundary.Source.AllocateBackup()
	if err != nil {
		t.Fatal(err)
	}
	if err := backup.CloseWriter(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(backup.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup.Path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backup.CleanupPartial(); !errors.Is(err, ErrBackupIdentityLost) {
		t.Fatalf("CleanupPartial() error = %v, want ErrBackupIdentityLost", err)
	}
	if got, _ := os.ReadFile(backup.Path); string(got) != "replacement" {
		t.Fatalf("replacement occupant changed to %q", got)
	}
}

func TestBackupIdentity_HardlinkedSourceRetiresOnlyNamedLink(t *testing.T) {
	boundary, legacyPath := prepareBackupSnapshot(t, []byte("[defaults]\nmodel = \"sonnet\"\n"))
	sibling := legacyPath + ".hardlink"
	if err := os.Link(legacyPath, sibling); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(sibling)
	backup := writeAndValidateBackup(t, boundary.Source)
	defer backup.Close() //nolint:errcheck
	if err := boundary.Source.Revalidate(); err != nil {
		t.Fatal(err)
	}
	if err := backup.Revalidate(boundary.Source.Data); err != nil {
		t.Fatal(err)
	}
	if err := boundary.Source.UnlinkNamedSource(); err != nil {
		t.Fatal(err)
	}
	if err := boundary.Source.SyncParent(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("named source still exists: %v", err)
	}
	after, err := os.Stat(sibling)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode() != after.Mode() || before.Size() != after.Size() {
		t.Fatalf("sibling hardlink metadata changed: before=%v after=%v", before, after)
	}
	if got, _ := os.ReadFile(sibling); string(got) != string(boundary.Source.Data) {
		t.Fatalf("sibling bytes = %q, want captured source", got)
	}
}

func TestBackupIdentity_EveryOccupiedStableLeafTypeRemainsUntouched(t *testing.T) {
	tests := []struct {
		name string
		make func(*testing.T, string)
	}{
		{name: "regular", make: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("prior"), 0o400); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink", make: func(t *testing.T, path string) {
			if err := os.Symlink("missing-target", path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "directory", make: func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o500); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "fifo", make: func(t *testing.T, path string) {
			if err := unix.Mkfifo(path, 0o400); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "socket", make: func(t *testing.T, path string) {
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				if errors.Is(err, unix.EPERM) {
					t.Skipf("sandbox refuses Unix sockets: %v", err)
				}
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			boundary, legacyPath := prepareBackupSnapshot(t, []byte("[defaults]\nmodel = \"sonnet\"\n"))
			stable := legacyPath + ".bak"
			tt.make(t, stable)
			before, err := os.Lstat(stable)
			if err != nil {
				t.Fatal(err)
			}
			beforeID, err := identityFromFileInfo(before)
			if err != nil {
				t.Fatal(err)
			}
			backup := writeAndValidateBackup(t, boundary.Source)
			defer backup.Close() //nolint:errcheck
			if backup.Path == stable || !strings.HasPrefix(backup.Path, stable+".") {
				t.Fatalf("backup path=%q, want unique after occupied stable", backup.Path)
			}
			after, err := os.Lstat(stable)
			if err != nil {
				t.Fatal(err)
			}
			afterID, err := identityFromFileInfo(after)
			if err != nil {
				t.Fatal(err)
			}
			if beforeID != afterID || before.Mode() != after.Mode() {
				t.Fatalf("occupied stable entry changed: before=%v/%v after=%v/%v", beforeID, before.Mode(), afterID, after.Mode())
			}
			if tt.name == "regular" {
				if got, _ := os.ReadFile(stable); string(got) != "prior" {
					t.Fatalf("regular occupant bytes=%q", got)
				}
			}
			if tt.name == "symlink" {
				if got, _ := os.Readlink(stable); got != "missing-target" {
					t.Fatalf("symlink target=%q", got)
				}
			}
		})
	}
}

func TestBackupConcurrentAllocation_ReceivesDistinctExclusiveNames(t *testing.T) {
	body := []byte("[defaults]\nmodel = \"sonnet\"\n")
	first, legacyPath := prepareBackupSnapshot(t, body)
	secondEnv := EnvSnapshot{Home: filepath.Dir(filepath.Dir(legacyPath)), XDGConfigHome: filepath.Dir(filepath.Dir(legacyPath)), UserConfigHome: filepath.Dir(filepath.Dir(legacyPath))}
	second, err := PrepareLegacyMigrationBoundary(secondEnv, NativeMigrationFS())
	if err != nil || second.Status != BoundaryMigratable {
		t.Fatalf("second boundary=%v/%v", second, err)
	}
	defer second.Close() //nolint:errcheck

	allocations := make([]*BackupAllocation, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, source := range []*LegacySnapshot{first.Source, second.Source} {
		wg.Add(1)
		go func(i int, source *LegacySnapshot) {
			defer wg.Done()
			allocations[i], errs[i] = source.AllocateBackup()
		}(i, source)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("allocation %d error=%v", i, err)
		}
		defer allocations[i].Close() //nolint:errcheck
	}
	if allocations[0].Path == allocations[1].Path {
		t.Fatalf("concurrent allocations collided at %q", allocations[0].Path)
	}
}

func TestBackupIdentity_InPlaceMutationAfterValidationIsDetected(t *testing.T) {
	boundary, _ := prepareBackupSnapshot(t, []byte("[defaults]\nmodel = \"sonnet\"\n"))
	backup := writeAndValidateBackup(t, boundary.Source)
	defer backup.Close() //nolint:errcheck
	if err := os.WriteFile(backup.Path, []byte("changed in place"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backup.Revalidate(boundary.Source.Data); !errors.Is(err, ErrBackupDrift) {
		t.Fatalf("Revalidate error=%v, want backup drift", err)
	}
}
