package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// envFixture is the same session/runner shape windows_test.go uses, reduced to
// what an env assertion needs.
func envFixture(t *testing.T) (*exec.FakeRunner, *Client, SessionIdentity) {
	t.Helper()
	sessionRow := strings.Join([]string{"123", "456", "$1", "forge", "1", "0", "0", "/repo"}, FieldSep)
	fake := &exec.FakeRunner{RunFunc: func(_ string, args []string) (string, error) {
		switch args[0] {
		case "list-sessions":
			return sessionRow, nil
		case "new-window":
			return "123" + FieldSep + "456" + FieldSep + "@7", nil
		}
		return "", nil
	}}
	c := New(fake)
	identityEnv(c, "", "/tmp")
	return fake, c, SessionIdentity{
		Generation: ServerGeneration{Selector: ServerSelector{TmpDir: "/tmp"}, PID: "123", StartTime: "456"},
		ID:         "$1",
		Name:       "forge",
	}
}

// TestNewWindowWithEnv_EmitsOneFlagPerEntry pins the argv tmux actually gets.
// Order matters to the assertion, not to tmux: the -e flags must land after -c
// and before the `--` that ends option parsing, or tmux reads them as part of
// the command.
func TestNewWindowWithEnv_EmitsOneFlagPerEntry(t *testing.T) {
	fake, c, session := envFixture(t)

	env := []string{"HTTPS_PROXY=http://proxy.example:8080", "NO_PROXY="}
	if _, err := c.NewWindowWithEnv(
		context.Background(), session, "review", "/repo", env, "claude", "-p", "prompt",
	); err != nil {
		t.Fatalf("NewWindowWithEnv: %v", err)
	}
	if len(fake.Calls) != 2 {
		t.Fatalf("calls = %d, want revalidation + creation", len(fake.Calls))
	}
	argsEqual(t, fake.Calls[1].Args, []string{
		"new-window", "-P", "-F", IdentityFormat,
		"-t", "$1:", "-n", "review", "-c", "/repo",
		"-e", "HTTPS_PROXY=http://proxy.example:8080",
		"-e", "NO_PROXY=",
		"--", "claude", "-p", "prompt",
	})
}

// TestNewWindow_PassesNoEnvFlags is the control for the test above: the
// original entry point must produce byte-identical argv to what it produced
// before the env parameter existed, or every existing caller changed behavior.
func TestNewWindow_PassesNoEnvFlags(t *testing.T) {
	fake, c, session := envFixture(t)

	if _, err := c.NewWindow(context.Background(), session, "review", "/repo", "claude"); err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	for _, a := range fake.Calls[1].Args {
		if a == "-e" {
			t.Fatalf("NewWindow emitted an -e flag with no env supplied: %v", fake.Calls[1].Args)
		}
	}
}

// TestNewWindowWithEnv_RefusesBeforeRunningTmux is the ordering that makes the
// validation worth having: a bad entry must be rejected before any tmux
// command runs, so a refused window is never half-created.
func TestNewWindowWithEnv_RefusesBeforeRunningTmux(t *testing.T) {
	fake, c, session := envFixture(t)

	_, err := c.NewWindowWithEnv(
		context.Background(), session, "review", "/repo",
		[]string{"-kill-server=1"}, "claude",
	)
	if !errors.Is(err, ErrBadEnvAssignment) {
		t.Fatalf("err = %v, want ErrBadEnvAssignment", err)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("tmux ran %d command(s) before the refusal: %+v", len(fake.Calls), fake.Calls)
	}
}

// TestValidateEnvAssignment covers the boundary that keeps a profile value from
// being parsed as a tmux flag. The key rule is what carries it: a POSIX
// variable name cannot begin with `-`, so `-e <entry>` can never hand tmux
// something it reads as an option instead of an operand.
func TestValidateEnvAssignment(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry string
		ok    bool
	}{
		{"a plain assignment", "HTTPS_PROXY=http://proxy.example:8080", true},
		{"lowercase spelling", "https_proxy=http://proxy.example:8080", true},
		{"digits after the first character", "PROXY2=http://proxy.example:8080", true},
		{"leading underscore", "_X=1", true},
		// The removal form the tmux sink is limited to: set-empty, because
		// new-window can set a variable and cannot unset one.
		{"an empty value is the removal form", "NO_PROXY=", true},
		// A value may legally contain '=' — a proxy URL with a query string
		// does. Only the FIRST '=' separates.
		{"equals inside the value", "ALL_PROXY=socks5://h:1080?a=b", true},
		{"semicolon inside the value", "NO_PROXY=a;b", true},

		{"no equals at all", "HTTPS_PROXY", false},
		{"empty key", "=value", false},
		{"flag-shaped entry", "-kill-server=1", false},
		{"leading digit", "2FA=x", false},
		{"space in the key", "HTTPS PROXY=x", false},
		// A newline is what a crafted value would use to forge a second line in
		// anything that reads tmux's argv back; NUL cannot cross exec at all.
		{"newline in the value", "HTTPS_PROXY=http://a\nkill-server", false},
		{"carriage return in the value", "HTTPS_PROXY=http://a\rx", false},
		// tmux ends a command at an argument ending in ";", so a value ending
		// in one splits the new-window argv. A ";" inside a value is harmless.
		{"trailing semicolon in the value", "NO_PROXY=localhost;", false},
		{"NUL in the value", "HTTPS_PROXY=http://a\x00x", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateEnvAssignment(tc.entry)
			if tc.ok && err != nil {
				t.Fatalf("validateEnvAssignment(%q) = %v, want nil", tc.entry, err)
			}
			if !tc.ok && !errors.Is(err, ErrBadEnvAssignment) {
				t.Fatalf("validateEnvAssignment(%q) = %v, want ErrBadEnvAssignment", tc.entry, err)
			}
		})
	}
}
