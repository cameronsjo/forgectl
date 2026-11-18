//go:build unix

package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"

	internalexec "github.com/cameronsjo/forgectl/internal/exec"
)

// rendersSeparatorEscaped reports whether this tmux prints a 0x1F in -F output
// as the four characters `\037` instead of the raw byte. It runs `-u`, as
// Client.tmuxArgs does, so it measures the rendering the parsers actually see.
//
// This, not name storage, is what separates tmux 3.4 from 3.6a here. Both
// ACCEPT a name carrying 0x1F and both store it as the text `\037` (measured:
// #{n:window_name} is 10 for "hid<0x1F>den" on 3.4 and on 3.6a), so no stored
// name holds the byte. 3.4 then renders every real separator as `\037` too,
// which makes the stored text indistinguishable from a separator: the row
// gains fields and is dropped as unreadable. 3.6a renders the separator raw, so
// the same name is plain text and the row stays readable. 3.7 and later refuse
// the name outright. A caller branches on this to pick the outcome that
// rendering must produce. The rendering table in format.go (escapedFieldSep)
// records the versions measured.
//
// It probes with list-sessions, the command family the parsers read, and needs
// a running server, so call it after the test has seeded one.
func rendersSeparatorEscaped(t *testing.T, runner internalexec.Runner, tmuxBin string) bool {
	t.Helper()
	out, err := runner.Run(context.Background(), tmuxBin, "-u", "list-sessions", "-F", "x"+FieldSep+"y")
	if err != nil {
		t.Fatalf("list-sessions: %v", err)
	}
	line, _, _ := strings.Cut(out, "\n")
	switch line {
	case "x" + FieldSep + "y":
		return false
	case "x" + escapedFieldSep + "y":
		return true
	default:
		t.Fatalf("list-sessions rendered 0x1F as %q, which is neither raw nor %q", line, escapedFieldSep)
		return false
	}
}

// skipUnreadableRowAbsent ends a test whose subject, a row tmux could not
// render readably, cannot exist on a tmux that renders the separator raw. The
// readable outcome was asserted first, so this is a skip with a reason rather
// than a PASS for a check that never ran, as pane_rows_integration_test.go does
// for the same gap. The parse itself stays covered by the unit tests over the
// row parsers (TestParseWindowRowsCountsDroppedRows, all_unreadable_test.go),
// which feed them rows whose name carries the separator, in both renderings.
func skipUnreadableRowAbsent(t *testing.T, runner internalexec.Runner, tmuxBin, what string) {
	t.Helper()
	version, _ := runner.Run(context.Background(), tmuxBin, "-V")
	t.Skipf("%s renders the field separator raw, so %s is plain text and its row is readable "+
		"(asserted above); there is no unreadable row to count on this tmux", strings.TrimSpace(version), what)
}

// TestOnlyWindowWithFieldSepIsAnEmptyListingIsolated is forgectl#826 on a
// real tmux: a server whose only window is renamed to carry 0x1F lists no
// window and counts one, instead of failing with the locale error. tmux 3.7
// and later refuse such a name, so the test skips there, as
// TestWindowWithFieldSepIsCountedIsolated does. A tmux that renders the
// separator raw (3.6a) stores the name as the text `\037`, which is not the
// separator, so the window lists as one readable row; see
// rendersSeparatorEscaped. This test asserts that outcome there and skips,
// because the empty listing it exists to check cannot be built.
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
			t.Skipf("%s refuses a window name carrying 0x1F, so there is no such window: %v", strings.TrimSpace(version), err)
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
// the separator raw (3.6a) the row is readable, the test asserts that and
// skips, because the unreadable row it exists to check cannot be built there.
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
			t.Skipf("%s refuses a session name carrying 0x1F, so there is no such session "+
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
