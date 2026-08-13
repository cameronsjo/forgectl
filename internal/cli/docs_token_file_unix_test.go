//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func writeDocsTokenFixture(t *testing.T, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("Az09-._~+/===\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOpenDocsTokenFile_UnixIdentityPolicy(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o400} {
		t.Run(mode.String(), func(t *testing.T) {
			path := writeDocsTokenFixture(t, mode)
			file, err := openDocsTokenFile(path)
			if err != nil {
				t.Fatalf("openDocsTokenFile(%s): %v", mode, err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}

	for _, mode := range []os.FileMode{0o640, 0o644} {
		t.Run("reject_"+mode.String(), func(t *testing.T) {
			path := writeDocsTokenFixture(t, mode)
			if file, err := openDocsTokenFile(path); err == nil {
				file.Close() //nolint:errcheck // failing test cleanup
				t.Fatalf("openDocsTokenFile(%s) succeeded, want unsafe-permissions error", mode)
			}
		})
	}

	t.Run("directory", func(t *testing.T) {
		if file, err := openDocsTokenFile(t.TempDir()); err == nil {
			file.Close() //nolint:errcheck // failing test cleanup
			t.Fatal("directory accepted as token file")
		}
	})

	t.Run("leaf symlink", func(t *testing.T) {
		target := writeDocsTokenFixture(t, 0o600)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if file, err := openDocsTokenFile(link); err == nil {
			file.Close() //nolint:errcheck // failing test cleanup
			t.Fatal("leaf symlink accepted as token file")
		}
	})

	t.Run("intermediate directory symlink", func(t *testing.T) {
		dir := t.TempDir()
		realDir := filepath.Join(dir, "real")
		if err := os.Mkdir(realDir, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(realDir, "token")
		if err := os.WriteFile(target, []byte("valid"), 0o600); err != nil {
			t.Fatal(err)
		}
		linkDir := filepath.Join(dir, "link")
		if err := os.Symlink(realDir, linkDir); err != nil {
			t.Fatal(err)
		}
		file, err := openDocsTokenFile(filepath.Join(linkDir, "token"))
		if err != nil {
			t.Fatalf("intermediate symlink was rejected: %v", err)
		}
		file.Close() //nolint:errcheck // successful policy probe cleanup
	})

	t.Run("hard link", func(t *testing.T) {
		target := writeDocsTokenFixture(t, 0o600)
		link := filepath.Join(t.TempDir(), "hardlink")
		if err := os.Link(target, link); err != nil {
			t.Fatal(err)
		}
		if file, err := openDocsTokenFile(target); err == nil {
			file.Close() //nolint:errcheck // failing test cleanup
			t.Fatal("multiply-linked token file accepted")
		}
	})

	t.Run("fifo", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "fifo")
		if err := unix.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if file, err := openDocsTokenFile(path); err == nil {
			file.Close() //nolint:errcheck // failing test cleanup
			t.Fatal("FIFO accepted as token file")
		}
	})

	t.Run("owner mismatch when chown is available", func(t *testing.T) {
		path := writeDocsTokenFixture(t, 0o600)
		otherUID := os.Geteuid() + 1
		if err := os.Chown(path, otherUID, -1); err != nil {
			t.Skipf("cannot create owner-mismatch fixture: %v", err)
		}
		if file, err := openDocsTokenFile(path); err == nil {
			file.Close() //nolint:errcheck // failing test cleanup
			t.Fatal("foreign-owned token file accepted")
		}
	})
}

func TestOpenDocsTokenFile_UsesPinnedCloseOnExecDescriptor(t *testing.T) {
	path := writeDocsTokenFixture(t, 0o600)
	file, err := openDocsTokenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	osFile, ok := file.(*os.File)
	if !ok {
		t.Fatalf("opened token file type = %T, want *os.File", file)
	}
	flags, err := unix.FcntlInt(osFile.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("opened token descriptor is missing FD_CLOEXEC")
	}

	old := path + ".old"
	if err := os.Rename(path, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readDocsTokenFile(path, file)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Az09-._~+/===" || strings.Contains(got, "replacement") {
		t.Fatalf("read token = %q, want bytes from the validated descriptor", got)
	}
}
