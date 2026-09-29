package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	forgexec "github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/theme"
)

// hostileArg is a 10 KB command-line value built to be noticed if echoed.
var hostileArg = "MARKER" + strings.Repeat("\x1b[2J\u202e", 1400)

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

// ghFailure is a failed gh call as the real runner reports it: the
// CommandError's text is gh's stderr, which the host chooses (#658).
func ghFailure(name string, args []string) error {
	return &forgexec.CommandError{Name: name, Args: args, Stderr: "STDERRMARKER\x1b[2J‮", ExitCode: 1, Err: errors.New("exit status 1")}
}

// assertNoSubprocessText: the text shows neither gh's stderr nor a raw
// control, whichever sink it reached.
func assertNoSubprocessText(t *testing.T, where, text string) {
	t.Helper()
	if strings.Contains(text, "STDERRMARKER") {
		t.Fatalf("%s %q echoes the subprocess's stderr", where, text)
	}
	for _, r := range text {
		if termsafe.IsUnsafeTerminalRune(r) {
			t.Fatalf("%s %q carries raw rune %U", where, text, r)
		}
	}
}

// TestEcho_ProjectsCloneOrgListFailureHidesGhStderr: a failed `gh repo list`
// is reported categorically, and the CommandError stays on the chain.
func TestEcho_ProjectsCloneOrgListFailureHidesGhStderr(t *testing.T) {
	client := cloneFixture(t, func(name string, args []string) (string, error) {
		return "", ghFailure(name, args)
	})
	cmd := newProjectsCloneCmd(client, theme.Theme{})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--org", "cameronsjo"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("want an error when gh repo list fails")
	}
	assertNoSubprocessText(t, "error", err.Error())
	if !strings.Contains(err.Error(), "gh repo list failed") {
		t.Errorf("error = %q, want the categorical gh repo list failure", err)
	}
	var cmdErr *forgexec.CommandError
	if !errors.As(err, &cmdErr) {
		t.Errorf("error %v lost the CommandError from its chain", err)
	}
}

// TestEcho_ProjectsCloneOrgPerRepoLineHidesGhStderr covers the per-repo
// stderr line, which is a direct Fprintf and never passes the root error
// handler: before #658 it printed gh's stderr, raw controls included.
func TestEcho_ProjectsCloneOrgPerRepoLineHidesGhStderr(t *testing.T) {
	client := cloneFixture(t, func(name string, args []string) (string, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "repo" && args[1] == "list" {
			return `[{"name":"forgectl","sshUrl":"git@github.com:cameronsjo/forgectl.git","isPrivate":false}]`, nil
		}
		if name == "gh" && len(args) >= 2 && args[0] == "repo" && args[1] == "clone" {
			return "", ghFailure(name, args)
		}
		return "", nil
	})
	cmd := newProjectsCloneCmd(client, theme.Theme{})
	var stderr bytes.Buffer
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--org", "cameronsjo"})
	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("want the batch to fail when its one clone fails")
	}
	for _, line := range strings.Split(stderr.String(), "\n") {
		assertNoSubprocessText(t, "stderr line", line)
	}
	if !strings.Contains(stderr.String(), "gh repo clone failed") {
		t.Errorf("stderr = %q, want the categorical clone failure", stderr.String())
	}
}
