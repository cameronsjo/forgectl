package theme

import (
	"bytes"
	"strings"
	"testing"
)

// TestColorEnv_NonBooleanNoColorStillWins pins the second half of the NO_COLOR
// precedence fix.
//
// colorprofile parses NO_COLOR with strconv.ParseBool
// (colorprofile@v0.4.3 env.go:116), so "purple" — which no-color.org
// explicitly makes valid, "regardless of its value" — fails to parse and its
// branch never fires. On a pipe that was masked, because the profile is
// already NoTTY. With CLICOLOR_FORCE promoting the profile it meant colour
// despite NO_COLOR being set.
func TestColorEnv_NonBooleanNoColorStillWins(t *testing.T) {
	styled := New(Options{}, true).Style(RoleAccent).Render("x")
	if !strings.Contains(styled, "\x1b") {
		t.Fatal("the fixture rendered no escape; this test could not go red")
	}

	for _, value := range []string{"purple", "1", "true", "0", "yes", "  "} {
		t.Run("NO_COLOR="+value, func(t *testing.T) {
			var buf bytes.Buffer
			w := New(Options{}, true).Writer(&buf, []string{
				"NO_COLOR=" + value,
				"CLICOLOR_FORCE=1",
				"TERM=xterm-256color",
			})
			if _, err := w.Write([]byte(styled)); err != nil {
				t.Fatalf("write: %v", err)
			}
			if strings.Contains(buf.String(), "\x1b") {
				t.Errorf("NO_COLOR=%q with CLICOLOR_FORCE=1 still emitted colour: %q", value, buf.String())
			}
		})
	}

	// An EMPTY NO_COLOR is not set, per the same sentence of the spec, so the
	// force flag must still win there.
	var empty bytes.Buffer
	ew := New(Options{}, true).Writer(&empty, []string{"NO_COLOR=", "CLICOLOR_FORCE=1", "TERM=xterm-256color"})
	if _, err := ew.Write([]byte(styled)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(empty.String(), "\x1b") {
		t.Error("an empty NO_COLOR suppressed colour; the spec says empty means not set")
	}

	// Positive control: with NO_COLOR absent the force flag works, so the
	// table above is not passing against a writer that never emits colour.
	var forced bytes.Buffer
	fw := New(Options{}, true).Writer(&forced, []string{"CLICOLOR_FORCE=1", "TERM=xterm-256color"})
	if _, err := fw.Write([]byte(styled)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(forced.String(), "\x1b") {
		t.Error("forced colour produced none; the assertions above prove nothing")
	}
}
