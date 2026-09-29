package review

import (
	"context"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// TestNewGitea_HostLengthCap pins the #562 cap: a charset-valid host longer
// than the 253-byte DNS limit is refused without being echoed, and a host at
// exactly the limit is accepted.
func TestNewGitea_HostLengthCap(t *testing.T) {
	// reGiteaHost bounds no label, so a run of letters is charset-valid at
	// any length — the refusal below can only come from the length cap.
	atLimit := strings.Repeat("a", MaxGiteaHostBytes)
	overLimit := "MARKER" + strings.Repeat("a", MaxGiteaHostBytes+1-len("MARKER"))
	if len(atLimit) != MaxGiteaHostBytes || len(overLimit) != MaxGiteaHostBytes+1 {
		t.Fatalf("fixture lengths %d/%d", len(atLimit), len(overLimit))
	}
	if !reGiteaHost.MatchString(overLimit) {
		t.Fatal("over-limit fixture must be charset-valid, or the test proves only the charset")
	}

	if _, err := NewGitea(&exec.FakeRunner{}, atLimit, "cameron", nil); err != nil {
		t.Fatalf("host of exactly %d bytes: %v, want accepted", MaxGiteaHostBytes, err)
	}
	_, err := NewGitea(&exec.FakeRunner{}, overLimit, "cameron", nil)
	if err == nil {
		t.Fatalf("host of %d bytes accepted, want refused", len(overLimit))
	}
	if strings.Contains(err.Error(), "MARKER") {
		t.Fatalf("refusal %q echoes the host", err)
	}
}

// TestEcho_ConfiguredOwnerIsCategorical: an owner from config that fails the
// charset is refused on both sources without being echoed (#562).
func TestEcho_ConfiguredOwnerIsCategorical(t *testing.T) {
	hostile := "MARKER" + strings.Repeat("\x1b[2J", 2000)
	g, err := NewGitea(&exec.FakeRunner{}, "git.example.test", "cameron", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, giteaErr := g.issuesForOwner(context.Background(), hostile)
	_, _, githubErr := NewGitHub(&exec.FakeRunner{}, nil, "").searchIssues(context.Background(), hostile)
	for name, err := range map[string]error{"gitea": giteaErr, "github": githubErr} {
		if err == nil {
			t.Fatalf("%s: hostile owner accepted", name)
		}
		if msg := err.Error(); strings.Contains(msg, "MARKER") || strings.Contains(msg, "\x1b") || strings.Contains(msg, `\x1b`) {
			t.Fatalf("%s: error %q echoes the configured owner", name, msg)
		}
	}
}

// TestEcho_WorkRefArgvIsCapped: `review mark/unmark` argv is echoed back so
// the operator sees the typo, but capped and escaped (#562).
func TestEcho_WorkRefArgvIsCapped(t *testing.T) {
	for _, in := range []string{
		"MARKER" + strings.Repeat("\x1b[2J\u202e", 1500),                         // unrecognized form
		"MARKER" + strings.Repeat("a", 5000) + ".test/o/r#1",                // unconfigured host, slug form
		"https://MARKER" + strings.Repeat("a", 5000) + ".test/o/r/issues/1", // unconfigured host, URL form
	} {
		_, err := ParseWorkRefForHosts(in, GitHubHost, nil)
		if err == nil {
			t.Fatalf("ParseWorkRefForHosts(%.20q…) accepted", in)
		}
		msg := err.Error()
		if len(msg) > 2048 {
			t.Fatalf("error is %d bytes, want a capped echo", len(msg))
		}
		if !strings.Contains(msg, "MARKER") {
			t.Fatalf("error %q does not echo the typed reference", msg)
		}
		for _, r := range msg {
			if termsafe.IsUnsafeTerminalRune(r) {
				t.Fatalf("error %q carries raw rune %U", msg, r)
			}
		}
	}
}
