package exec

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestOSRunner_URLCredentialsNeverLogged pins #734: a token in a clone URL's
// userinfo reaches neither the debug log's argv, the failure log line, nor the
// error text, while the child still receives the real argv. The child echoes
// its argument to stderr, standing in for a tool (an older git, a remote's
// sideband) that repeats the URL it was given.
func TestOSRunner_URLCredentialsNeverLogged(t *testing.T) {
	const token = "ghp_fakeTokenValue0123456789" //nolint:gosec // G101: a fake token the redactor must hide
	cases := []string{
		"https://x-access-token:" + token + "@example.invalid/owner/repo",
		"https://" + token + "@example.invalid/owner/repo",
		token + "@example.invalid:owner/repo",
		"user:" + token + "@example.invalid:/p://x",
		// net/url reads no userinfo in these; git 2.43 sends both as Basic
		// auth.
		"http:///U:" + token + "@example.invalid/owner/repo",
		"http::http://U:" + token + "@example.invalid/owner/repo",
		// curl refuses four slashes, and git's error echoes the URL whole.
		"http:////U:" + token + "@example.invalid/owner/repo",
	}
	for _, url := range cases {
		t.Run(url, func(t *testing.T) {
			logs := captureLogs(t)
			_, err := OSRunner{}.Run(context.Background(), "sh", "-c", `echo "fatal: cannot reach $2" >&2; exit 3`, "sh", "--", url)
			if err == nil {
				t.Fatal("expected the command to fail")
			}
			if strings.Contains(err.Error(), token) {
				t.Errorf("token in the error text: %s", err.Error())
			}
			if strings.Contains(logs.String(), token) {
				t.Errorf("token in the log:\n%s", logs.String())
			}
			if !strings.Contains(err.Error(), "-- [redacted-arg]: fatal: cannot reach [redacted]") {
				t.Errorf("the element and the echoed word should be withheld whole: %s", err.Error())
			}
			// The structured fields stay the data the command ran with:
			// internal/tmux compares Args and Stderr for equality.
			var cmdErr *CommandError
			if !errors.As(err, &cmdErr) {
				t.Fatalf("want a *CommandError, got %T", err)
			}
			if cmdErr.Args[len(cmdErr.Args)-1] != url {
				t.Errorf("Args should keep the real element, got %q", cmdErr.Args)
			}
		})
	}
}

// TestOSRunner_ChildGetsRealURL: redaction is display-only.
func TestOSRunner_ChildGetsRealURL(t *testing.T) {
	url := "https://user:tok@example.invalid/x"
	out, err := OSRunner{}.Run(context.Background(), "sh", "-c", `printf %s "$1"`, "sh", url)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != url {
		t.Errorf("child saw %q, want %q", out, url)
	}
}

// TestCommandError_ErrorRedactsAHandBuiltError: Error() is itself a rendering
// point, so a CommandError another Runner (or a fake) builds with a raw argv
// and stderr renders without the credential too.
func TestCommandError_ErrorRedactsAHandBuiltError(t *testing.T) {
	e := &CommandError{
		Name:   "git",
		Args:   []string{"clone", "--", "https://u:s3cr3t@example.invalid/o/r"},
		Stderr: "remote: see https://u:s3cr3t@example.invalid/o/r",
		Err:    errors.New("exit status 128"),
	}
	got := e.Error()
	if strings.Contains(got, "s3cr3t") {
		t.Fatalf("credential rendered: %s", got)
	}
	want := "git clone -- [redacted-arg]: remote: see " + Redacted
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	e.Stderr = ""
	e.Err = errors.New("wrapped https://u:s3cr3t@example.invalid/")
	if got := e.Error(); strings.Contains(got, "s3cr3t") {
		t.Errorf("credential in the Err fallback: %s", got)
	}
}

// TestCommandError_ErrorKeepsPlainArgv: an argv with nothing URL-shaped
// renders exactly as before, including git's reflog '@{' syntax and a tmux
// window id; a plain repository URL renders as host/owner/repo.
func TestCommandError_ErrorKeepsPlainArgv(t *testing.T) {
	e := &CommandError{Name: "git", Args: []string{"rev-list", "--count", "@{upstream}..HEAD", "main@{u}", "@8"}, Stderr: "fatal: no upstream"}
	if got, want := e.Error(), "git rev-list --count @{upstream}..HEAD main@{u} @8: fatal: no upstream"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	e = &CommandError{Name: "git", Args: []string{"clone", "--", "https://github.com/o/r", "/tmp/x"}, Err: errors.New("exit status 128")}
	if got, want := e.Error(), "git clone -- github.com/o/r /tmp/x: exit status 128"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
