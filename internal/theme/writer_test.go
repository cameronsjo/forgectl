package theme

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// TestWriter_NoColorOutranksClicolorForce mirrors
// internal/cli/colorenv_test.go's TestColorEnv_NoColorOutranksClicolorForce:
// this is the same precedence fix, moved to the theme side of the leaf
// boundary that file's own doc comment says it will be replaced by.
func TestWriter_NoColorOutranksClicolorForce(t *testing.T) {
	tests := []struct {
		name      string
		env       []string
		wantColor bool
	}{
		{name: "force alone colours a pipe", env: []string{"CLICOLOR_FORCE=1", "TERM=xterm-256color"}, wantColor: true},
		{name: "no-color beats force", env: []string{"NO_COLOR=1", "CLICOLOR_FORCE=1", "TERM=xterm-256color"}},
		{name: "empty no-color does not count as set", env: []string{"NO_COLOR=", "CLICOLOR_FORCE=1", "TERM=xterm-256color"}, wantColor: true},
		{name: "no-color=0 still counts as set", env: []string{"NO_COLOR=0", "CLICOLOR_FORCE=1", "TERM=xterm-256color"}},
		{name: "a later assignment wins", env: []string{"NO_COLOR=", "NO_COLOR=1", "CLICOLOR_FORCE=1", "TERM=xterm-256color"}},
		{name: "plain pipe stays plain", env: []string{"TERM=xterm-256color"}},
	}

	th := Default().WithDark(true)
	styled := th.Style(RoleOK).Render("ok")
	if !strings.Contains(styled, "\x1b") {
		t.Fatal("theme rendered no escape at all; this test could not have gone red")
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := th.Writer(&buf, tt.env)
			if _, err := w.Write([]byte(styled)); err != nil {
				t.Fatalf("write: %v", err)
			}
			gotColor := strings.Contains(buf.String(), "\x1b")
			if gotColor != tt.wantColor {
				t.Errorf("colour reached the buffer = %v, want %v (env %v)\ngot %q",
					gotColor, tt.wantColor, tt.env, buf.String())
			}
		})
	}
}

func TestColorEnv_LeavesOtherVariablesAlone(t *testing.T) {
	in := []string{"PATH=/usr/bin", "NO_COLOR=1", "CLICOLOR_FORCE=1", "TERM=xterm", "CLICOLOR=1"}
	got := colorEnv(in)
	for _, kv := range got {
		if strings.HasPrefix(kv, "CLICOLOR_FORCE=") {
			t.Errorf("CLICOLOR_FORCE survived: %v", got)
		}
	}
	for _, want := range []string{"PATH=/usr/bin", "NO_COLOR=1", "TERM=xterm", "CLICOLOR=1"} {
		if !slices.Contains(got, want) {
			t.Errorf("colorEnv dropped %q; it should only remove CLICOLOR_FORCE: %v", want, got)
		}
	}

	unchanged := []string{"CLICOLOR_FORCE=1", "TERM=xterm"}
	if len(colorEnv(unchanged)) != len(unchanged) {
		t.Errorf("colorEnv altered the environment with no NO_COLOR set: %v", colorEnv(unchanged))
	}
}
