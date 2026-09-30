//go:build unix

package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestUnreadableConfigIsRecorded is forgectl#684: a config.toml that exists
// but cannot be read is recorded the way a parse failure is — DecodeError is
// set, so the #653 gate refuses to run against defaults — and every surface
// (the loader, ValidatePath for doctor, Describe for `forgectl config`) words
// it the same way, without blocking on a FIFO.
func TestUnreadableConfigIsRecorded(t *testing.T) {
	tests := []struct {
		name   string
		make   func(*testing.T, string) string
		reason string
	}{
		{"directory", func(t *testing.T, dir string) string {
			path := filepath.Join(dir, "config.toml")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			return path
		}, "not a regular file"},
		{"fifo", func(t *testing.T, dir string) string {
			path := filepath.Join(dir, "config.toml")
			if err := unix.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}, "not a regular file"},
		{"parent is a regular file", func(t *testing.T, dir string) string {
			parent := filepath.Join(dir, "forgectl")
			if err := os.WriteFile(parent, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(parent, "config.toml")
		}, "not a directory"},
		{"permission denied", func(t *testing.T, dir string) string {
			if os.Geteuid() == 0 {
				t.Skip("root reads a mode-000 file")
			}
			path := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(path, []byte("no_icons = true\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
			return path
		}, "permission denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.make(t, t.TempDir())
			want := "config file " + `"` + path + `"` + " cannot be read: " + tt.reason

			type result struct {
				load, validate, describe error
				degraded, found          bool
			}
			done := make(chan result, 1)
			go func() {
				cfg := LoadPath(path)
				_, rep := describeFile(path)
				done <- result{cfg.DecodeError(), ValidatePath(path), rep.DecodeErr, cfg.DecodeDegraded(), rep.Found}
			}()
			var got result
			select {
			case got = <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("loading an unreadable config blocked")
			}
			for surface, err := range map[string]error{"LoadPath.DecodeError": got.load, "ValidatePath": got.validate, "Describe": got.describe} {
				if err == nil || err.Error() != want {
					t.Errorf("%s = %v, want %q", surface, err, want)
				}
			}
			if !got.degraded {
				t.Error("DecodeDegraded() = false for an unreadable config")
			}
			if !got.found {
				t.Error("Describe reports found = false for a file that exists")
			}
		})
	}
}

// TestUnreadableConfigKeepsCause: the worded error keeps the underlying error
// on the chain, so a caller can still tell a permission failure from the rest.
func TestUnreadableConfigKeepsCause(t *testing.T) {
	err := describeReadError("/x/config.toml", &fs.PathError{Op: "open", Path: "/x/config.toml", Err: unix.EACCES})
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("errors.Is(%v, fs.ErrPermission) = false", err)
	}
	if !strings.HasSuffix(err.Error(), ": permission denied") || strings.Count(err.Error(), "/x/config.toml") != 1 {
		t.Errorf("err = %q, want the path once and the fixed reason", err)
	}
}
