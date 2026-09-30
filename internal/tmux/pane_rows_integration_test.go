//go:build unix

package tmux

import (
	"context"
	"testing"
	"time"
)

// TestPaneWithFieldSepCommandIsCountedIsolated is forgectl#823 item 2 on a
// real tmux. A pane title cannot carry 0x1F (select-pane -T and the title
// escape both refuse it on 3.4 and 3.7c; on 3.5a and older the literal text
// \037 in a title hides its row instead, see parsePaneRows), but
// #{pane_current_command} is what the running program calls itself, and on
// Linux that is argv[0]. A program started under `exec -a` with 0x1F in its
// name drops its pane out of the listing, and DisplayPaneListing must count
// it.
//
// macOS reports the executable's own name there rather than argv[0], so no
// such pane can be made on the CI runner, and the test skips when the pane
// comes back readable. TestParsePaneRowsCountsUnreadableRows covers the
// counting on every platform.
//
// Mutation that turns it red: return 0 in place of the count from
// parsePaneRows.
func TestPaneWithFieldSepCommandIsCountedIsolated(t *testing.T) {
	c, runner, tmuxBin := isolatedTmux(t)
	ctx := context.Background()
	if _, err := runner.Run(ctx, tmuxBin, "new-session", "-d", "-s", "base", "sleep 60"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	window, err := runner.Run(ctx, tmuxBin, "new-window", "-d", "-P", "-F", "#{window_id}", "-t", "base:",
		"--", "bash", "-c", `exec -a "$(printf 'a\037b')" sleep 60`)
	if err != nil {
		t.Fatalf("new-window: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		panes, unreadable, err := c.DisplayPaneListing(ctx)
		if err != nil {
			t.Fatalf("DisplayPaneListing: %v", err)
		}
		if unreadable == 1 {
			if len(panes) != 1 {
				t.Fatalf("DisplayPaneListing = %+v, want only the seed pane readable", panes)
			}
			return
		}
		if unreadable != 0 {
			t.Fatalf("DisplayPaneListing counted %d unreadable panes, want 1", unreadable)
		}
		if time.Now().After(deadline) {
			for _, p := range panes {
				if p.WindowID == window && p.Command == "sleep" {
					t.Skipf("this platform reports %q, not argv[0], as the pane command, so no pane can carry 0x1F there", p.Command)
				}
			}
			t.Fatalf("DisplayPaneListing = %+v with none counted unreadable, want the exec -a pane counted", panes)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
