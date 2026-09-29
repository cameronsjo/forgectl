package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// hostileArg is a 10 KB command-line value built to be noticed if echoed.
var hostileArg = "MARKER" + strings.Repeat("\x1b[2J‮", 1400)

// assertCappedArgEcho: argv the operator typed is echoed back so they can see
// the typo, but capped and escaped (#562).
func assertCappedArgEcho(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if len(msg) > 2048 {
		t.Fatalf("error is %d bytes, want a capped echo", len(msg))
	}
	if !strings.Contains(msg, "MARKER") {
		t.Fatalf("error %q does not echo the typed value", msg)
	}
	for _, r := range msg {
		if termsafe.IsUnsafeTerminalRune(r) {
			t.Fatalf("error %q carries raw rune %U", msg, r)
		}
	}
}

func TestEcho_ProjectsListHostFlagIsCapped(t *testing.T) {
	client := listFixture(t, twoHostRunFunc("[]", ""))
	cmd := newProjectsListCmd(client)
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--host", hostileArg})
	assertCappedArgEcho(t, cmd.ExecuteContext(context.Background()))
}

// TestEcho_ProjectsCloneOrgIsCapped covers both --org echoes: the listing
// failure (where the rejected org reached the message unquoted) and the
// empty listing.
func TestEcho_ProjectsCloneOrgIsCapped(t *testing.T) {
	for name, listing := range map[string]string{"list failure": "", "no repos": "[]", "not a path segment": ""} {
		t.Run(name, func(t *testing.T) {
			client := cloneFixture(t, func(string, []string) (string, error) { return listing, nil })
			cmd := newProjectsCloneCmd(client, theme.Theme{})
			cmd.SetOut(new(bytes.Buffer))
			cmd.SetErr(new(bytes.Buffer))
			org := hostileArg
			if name == "no repos" {
				// A valid owner reaches the listing; only length is hostile.
				org = "MARKER" + strings.Repeat("a", 5000)
			}
			if name == "not a path segment" {
				// A '/' fails ListOrg's validPathSegment, whose own refusal
				// quoted the value uncapped before it rode out via %w.
				org = "MARKER/" + strings.Repeat("a", 5000)
			}
			cmd.SetArgs([]string{"--org", org})
			assertCappedArgEcho(t, cmd.ExecuteContext(context.Background()))
		})
	}
}
