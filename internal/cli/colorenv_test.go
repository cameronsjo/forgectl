package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// TestColorEnv_NoColorOutranksClicolorForce pins the precedence colorprofile
// does not implement on its own.
//
// colorprofile applies NO_COLOR only when the destination is a TTY
// (colorprofile@v0.4.3 env.go:86), but evaluates CLICOLOR_FORCE afterwards and
// unconditionally — so on a pipe the two together promote a NoTTY profile to
// ANSI and colour reaches the pipe. Measured at 12 escape sequences from
// `forgectl doctor` before colorEnv existed.
//
// https://no-color.org makes NO_COLOR absolute "when present and not an empty
// string (regardless of its value)", so it has to win.
func TestColorEnv_NoColorOutranksClicolorForce(t *testing.T) {
	tests := []struct {
		name      string
		env       []string
		wantColor bool
	}{
		{
			name:      "force alone colours a pipe",
			env:       []string{"CLICOLOR_FORCE=1", "TERM=xterm-256color"},
			wantColor: true,
		},
		{
			name: "no-color beats force",
			env:  []string{"NO_COLOR=1", "CLICOLOR_FORCE=1", "TERM=xterm-256color"},
		},
		{
			// The spec's own carve-out: an EMPTY NO_COLOR is not set. The test
			// helper forceColor relies on this to force colour in a process
			// whose environment may already carry NO_COLOR.
			name:      "empty no-color does not count as set",
			env:       []string{"NO_COLOR=", "CLICOLOR_FORCE=1", "TERM=xterm-256color"},
			wantColor: true,
		},
		{
			// Any non-empty value counts, per the spec's "regardless of its
			// value" — including one that looks falsy.
			name: "no-color=0 still counts as set",
			env:  []string{"NO_COLOR=0", "CLICOLOR_FORCE=1", "TERM=xterm-256color"},
		},
		{
			name: "a later assignment wins",
			env:  []string{"NO_COLOR=", "NO_COLOR=1", "CLICOLOR_FORCE=1", "TERM=xterm-256color"},
		},
		{
			name: "plain pipe stays plain",
			env:  []string{"TERM=xterm-256color"},
		},
	}

	styled := lipgloss.NewStyle().Foreground(lipgloss.Color("#b5bd68")).Render("ok")
	if !strings.Contains(styled, "\x1b") {
		t.Fatal("lipgloss rendered no escape at all; this test could not have gone red")
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := colorprofile.NewWriter(&buf, colorEnv(tt.env))
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

// TestColorEnv_LeavesOtherVariablesAlone keeps the filter narrow: it drops
// CLICOLOR_FORCE and nothing else, and only when it has reason to.
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

	// Without NO_COLOR the input is returned untouched.
	unchanged := []string{"CLICOLOR_FORCE=1", "TERM=xterm"}
	if len(colorEnv(unchanged)) != len(unchanged) {
		t.Errorf("colorEnv altered the environment with no NO_COLOR set: %v", colorEnv(unchanged))
	}
}
