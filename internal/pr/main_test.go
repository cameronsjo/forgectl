package pr

import (
	"os"
	"testing"
)

// TestMain gives every test in this package a host where Claude Code's
// sandbox is available, so a test about something else never depends on
// whether the machine running it has bubblewrap installed. It swaps the
// probe's inputs rather than the probe: claudeSandboxSupported itself still
// runs on every dispatch, and TestClaudeSandboxSupported drives its platform
// table directly.
func TestMain(m *testing.M) {
	sandboxGOOS = "linux"
	sandboxLookPath = func(file string) (string, error) { return "/usr/bin/" + file, nil }
	os.Exit(m.Run())
}
