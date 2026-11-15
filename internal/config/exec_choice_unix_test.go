//go:build unix

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckExecChoiceFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	me := os.Geteuid()
	tests := []struct {
		name string
		path string
		uid  int
		want string // substring of the refusal; empty means accepted
	}{
		{"0600 owned by me", write("a", 0o600), me, ""},
		{"0644 owned by me", write("b", 0o644), me, ""},
		{"group writable", write("c", 0o664), me, "writable by group or other"},
		{"world writable", write("d", 0o606), me, "writable by group or other"},
		{"owned by someone else", write("e", 0o600), me + 1, "not you or root"},
		{"missing file", filepath.Join(dir, "missing"), me, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkExecChoiceFile(tt.path, tt.uid)
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}
