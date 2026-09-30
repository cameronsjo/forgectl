package projects

import (
	"path/filepath"
	"testing"
)

func TestResolveRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tests := []struct {
		name string
		env  string
		want string
	}{
		{"unset falls back to ~/Projects", "", filepath.Join(home, "Projects")},
		{"absolute value is used as is", "/srv/code", "/srv/code"},
		{"leading ~/ expands", "~/work", filepath.Join(home, "work")},
		{"bare tilde is not expanded", "~work", "~work"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PROJECTS_DIR", tt.env)
			if got := ResolveRoot(); got != tt.want {
				t.Errorf("ResolveRoot() = %q, want %q", got, tt.want)
			}
		})
	}
}
