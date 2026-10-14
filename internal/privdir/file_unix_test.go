//go:build unix

package privdir_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/privdir"
)

// pinnedDir pins a fresh private directory and returns its descriptor and
// path.
func pinnedDir(t *testing.T) (int, string) {
	t.Helper()
	base := scratch(t)
	fd, err := pin(t, spec(base))
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	return fd, filepath.Join(base, "forgectl")
}

func TestOpenFile_CreatesAtModeAndReopens(t *testing.T) {
	dir, path := pinnedDir(t)
	f, err := privdir.OpenFile(dir, "data", unix.O_RDWR|unix.O_CREAT, 0o600)
	if err != nil {
		t.Fatalf("OpenFile create: %v", err)
	}
	if _, err := f.WriteString("x"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	info, err := os.Stat(filepath.Join(path, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("created mode %o, want 600", got)
	}
	f, err = privdir.OpenFile(dir, "data", unix.O_RDONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile reopen: %v", err)
	}
	_ = f.Close()
}

func TestOpenFile_AbsentIsNotExist(t *testing.T) {
	dir, _ := pinnedDir(t)
	_, err := privdir.OpenFile(dir, "missing", unix.O_RDONLY, 0o600)
	if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, privdir.ErrUnsafeFile) {
		t.Fatalf("OpenFile on an absent name = %v, want fs.ErrNotExist only", err)
	}
}

// Each hostile object planted at the name is refused with ErrUnsafeFile, and
// promptly: a FIFO must not block the open.
func TestOpenFile_RefusesHostileObjects(t *testing.T) {
	cases := map[string]func(t *testing.T, path string){
		"symlink to a file": func(t *testing.T, path string) {
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		},
		"dangling symlink": func(t *testing.T, path string) {
			if err := os.Symlink(filepath.Join(t.TempDir(), "nowhere"), path); err != nil {
				t.Fatal(err)
			}
		},
		"directory": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"fifo": func(t *testing.T, path string) {
			if err := unix.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"hardlink": func(t *testing.T, path string) {
			other := filepath.Join(filepath.Dir(path), "other")
			if err := os.WriteFile(other, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(other, path); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, plant := range cases {
		for _, flags := range []int{unix.O_RDONLY, unix.O_RDWR | unix.O_CREAT} {
			t.Run(name, func(t *testing.T) {
				dir, path := pinnedDir(t)
				target := filepath.Join(path, "data")
				plant(t, target)
				done := make(chan error, 1)
				go func() {
					f, err := privdir.OpenFile(dir, "data", flags, 0o600)
					if f != nil {
						_ = f.Close()
					}
					done <- err
				}()
				select {
				case err := <-done:
					if !errors.Is(err, privdir.ErrUnsafeFile) {
						t.Fatalf("OpenFile over a %s = %v, want ErrUnsafeFile", name, err)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("OpenFile blocked on a %s", name)
				}
			})
		}
	}
}

func TestOpenFile_RefusesAnotherUsersFile(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to give a file to another user")
	}
	dir, path := pinnedDir(t)
	target := filepath.Join(path, "data")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(target, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	if _, err := privdir.OpenFile(dir, "data", unix.O_RDONLY, 0o600); !errors.Is(err, privdir.ErrUnsafeFile) {
		t.Fatalf("OpenFile on another user's file = %v, want ErrUnsafeFile", err)
	}
}

func TestOpenFile_NarrowsABroadMode(t *testing.T) {
	dir, path := pinnedDir(t)
	target := filepath.Join(path, "data")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G302: a broad mode is the fixture OpenFile must narrow
	if err := os.Chmod(target, 0o666); err != nil {
		t.Fatal(err)
	}
	f, err := privdir.OpenFile(dir, "data", unix.O_RDONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	_ = f.Close()
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode after OpenFile %o, want 600", got)
	}
}

func TestOpenFile_RefusesANameOrModeItCannotCheck(t *testing.T) {
	dir, _ := pinnedDir(t)
	for _, name := range []string{"", ".", "..", "a/b", "/etc/passwd", "a\x00b"} {
		if _, err := privdir.OpenFile(dir, name, unix.O_RDONLY, 0o600); !errors.Is(err, privdir.ErrUnsafeFile) {
			t.Errorf("OpenFile(%q) = %v, want ErrUnsafeFile", name, err)
		}
	}
	for _, mode := range []fs.FileMode{0, 0o600 | fs.ModeSetuid} {
		if _, err := privdir.OpenFile(dir, "data", unix.O_RDWR|unix.O_CREAT, mode); !errors.Is(err, privdir.ErrUnsafeFile) {
			t.Errorf("OpenFile with mode %v = %v, want ErrUnsafeFile", mode, err)
		}
	}
}
