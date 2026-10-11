package pr

import (
	"context"
	"os"
	"testing"
)

// TestMain gives every test in this package a host where Claude Code's
// sandbox is available and the installed claude accepts the reviewer's
// settings, so a test about something else never depends on
// whether the machine running it has bubblewrap installed. It swaps the
// probe's inputs rather than the probe: claudeSandboxSupported itself still
// runs on every dispatch, and TestClaudeSandboxSupported drives its platform
// table directly. Likewise claudeAcceptsReviewSettings runs on every dispatch
// against healthyClaudeProbe, and reviewaccept_test.go drives its refusals.
func TestMain(m *testing.M) {
	sandboxGOOS = "linux"
	sandboxLookPath = func(file string) (string, error) { return "/usr/bin/" + file, nil }
	claudeProbe = healthyClaudeProbe
	os.Exit(m.Run())
}

// healthyClaudeProbe answers the dispatch-time checks as a current claude
// that accepts every document but the negative control would.
func healthyClaudeProbe(_ context.Context, _, _ string, _ []string, args ...string) (string, error) {
	return fakeClaudeAnswer("2.1.285 (Claude Code)", false, args), nil
}

// fakeClaudeAnswer is what a claude printing version answers to args: that
// for --version, and for doctor a report that lists the negative control
// under "Invalid settings", and every other document too when rejectAll.
func fakeClaudeAnswer(version string, rejectAll bool, args []string) string {
	if len(args) == 1 && args[0] == "--version" {
		return version
	}
	doc := ""
	for i, a := range args {
		if a == "--settings" && i+1 < len(args) {
			doc = args[i+1]
		}
	}
	report := "Claude Code doctor\n\nRunning: native (2.1.285)\n\n"
	if doc == doctorNegativeControl || (rejectAll && doc != "") {
		report += "Invalid settings\n- /tmp/claude-settings-x.json \u203a sandbox.enabled: Expected boolean, but received string\n\n"
	}
	return report + doctorFooter + "\n"
}
