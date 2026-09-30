package tmux

import (
	"context"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

func paneRow(id, windowID, title, command string) string {
	return strings.Join([]string{"1", "2", id, windowID, "0", title, command, "1"}, FieldSep)
}

// TestParsePaneRowsCountsUnreadableRows is forgectl#823 item 2: a pane row
// that does not split into exactly paneFieldCount fields, or carries a
// malformed id, is dropped AND counted, the way session and window rows are.
// A pane's command is whatever its program calls itself, so it can carry
// FieldSep. Blank lines are not rows and are not counted.
//
// Mutation that turns it red: return 0 in place of the count from
// parsePaneRows.
func TestParsePaneRowsCountsUnreadableRows(t *testing.T) {
	out := strings.Join([]string{
		paneRow("%0", "@0", "ok", "zsh"),
		paneRow("%1", "@0", "t", "a"+FieldSep+"b"),
		paneRow("bogus", "@0", "t", "zsh"),
		"",
	}, "\n")
	panes, unreadable, err := parsePaneRows(out)
	if err != nil {
		t.Fatalf("parsePaneRows: %v", err)
	}
	if len(panes) != 1 || panes[0].ID != "%0" {
		t.Fatalf("panes = %+v, want only the readable row", panes)
	}
	if unreadable != 2 {
		t.Fatalf("unreadable = %d, want 2", unreadable)
	}
}

// TestDisplayPaneListingCountsUnreadableRows: the count reaches the listing
// that `tmux tree --json` reads.
//
// Mutation that turns it red: return 0 in place of the count from listPanes.
func TestDisplayPaneListingCountsUnreadableRows(t *testing.T) {
	fake := &exec.FakeRunner{RunFunc: func(_ string, _ []string) (string, error) {
		return paneRow("%0", "@0", "ok", "zsh") + "\n" + paneRow("%1", "@0", "t", "a"+FieldSep+"b"), nil
	}}
	panes, unreadable, err := New(fake).DisplayPaneListing(context.Background())
	if err != nil {
		t.Fatalf("DisplayPaneListing: %v", err)
	}
	if len(panes) != 1 || unreadable != 1 {
		t.Fatalf("DisplayPaneListing = %d panes, %d unreadable; want 1 and 1", len(panes), unreadable)
	}
}
