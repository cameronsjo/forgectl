package cli

import (
	"reflect"
	"testing"
)

// TestEditorCommand guards the $EDITOR parsing, including the whitespace-only
// case that used to panic on fields[0] (an empty strings.Fields result).
func TestEditorCommand(t *testing.T) {
	const path = "/tmp/config.toml"
	cases := []struct {
		name     string
		env      string
		wantName string
		wantArgs []string
	}{
		{"empty falls back to vi", "", "vi", []string{path}},
		{"whitespace-only falls back to vi", "   ", "vi", []string{path}},
		{"bare editor", "vim", "vim", []string{path}},
		{"editor with flag", "code --wait", "code", []string{"--wait", path}},
		{"editor with multiple flags", "vim -O -p", "vim", []string{"-O", "-p", path}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, args := editorCommand(tc.env, path)
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
			if !reflect.DeepEqual(args, tc.wantArgs) {
				t.Errorf("args = %v, want %v", args, tc.wantArgs)
			}
		})
	}
}
