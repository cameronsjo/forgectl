//go:build unix

package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
)

// TestOnlyWindowWithFieldSepIsAnEmptyListingIsolated is forgectl#826 on a
// real tmux: a server whose only window is renamed to carry 0x1F lists no
// window and counts one, instead of failing with the locale error. tmux 3.7
// and later refuse such a name, so the test skips there, as
// TestWindowWithFieldSepIsCountedIsolated does.
//
// Mutation that turns it red: drop the separator-survived loop from
// parsedRows.
func TestOnlyWindowWithFieldSepIsAnEmptyListingIsolated(t *testing.T) {
	c, runner, tmuxBin := isolatedTmux(t)
	ctx := context.Background()
	if _, err := runner.Run(ctx, tmuxBin, "new-session", "-d", "-s", "base", "sleep 60"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := runner.Run(ctx, tmuxBin, "rename-window", "-t", "base:0", "--", "hid"+FieldSep+"den"); err != nil {
		var cmdErr *internalexec.CommandError
		if errors.As(err, &cmdErr) && strings.HasPrefix(cmdErr.Stderr, "invalid window name: ") {
			version, _ := runner.Run(ctx, tmuxBin, "-V")
			t.Skipf("%s refuses a window name carrying 0x1F, so there is no such window: %v", strings.TrimSpace(version), err)
		}
		t.Fatalf("rename-window: %v", err)
	}
	windows, unreadable, err := c.DisplayWindowListing(ctx)
	if err != nil {
		t.Fatalf("DisplayWindowListing: %v", err)
	}
	if len(windows) != 0 || unreadable != 1 {
		t.Fatalf("DisplayWindowListing = %+v, %d unreadable; want none and 1", windows, unreadable)
	}
	if _, rows, err := c.TreeListing(ctx, false); err != nil || rows.Windows != 1 {
		t.Fatalf("TreeListing = (%+v, %v), want 1 unreadable window and no error", rows, err)
	}
}

// TestNeverAttachedUnreadableSessionIsEmptyIsolated is the lastAttachedFormat
// half of forgectl#836 on a real tmux. The only session has never been
// attached, so tmux renders its #{session_last_attached} as "", and its name
// carries FieldSep, so its row is unreadable. mostRecentSession must report
// no session plus one unreadable row. It must not report the locale error: a
// format led by the empty sort key has no decimal first field to prove the
// separator survived.
//
// Mutation that turns it red: move #{session_last_attached} back to the front
// of lastAttachedFormat (and the field indices with it).
func TestNeverAttachedUnreadableSessionIsEmptyIsolated(t *testing.T) {
	c, runner, tmuxBin := isolatedTmux(t)
	ctx := context.Background()
	if _, err := runner.Run(ctx, tmuxBin, "new-session", "-d", "-s", "a"+FieldSep+"b", "sleep 60"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, unreadable, err := c.mostRecentSession(ctx)
	if err != nil {
		t.Fatalf("mostRecentSession: %v; want no session and one unreadable row", err)
	}
	if got.ID != "" || unreadable != 1 {
		t.Fatalf("mostRecentSession = %+v, %d unreadable; want none and 1", got, unreadable)
	}
}
