package ghfail

import (
	"context"
	"errors"
	"fmt"
	osexec "os/exec"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func cmdErr(stderr string, code int) error {
	return fmt.Errorf("search: %w", &exec.CommandError{Name: "gh", Stderr: stderr, ExitCode: code, Err: errors.New("exit status")})
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want Cause
	}{
		{"not signed in, by exit code", cmdErr("", ghExitAuth), NotSignedIn},
		{"not signed in, by text", cmdErr("To get started with GitHub CLI, please run:  gh auth login", 1), NotSignedIn},
		{"token rejected", cmdErr("HTTP 401: Bad credentials (https://api.github.com/graphql)", 1), TokenRejected},
		{"rate limited", cmdErr("HTTP 403: API rate limit exceeded for user ID 1.", 1), RateLimited},
		{"unreachable", cmdErr("error connecting to api.github.com\ncheck your internet connection", 1), Unreachable},
		{"missing binary", &exec.CommandError{Name: "gh", ExitCode: -1, Err: &osexec.Error{Name: "gh", Err: osexec.ErrNotFound}}, Missing},
		{"deadline", fmt.Errorf("gh: %w", context.DeadlineExceeded), Timeout},
		{"canceled", context.Canceled, Canceled},
		{"unrecognized stderr", cmdErr("something else entirely", 1), Other},
		{"not a command error", errors.New("parse gh search prs output: bad json"), Other},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.err); got != tc.want {
				t.Errorf("Classify = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestNoteNeverRendersStderr pins the guarantee the package exists for: a
// note carries fixed text only, whatever gh wrote.
func TestNoteNeverRendersStderr(t *testing.T) {
	const hostile = "gh auth login \x1b]52;c;ZXZpbA==\x07 ghp_secret123"
	got := Note("prs(acme)", cmdErr(hostile, 1), "github.com")
	if strings.Contains(got, "\x1b") || strings.Contains(got, "ghp_secret123") {
		t.Fatalf("Note rendered gh stderr: %q", got)
	}
	if want := "prs(acme): query failed (gh is not signed in to github.com; run gh auth login)"; got != want {
		t.Errorf("Note = %q, want %q", got, want)
	}
}

func TestReasonHost(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_ENTERPRISE_TOKEN", "")
	t.Setenv("GITHUB_ENTERPRISE_TOKEN", "")
	auth := cmdErr("", ghExitAuth)
	cases := []struct{ host, want string }{
		{"", "gh is not signed in to github.com; run gh auth login"},
		{"github.example.com", "gh is not signed in to github.example.com; run gh auth login --hostname github.example.com"},
		{"evil\x1b[2J.com", "gh is not signed in to the GitHub host; run gh auth login"},
	}
	for _, tc := range cases {
		if got := Reason(auth, tc.host); got != tc.want {
			t.Errorf("Reason(host %q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

func TestReasonTokenRejectedNamesTheVariable(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_ENTERPRISE_TOKEN", "")
	t.Setenv("GITHUB_ENTERPRISE_TOKEN", "")
	rejected := cmdErr("HTTP 401: Bad credentials", 1)

	t.Setenv("GH_TOKEN", "x")
	if got, want := Reason(rejected, ""), "github.com rejected the token in GH_TOKEN; replace it, or unset it and run gh auth login"; got != want {
		t.Errorf("with GH_TOKEN set: %q, want %q", got, want)
	}
	t.Setenv("GH_TOKEN", "")
	if got, want := Reason(rejected, ""), "github.com rejected gh's credential; run gh auth login"; got != want {
		t.Errorf("with no token variable: %q, want %q", got, want)
	}
}

func TestReasonOtherPointsAtDoctor(t *testing.T) {
	if got, want := Note("issues(acme)", errors.New("boom"), ""), "issues(acme): query failed (run forgectl doctor for the cause)"; got != want {
		t.Errorf("Note = %q, want %q", got, want)
	}
}

// TestCategorizedSurvivesAWrap: a caller that drops the raw error and wraps
// Categorize(err) keeps the cause for Classify, and its text is fixed.
func TestCategorizedSurvivesAWrap(t *testing.T) {
	wrapped := fmt.Errorf("login unavailable: %w", Categorize(cmdErr("ghp_secret123", ghExitAuth)))
	if got := Classify(wrapped); got != NotSignedIn {
		t.Errorf("Classify = %d, want NotSignedIn", got)
	}
	if strings.Contains(wrapped.Error(), "ghp_secret123") {
		t.Errorf("Categorize carried gh's text: %q", wrapped.Error())
	}
}
