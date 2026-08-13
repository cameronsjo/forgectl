//go:build unix

package config

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestReadPathAndValidatePath_SpecialFilesNeverBlock(t *testing.T) {
	tests := []struct {
		name string
		make func(*testing.T, string)
	}{
		{name: "directory", make: func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "fifo", make: func(t *testing.T, path string) {
			if err := unix.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink to fifo", make: func(t *testing.T, path string) {
			target := path + ".target"
			if err := unix.Mkfifo(target, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
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
			dir, err := os.MkdirTemp("/tmp", "forgectl-config-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			path := filepath.Join(dir, "config.toml")
			tt.make(t, path)
			done := make(chan error, 1)
			go func() { done <- ValidatePath(path) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("ValidatePath accepted a special file")
				}
			case <-time.After(time.Second):
				t.Fatal("ValidatePath blocked on special file")
			}
		})
	}
}

func TestReadPath_LeafSymlinkToRegularFileRemainsCompatible(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.toml")
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(target, []byte("no_icons = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	data, err := ReadPath(path)
	if err != nil || string(data) != "no_icons = true\n" {
		t.Fatalf("data=%q error=%v", data, err)
	}
}
