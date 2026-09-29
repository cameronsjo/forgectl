package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/module"
)

// TestPrPrs_SearchIsPinnedToConfiguredHost is the #413 wiring check: the @me
// searches behind `pr prs` name no repository, so newPrCmd must route them
// through githubauth.Runner on [github] host. An ambient GH_HOST naming some
// other host must not win, and on a non-default host the ambient token
// variables are removed from the child — the same pin `review` applies.
func TestPrPrs_SearchIsPinnedToConfiguredHost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GH_HOST", "ambient.example.test")

	fake := &exec.FakeRunner{RunFunc: prsRunFunc("[" + prSearchRow("platform/tools", 7) + "]")}
	var cfg config.Config
	cfg.Github.Host = "GHE.Example.test"

	cmd := newPrCmd(module.Deps{Cfg: cfg, Runner: fake})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"prs", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("pr prs --json: %v (stderr %q)", err, stderr.String())
	}

	var searches int
	for _, call := range fake.Calls {
		if call.Name != "gh" || len(call.Args) < 2 || call.Args[0] != "search" {
			continue
		}
		searches++
		if got := call.Env["GH_HOST"]; got != "ghe.example.test" {
			t.Errorf("search GH_HOST = %q, want the configured host ghe.example.test", got)
		}
		unset := strings.Join(call.UnsetEnv, " ")
		for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
			if !strings.Contains(unset, k) {
				t.Errorf("search on a non-default host does not remove %s (unset %v)", k, call.UnsetEnv)
			}
		}
	}
	if searches != 3 {
		t.Fatalf("gh search calls = %d, want 3", searches)
	}
	if !strings.Contains(stdout.String(), "platform/tools#7") {
		t.Fatalf("stdout = %q, want the searched PR", stdout.String())
	}
}

// TestPrPrs_InvalidConfiguredHostFailsClosed: a [github] host that fails
// validation must not let the searches run on the ambient host instead. The
// searches degrade (they never spawn), and the rejected value is not echoed.
func TestPrPrs_InvalidConfiguredHostFailsClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)

	fake := &exec.FakeRunner{RunFunc: prsRunFunc("[" + prSearchRow("platform/tools", 7) + "]")}
	var cfg config.Config
	cfg.Github.Host = "evil.test/\x1b[2J"

	cmd := newPrCmd(module.Deps{Cfg: cfg, Runner: fake})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"prs", "--json"})
	_ = cmd.Execute()

	for _, call := range fake.Calls {
		if call.Name == "gh" {
			t.Fatalf("gh ran (%v) under an invalid [github] host", call.Args)
		}
	}
	if out := stdout.String() + stderr.String(); strings.Contains(out, "evil.test") || strings.Contains(out, "\x1b") {
		t.Fatalf("output echoes the rejected host: %q", out)
	}
}
