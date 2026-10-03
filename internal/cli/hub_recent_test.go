package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/history"
	"github.com/cameronsjo/forgectl/internal/module"
)

func historyOf(lines ...string) []history.Entry {
	out := make([]history.Entry, 0, len(lines))
	for _, l := range lines {
		out = append(out, history.Entry{Command: l})
	}
	return out
}

func cmdPaths(cmds []*cobra.Command) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, strings.Join(commandArgv(c), " "))
	}
	return out
}

// TestRecentCommands_RanksByCountThenRecency pins the recent section's
// ranking: frequency first, the newer command on a tie, pinned bare modules
// and non-forgectl lines left out, and aliases and a path-qualified binary
// resolved to the canonical command.
func TestRecentCommands_RanksByCountThenRecency(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	entries := historyOf(
		"forgectl doctor",
		"forgectl pr prs",
		"git status",
		"forgectl pr",   // pinned bare module: already a row
		"forgectl docs", // pinned bare module
		"forgectl resume ls --limit 5",
		"/usr/local/bin/forgectl doctor",
		"forgectl pr prs --json",
		"forgectl upgrade",
	)
	got := cmdPaths(recentCommands(root, entries, 3))
	// doctor and pr prs both appear twice; pr prs is newer. upgrade (once,
	// newest) outranks resume ls (once, older).
	want := []string{"pr prs", "doctor", "upgrade"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("recentCommands = %v, want %v", got, want)
	}
}

// TestRecentCommands_WindowIsTheNewestForgectlLines pins hubRecentWindow: a
// command seen only before the newest window of forgectl lines drops out,
// however often it ran back then.
func TestRecentCommands_WindowIsTheNewestForgectlLines(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	var lines []string
	for range 5 {
		lines = append(lines, "forgectl upgrade")
	}
	for range hubRecentWindow {
		lines = append(lines, "forgectl doctor")
		lines = append(lines, "echo not forgectl") // non-forgectl lines do not use up the window
	}
	got := cmdPaths(recentCommands(root, historyOf(lines...), 3))
	if strings.Join(got, "|") != "doctor" {
		t.Errorf("recentCommands = %v, want only [doctor]: upgrade ran before the %d-line window", got, hubRecentWindow)
	}
}

// TestHistoryCommand_NeverSurfacesHistoryText pins the trust boundary: only a
// command registered in the tree comes back, never text from the line.
func TestHistoryCommand_NeverSurfacesHistoryText(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	for _, tc := range []struct {
		line       string
		want       string // "" = no command
		isForgectl bool
	}{
		{"forgectl pr prs", "pr prs", true},
		{"forgectl proj ls", "projects list", true},
		{"forgectl sessions last cadence", "sessions last", true},
		{"forgectl $(rm -rf ~)", "", true},
		{"forgectl \x1b]0;pwned\x07", "", true},
		{"forgectl --no-icons pr prs", "", true},
		{"forgectl env", "", true}, // a group, not runnable on its own
		{"forgectl pr prs\nrm -rf ~", "pr prs", true},
		{"forgectlx doctor", "", false},
		{"echo forgectl doctor", "", false},
		{"", "", false},
	} {
		cmd, isForgectl := historyCommand(root, tc.line)
		got := ""
		if cmd != nil {
			got = strings.Join(commandArgv(cmd), " ")
		}
		if got != tc.want || isForgectl != tc.isForgectl {
			t.Errorf("historyCommand(%q) = (%q, %v), want (%q, %v)", tc.line, got, isForgectl, tc.want, tc.isForgectl)
		}
	}
}

// TestRecentCommands_PinnedModulesStayOut pins that a pinned module's bare
// invocation never takes a recent slot, however often it ran — it already
// has a row above.
func TestRecentCommands_PinnedModulesStayOut(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	var lines []string
	for range 10 {
		lines = append(lines, "forgectl pr", "forgectl docs", "forgectl projects", "forgectl tmux", "forgectl sessions")
	}
	lines = append(lines, "forgectl doctor")
	got := cmdPaths(recentCommands(root, historyOf(lines...), 3))
	if strings.Join(got, "|") != "doctor" {
		t.Errorf("recentCommands = %v, want only [doctor]: pinned modules have rows already", got)
	}
}

// TestRecentCommands_NonModuleVerbsStayOut pins that a root child with no hub
// tier — menu, version — never fills the recent section, however often it
// ran: it is host plumbing, not a hub row. Mutation that turns it red: drop
// the tier check in recentCommands.
func TestRecentCommands_NonModuleVerbsStayOut(t *testing.T) {
	root := newRoot(module.Deps{Runner: &exec.FakeRunner{}})
	entries := historyOf(
		"forgectl pr prs",
		"forgectl menu", "forgectl menu --json", "forgectl menu",
		"forgectl version", "forgectl version", "forgectl version --json",
	)
	got := cmdPaths(recentCommands(root, entries, 3))
	if strings.Join(got, "|") != "pr prs" {
		t.Errorf("recentCommands = %v, want only [pr prs]", got)
	}
}
