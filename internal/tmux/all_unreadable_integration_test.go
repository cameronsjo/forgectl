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
// TestWindowWithFieldSepIsCountedIsolated does. A tmux that renders the
// separator raw (3.6a) stores the name as the text `\037`, which is not the
// separator, so the window lists as one readable row; see
// rendersSeparatorEscaped. This test asserts that outcome there and then SKIPS,
// because the empty listing it exists to check cannot be built: a green run on
// 3.6a is a skip after those assertions, not a pass of the original check. The
// original check runs on the ubuntu-24.04 CI leg (tmux 3.4), which sets
// requireEscapedSeparatorEnv so that a skip there fails.
//
// Mutation that turns it red: drop the separator-survived loop from
// parsedRows.
func TestOnlyWindowWithFieldSepIsAnEmptyListingIsolated(t *testing.T) {
	c, runner, tmuxBin := isolatedTmux(t)
	ctx := context.Background()
	if _, err := runner.Run(ctx, tmuxBin, "new-session", "-d", "-s", "base", "sleep 60"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Target the window by its id, not "base:0": the isolated server still
	// reads the user's tmux config, and a base-index of 1 there makes the
	// only window base:1 (forgectl#882).
	idOut, err := runner.Run(ctx, tmuxBin, "display-message", "-p", "-t", "base", "#{window_id}")
	if err != nil {
		t.Fatalf("window id: %v", err)
	}
	windowID := strings.TrimSpace(idOut)
	if _, err := runner.Run(ctx, tmuxBin, "rename-window", "-t", windowID, "--", "hid"+FieldSep+"den"); err != nil {
		var cmdErr *internalexec.CommandError
		if errors.As(err, &cmdErr) && strings.HasPrefix(cmdErr.Stderr, "invalid window name: ") {
			version, _ := runner.Run(ctx, tmuxBin, "-V")
			skipOrRequireEscaped(t, "%s refuses a window name carrying 0x1F, so there is no such window: %v", strings.TrimSpace(version), err)
		}
		t.Fatalf("rename-window: %v", err)
	}
	windows, unreadable, err := c.DisplayWindowListing(ctx)
	if err != nil {
		t.Fatalf("DisplayWindowListing: %v", err)
	}
	if !rendersSeparatorEscaped(t, runner, tmuxBin) {
		wantName := "hid" + escapedFieldSep + "den"
		if len(windows) != 1 || unreadable != 0 || windows[0].Name != wantName {
			t.Fatalf("tmux renders the separator raw, so the stored name is the text %q and DisplayWindowListing = %+v, %d unreadable; want that one window and none unreadable", wantName, windows, unreadable)
		}
		skipUnreadableRowAbsent(t, runner, tmuxBin, "the renamed window's name")
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
// separator survived. That holds only where tmux renders the separator
// escaped (3.4): there the stored text `\037` splits the row. Where it renders
// the separator raw (3.6a) the row is readable, the test asserts that and then
// SKIPS, because the unreadable row it exists to check cannot be built there:
// a green run on 3.6a is a skip, not a pass of the original check. The
// ubuntu-24.04 CI leg (tmux 3.4) runs it, with requireEscapedSeparatorEnv set
// so that a skip there fails.
//
// Mutation that turns it red: move #{session_last_attached} back to the front
// of lastAttachedFormat (and the field indices with it).
func TestNeverAttachedUnreadableSessionIsEmptyIsolated(t *testing.T) {
	c, runner, tmuxBin := isolatedTmux(t)
	ctx := context.Background()
	if _, err := runner.Run(ctx, tmuxBin, "new-session", "-d", "-s", "a"+FieldSep+"b", "sleep 60"); err != nil {
		var cmdErr *internalexec.CommandError
		if errors.As(err, &cmdErr) && strings.HasPrefix(cmdErr.Stderr, "invalid session name: ") {
			version, _ := runner.Run(ctx, tmuxBin, "-V")
			skipOrRequireEscaped(t, "%s refuses a session name carrying 0x1F, so there is no such session "+
				"(TestNeverAttachedUnreadableRowIsEmpty covers the parse): %v", strings.TrimSpace(version), err)
		}
		t.Fatalf("seed: %v", err)
	}
	got, unreadable, err := c.mostRecentSession(ctx)
	if err != nil {
		t.Fatalf("mostRecentSession: %v; want no session and one unreadable row", err)
	}
	if !rendersSeparatorEscaped(t, runner, tmuxBin) {
		// The separator renders raw, so the stored text `\037` is plain: the
		// row is readable.
		wantName := "a" + escapedFieldSep + "b"
		if got.ID == "" || got.Name != wantName || unreadable != 0 {
			t.Fatalf("tmux renders the separator raw, so the stored name is the text %q and mostRecentSession = %+v, %d unreadable; want that session and none unreadable", wantName, got, unreadable)
		}
		skipUnreadableRowAbsent(t, runner, tmuxBin, "the session's name")
	}
	if got.ID != "" || unreadable != 1 {
		t.Fatalf("mostRecentSession = %+v, %d unreadable; want none and 1", got, unreadable)
	}
}
