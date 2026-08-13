//go:build unix

package config

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestConfigWriteLock_NonregularLeafRefusesWithoutCallback(t *testing.T) {
	tests := []struct {
		name string
		make func(t *testing.T, path string) func()
	}{
		{
			name: "symlink",
			make: func(t *testing.T, path string) func() {
				t.Helper()
				target := filepath.Join(filepath.Dir(path), "target")
				if err := os.WriteFile(target, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
				return func() {}
			},
		},
		{
			name: "directory",
			make: func(t *testing.T, path string) func() {
				t.Helper()
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				return func() {}
			},
		},
		{
			name: "fifo",
			make: func(t *testing.T, path string) func() {
				t.Helper()
				if err := unix.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
				return func() {}
			},
		},
		{
			name: "unix socket",
			make: func(t *testing.T, path string) func() {
				t.Helper()
				ln, err := net.Listen("unix", path)
				if err != nil {
					if errors.Is(err, unix.EPERM) {
						t.Skip("sandbox does not permit Unix socket creation")
					}
					t.Fatal(err)
				}
				return func() { _ = ln.Close() }
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "forgectl-lock-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			configPath := filepath.Join(dir, "config.toml")
			cleanup := tt.make(t, configPath+".lock")
			defer cleanup()
			calls := 0
			err = WithFileLock(configPath, func() error {
				calls++
				return nil
			})
			if err == nil {
				t.Fatal("WithFileLock() = nil, want refusal")
			}
			if calls != 0 {
				t.Fatalf("callback calls = %d, want 0", calls)
			}
		})
	}
}
