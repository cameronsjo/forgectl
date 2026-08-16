package tmux

import (
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/termsafe/termsafetest"
)

// TestBuildTreeEmitsNoUnsafeRunes pins the tree renderer. A session, window,
// or pane name is chosen by whoever created the object — any same-uid process,
// including a hostile repo's tooling — and `forgectl tmux tree` prints all
// three, so the assembled tree is a terminal-output boundary.
func TestBuildTreeEmitsNoUnsafeRunes(t *testing.T) {
	hostile := termsafetest.Hostile
	sessions := []Session{{ServerPID: "1", ServerStart: "2", ID: "$0", Name: hostile("work"), Attached: true}}
	windows := []Window{{
		ServerPID: "1", ServerStart: "2", ID: "@0", SessionID: "$0",
		Session: hostile("work"), Index: 0, Name: hostile("edit"), Active: true, Panes: 2,
	}}
	panes := []Pane{
		{ServerPID: "1", ServerStart: "2", ID: "%0", WindowID: "@0", Index: 0, Command: hostile("vim"), Active: true},
		// Command empty on purpose: buildTree falls back to Title, so the
		// fallback branch is a sink of its own and needs its own coverage.
		{ServerPID: "1", ServerStart: "2", ID: "%1", WindowID: "@0", Index: 1, Title: hostile("logs")},
	}

	out := buildTree(sessions, windows, panes, asciiTreeMarkers)
	termsafetest.AssertInert(t, "tmux tree", out)

	// The benign part of every name must still be legible — neutralizing must
	// not degrade into dropping the row.
	for _, want := range []string{"work", "edit", "vim", "logs"} {
		if !strings.Contains(out, want) {
			t.Errorf("tree lost the legible part of a name: want %q in\n%s", want, out)
		}
	}
}

// TestBuildTreeLeavesOrdinaryNamesByteIdentical is the other half of the
// contract: neutralizing must be a no-op on names that were never dangerous,
// so the ordinary tree an operator reads every day is unchanged.
func TestBuildTreeLeavesOrdinaryNamesByteIdentical(t *testing.T) {
	sessions := []Session{{ID: "$0", Name: "forge", Attached: true}}
	windows := []Window{{ID: "@0", SessionID: "$0", Session: "forge", Index: 1, Name: "pr-o-r-1", Panes: 1}}
	panes := []Pane{{ID: "%0", WindowID: "@0", Index: 0, Command: "claude"}}

	want := "* forge\n  1: pr-o-r-1 (1 pane)\n    0: claude"
	if got := buildTree(sessions, windows, panes, asciiTreeMarkers); got != want {
		t.Errorf("ordinary tree changed shape:\ngot  %q\nwant %q", got, want)
	}
}
