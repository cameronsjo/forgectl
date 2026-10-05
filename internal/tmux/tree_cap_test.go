package tmux

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// TestBuildTreeCapsEachName is #934 for the tmux tree: a session, window or
// pane name any same-uid process chose is escaped AND bounded, so one
// 100k-rune name cannot flood the terminal. Each line keeps its head and says
// it was cut.
//
// Mutation that turns it red: print w.Name (or s.Name, or the pane command)
// through termsafe.SafeLine instead of SafeLineMax in buildTree.
func TestBuildTreeCapsEachName(t *testing.T) {
	long := func(head string) string { return head + strings.Repeat("x\u202e", 50_000) }
	sessions := []Session{{ServerPID: "1", ServerStart: "2", ID: "$0", Name: long("SESS"), Attached: true}}
	windows := []Window{{
		ServerPID: "1", ServerStart: "2", ID: "@0", SessionID: "$0",
		Session: "s", Index: 0, Name: long("WIN"), Active: true, Panes: 1,
	}}
	panes := []Pane{{ServerPID: "1", ServerStart: "2", ID: "%0", WindowID: "@0", Index: 0, Command: long("CMD")}}

	out := buildTree(sessions, windows, panes, asciiTreeMarkers)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("tree has %d lines, want 3:\n%.300s", len(lines), out)
	}
	for i, head := range []string{"SESS", "WIN", "CMD"} {
		line := lines[i]
		if n := utf8.RuneCountInString(line); n > treeNameMaxRunes+64 {
			t.Errorf("line %d is %d runes; want the name capped at %d", i, n, treeNameMaxRunes)
		}
		if !strings.Contains(line, head) || !strings.Contains(line, termsafe.TruncatedMarker) {
			t.Errorf("line %d = %.120q; want its head %q and the truncation marker", i, line, head)
		}
	}
}
