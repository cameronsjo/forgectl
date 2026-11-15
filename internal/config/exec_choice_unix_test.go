//go:build unix

package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestReadPathTrust(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil { //nolint:gosec // G302: the test needs loose modes to prove they are refused
			t.Fatal(err)
		}
		return p
	}
	me := os.Geteuid()
	tests := []struct {
		name string
		path string
		uid  int
		want string // substring of the refusal; empty means trusted
	}{
		{"0600 owned by me", write("a", 0o600), me, ""},
		{"0644 owned by me", write("b", 0o644), me, ""},
		{"group writable", write("c", 0o664), me, "writable by group or other"},
		{"world writable", write("d", 0o606), me, "writable by group or other"},
		{"owned by someone else", write("e", 0o600), me + 1, "not you or root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, trust, err := readPathTrustAs(tt.path, tt.uid)
			if err != nil || string(data) != "x" {
				t.Fatalf("read = %q, %v; the verdict must not stop the read", data, err)
			}
			switch {
			case tt.want == "" && trust != nil:
				t.Fatalf("refused: %v", trust)
			case tt.want != "" && (trust == nil || !strings.Contains(trust.Error(), tt.want)):
				t.Fatalf("trust = %v, want it to contain %q", trust, tt.want)
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		if _, _, err := readPathTrustAs(filepath.Join(dir, "missing"), me); !os.IsNotExist(err) {
			t.Fatalf("err = %v, want not-exist", err)
		}
	})
}

// TestReadPathTrust_RootAndSymlinks pins the two behaviours the doc states: a
// root-owned file is trusted for any user, and a symlink is judged by the file
// it reaches, not by its own 0777 mode.
func TestReadPathTrust_RootAndSymlinks(t *testing.T) {
	t.Run("root-owned file is trusted for another user", func(t *testing.T) {
		const hosts = "/etc/hosts"
		info, err := os.Stat(hosts)
		if err != nil {
			t.Skip("no /etc/hosts on this platform")
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
			t.Skip("/etc/hosts is not a root-owned, non-writable file here")
		}
		if _, trust, err := readPathTrustAs(hosts, os.Geteuid()+1); err != nil || trust != nil {
			t.Fatalf("read err = %v, trust = %v; want a root-owned file trusted", err, trust)
		}
	})
	dir := t.TempDir()
	good, loose := filepath.Join(dir, "good"), filepath.Join(dir, "loose")
	for name, mode := range map[string]os.FileMode{good: 0o600, loose: 0o664} {
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(name, mode); err != nil { //nolint:gosec // G302: see above
			t.Fatal(err)
		}
	}
	toGood, toLoose := filepath.Join(dir, "to-good"), filepath.Join(dir, "to-loose")
	if err := os.Symlink(good, toGood); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(loose, toLoose); err != nil {
		t.Fatal(err)
	}
	if _, trust, err := readPathTrustAs(toGood, os.Geteuid()); err != nil || trust != nil {
		t.Errorf("symlink to a 0600 file: err = %v, trust = %v; want trusted", err, trust)
	}
	if _, trust, _ := readPathTrustAs(toLoose, os.Geteuid()); trust == nil {
		t.Error("trusted a symlink to a group-writable file")
	}
}

// TestLoadPath_RecordsTheVerdictFromTheDescriptorItRead: LoadPath carries the
// verdict, so a caller never re-stats the path.
func TestLoadPath_RecordsTheVerdictFromTheDescriptorItRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[surface.rex]\nbinary_path = \"/nonexistent/rex\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := LoadPath(path).ExecChoiceFileError(); err != nil {
		t.Fatalf("0600 file refused: %v", err)
	}
	if err := os.Chmod(path, 0o666); err != nil { //nolint:gosec // G302: see above
		t.Fatal(err)
	}
	cfg := LoadPath(path)
	if err := cfg.ExecChoiceFileError(); err == nil {
		t.Fatal("a world-writable config was trusted")
	}
	if cfg.Surface.Rex.BinaryPath != "/nonexistent/rex" {
		t.Errorf("the verdict must not stop the decode: binary_path = %q", cfg.Surface.Rex.BinaryPath)
	}
	if err := LoadPath(filepath.Join(t.TempDir(), "absent.toml")).ExecChoiceFileError(); err != nil {
		t.Errorf("a missing file produced a verdict: %v", err)
	}
}
