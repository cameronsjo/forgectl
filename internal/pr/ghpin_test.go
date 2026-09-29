package pr

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
)

// TestPinReviewWindowEnv_GitHubComIsUnchanged: on github.com the pin adds
// nothing, so a github.com review's window argv is what it was before #673.
func TestPinReviewWindowEnv_GitHubComIsUnchanged(t *testing.T) {
	in := []string{"HTTPS_PROXY=http://proxy.example:8080", "GH_HOST=ambient.example"}
	got := pinReviewWindowEnv(in, "github.com")
	if !slices.Equal(got, in) {
		t.Errorf("pinReviewWindowEnv on github.com = %v, want %v unchanged", got, in)
	}
	if got := pinReviewWindowEnv(nil, "github.com"); len(got) != 0 {
		t.Errorf("pinReviewWindowEnv(nil, github.com) = %v, want empty", got)
	}
}

// TestPinReviewWindowEnv_EnterpriseHostPinsAndEmptiesTokens is the window half
// of forgectl#673: on a GHE host the window must name that host in GH_HOST and
// carry every token variable empty, and a caller-supplied GH_HOST or token
// entry must not survive to override the pin.
func TestPinReviewWindowEnv_EnterpriseHostPinsAndEmptiesTokens(t *testing.T) {
	const ghe = "ghe.corp.example"
	in := []string{
		"HTTPS_PROXY=http://proxy.example:8080",
		"GH_HOST=evil.example",
		"GH_ENTERPRISE_TOKEN=leak",
	}
	got := pinReviewWindowEnv(in, ghe)

	want := map[string]string{
		"HTTPS_PROXY":             "http://proxy.example:8080",
		"GH_HOST":                 ghe,
		"GH_TOKEN":                "",
		"GITHUB_TOKEN":            "",
		"GH_ENTERPRISE_TOKEN":     "",
		"GITHUB_ENTERPRISE_TOKEN": "",
	}
	seen := map[string]int{}
	for _, e := range got {
		k, v, _ := strings.Cut(e, "=")
		seen[k]++
		if w, ok := want[k]; !ok || w != v {
			t.Errorf("window env entry %q, want %s=%q", e, k, w)
		}
	}
	for k := range want {
		if seen[k] != 1 {
			t.Errorf("%s appears %d times in %v, want exactly once", k, seen[k], got)
		}
	}
	if in[1] != "GH_HOST=evil.example" {
		t.Errorf("pinReviewWindowEnv mutated its input: %v", in)
	}
}

// TestLaunch_EnterprisePRWindowIsPinned drives the dispatch end to end: a
// review of a GHE-hosted PR opens its window with GH_HOST pinned and no token
// value, and its prompt names the exact gh reads the allow-list permits.
func TestLaunch_EnterprisePRWindowIsPinned(t *testing.T) {
	claudeBin := fakeHarnessBin(t, "claude")
	t.Setenv("FORGECTL_CLAUDE_BIN", claudeBin)

	fake := successfulLaunchRunner()
	c := New(fake,
		WithSessionsDir(os.TempDir()),
		WithTmuxSession("forgectl"),
		WithWindowEnv(func() ([]string, error) {
			return []string{"HTTPS_PROXY=http://sentinel.example:8080"}, nil
		}),
	)
	ref := Ref{Host: "ghe.corp.example", Owner: "base", Repo: "r", Number: 42}
	sess := Session{Ref: ref, Workspace: fakeWorkspace(t), Agent: "claude"}

	if _, err := c.Launch(context.Background(), sess, config.Config{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	args := fake.Last().Args
	if !argPair(args, "-e", "GH_HOST=ghe.corp.example") {
		t.Errorf("window env does not pin GH_HOST to the PR's host: %v", args)
	}
	for _, k := range GHTokenEnvVars {
		if !argPair(args, "-e", k+"=") {
			t.Errorf("window env does not empty %s: %v", k, args)
		}
	}
	if !argPair(args, "-e", "HTTPS_PROXY=http://sentinel.example:8080") {
		t.Errorf("the pin dropped the resolved window env: %v", args)
	}
	if !contains(args, remoteReviewPrompt("ghe.corp.example", ref)) {
		t.Errorf("argv missing the remote review prompt: %v", args)
	}
	if !strings.Contains(remoteReviewPrompt("ghe.corp.example", ref), "gh pr view 42 --repo ghe.corp.example/base/r") {
		t.Errorf("remote prompt does not name the base-repo gh read: %q", remoteReviewPrompt("ghe.corp.example", ref))
	}
}

// TestLaunchCodex_EnterprisePRWindowIsPinned covers the Codex dispatch's
// window-env site. Launch refuses a remote PR on Codex (provenance), so no
// route reaches it with a remote ref today; calling launchCodex directly keeps
// the pin in place for the day that gate relaxes, rather than leaving one
// dispatch site with the ambient token environment.
func TestLaunchCodex_EnterprisePRWindowIsPinned(t *testing.T) {
	t.Setenv("FORGECTL_CODEX_BIN", fakeHarnessBin(t, "codex"))

	fake := successfulLaunchRunner()
	c := New(fake, WithSessionsDir(os.TempDir()), WithTmuxSession("forgectl"))
	sess := Session{
		Ref:       Ref{Host: "ghe.corp.example", Owner: "base", Repo: "r", Number: 42},
		Workspace: fakeWorkspace(t),
		Agent:     "codex",
	}
	if _, err := c.launchCodex(context.Background(), sess, config.Config{}); err != nil {
		t.Fatalf("launchCodex: %v", err)
	}
	args := fake.Last().Args
	if !argPair(args, "-e", "GH_HOST=ghe.corp.example") || !argPair(args, "-e", "GH_ENTERPRISE_TOKEN=") {
		t.Errorf("Codex window env is not pinned to the PR's host: %v", args)
	}
}
