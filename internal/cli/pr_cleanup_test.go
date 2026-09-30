package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// TestPrCleanup_NamesEverySessionItDidNotDiscard is forgectl#666. A sweep
// carries on past failures, so the stderr report must describe each session
// by its own outcome: the one whose kill timed out (parked), the one skipped
// after that, and a tally that counts the stale session the sweep did remove.
// It must never claim "nothing was removed" for the sweep as a whole, and the
// exit stays non-zero.
func TestPrCleanup_NamesEverySessionItDidNotDiscard(t *testing.T) {
	dir := t.TempDir()
	mkws := func() string {
		ws, err := os.MkdirTemp("", "forgectl-workflow-test-*")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(ws) })
		return ws
	}
	first := seedRepairRecord(t, dir, "o/r#1", "prepared", mkws())
	second := seedRepairRecord(t, dir, "o/r#2", "prepared", mkws())
	gone := mkws()
	stale := seedRepairRecord(t, dir, "o/r#3", "prepared", gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	client := pr.New(blockingTmux{&exec.FakeRunner{}}, pr.WithSessionsDir(dir), pr.WithTmuxSession("forgectl"),
		pr.WithTTYCheck(func() bool { return false }))

	cmd := newPrCleanupCmd(client)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{time.Now().UTC().Format("2006-01-02")})
	// The parent deadline stands in for the package's own (unexported) budget.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err := cmd.ExecuteContext(ctx)
	if err == nil || ExitCode(err) != 1 {
		t.Fatalf("err = %v (exit %d), want a failure exiting 1", err, ExitCode(err))
	}
	if !errors.Is(err, pr.ErrWindowKillTimedOut) {
		t.Errorf("err = %v, want the first failure (a timed-out kill) still reachable", err)
	}
	if !strings.Contains(err.Error(), "2 session(s) not cleaned up") || !strings.Contains(err.Error(), "1 cleaned up") {
		t.Errorf("err = %q, want the tally of 2 failed and 1 cleaned up", err)
	}

	// A bare cobra command also echoes the returned error as "Error: ...";
	// the per-session report is everything else.
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(errOut.String()), "\n") {
		if !strings.HasPrefix(line, "Error: ") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("stderr has %d lines, want one per session not discarded:\n%s", len(lines), errOut.String())
	}
	var timedOut, skipped string
	for _, line := range lines {
		switch {
		case strings.Contains(line, "may still be running") && strings.Contains(line, "is parked as needs-repair"):
			timedOut = line
		case strings.HasPrefix(line, "skipped "):
			skipped = line
		}
	}
	if timedOut == "" || skipped == "" {
		t.Fatalf("stderr = %q, want one parked-timeout line and one skipped line", errOut.String())
	}
	for _, path := range []string{first, second} {
		if !strings.Contains(timedOut, path) && !strings.Contains(skipped, path) {
			t.Errorf("no stderr line names %s:\n%s", path, errOut.String())
		}
	}
	if _, serr := os.Stat(stale); !os.IsNotExist(serr) {
		t.Errorf("the stale record should have been swept: %v", serr)
	}
	if strings.Contains(out.String(), "cleaned up sessions") {
		t.Errorf("stdout = %q, must not report success on a failed sweep", out.String())
	}
}

// TestCleanupFailureLine_AmbiguousWindowSaysWhetherTheRecordWasParked: a
// teardown refused because two windows carry the review's name parks the
// record like a timeout does, and its line must say so, or that it could not.
func TestCleanupFailureLine_AmbiguousWindowSaysWhetherTheRecordWasParked(t *testing.T) {
	refused := fmt.Errorf("refusing to tear down o/r#1, nothing was removed: %w", tmux.ErrAmbiguousWindow)
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"parked", refused, "the record is parked as needs-repair"},
		{"not parked", fmt.Errorf("%w; %w", refused, pr.ErrRecordNotParked), "could not be parked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := cleanupFailureLine(pr.CleanupFailure{Path: "/s/o-r-1-1.json", Err: tc.err})
			if !strings.Contains(line, "more than one tmux window") || !strings.Contains(line, tc.want) {
				t.Errorf("line = %q, want the duplicate-name wording and %q", line, tc.want)
			}
			// More than one can mean more than two, so the line never says "neither" (#712).
			if !strings.Contains(line, "so none was killed") || strings.Contains(line, "neither") {
				t.Errorf("line = %q, want %q and no \"neither\"", line, "so none was killed")
			}
			if tc.name == "not parked" && strings.Contains(line, "is parked") {
				t.Errorf("line = %q claims a park that never happened", line)
			}
		})
	}
}

// TestCleanupFailureLine_UnreadableWindowSaysNothingWasRemoved: a teardown
// refused because tmux could not say whether the window exists (#702) gets its
// own line — naming that nothing was removed and whether the record was parked
// — rather than the generic "failed" line.
func TestCleanupFailureLine_UnreadableWindowSaysNothingWasRemoved(t *testing.T) {
	refused := fmt.Errorf("refusing to tear down o/r#1, nothing was removed: %w: %w",
		pr.ErrWindowStateUnreadable, tmux.ErrServerUnreadable)
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"parked", refused, "the record is parked as needs-repair"},
		{"not parked", fmt.Errorf("%w; %w", refused, pr.ErrRecordNotParked), "could not be parked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := cleanupFailureLine(pr.CleanupFailure{Path: "/s/o-r-1-1.json", Err: tc.err})
			if !strings.HasPrefix(line, "refused ") || !strings.Contains(line, "not treated as gone") ||
				!strings.Contains(line, tc.want) {
				t.Errorf("line = %q, want the unreadable-window wording and %q", line, tc.want)
			}
			if tc.name == "not parked" && strings.Contains(line, "is parked") {
				t.Errorf("line = %q claims a park that never happened", line)
			}
		})
	}
}

// TestCleanupFailureLine_TimeoutNotParked: a timed-out teardown whose record
// could not be parked must say so, never that it was parked.
func TestCleanupFailureLine_TimeoutNotParked(t *testing.T) {
	err := fmt.Errorf("%w: review window pr-x may still be running; %w", pr.ErrWindowKillTimedOut, pr.ErrRecordNotParked)
	line := cleanupFailureLine(pr.CleanupFailure{Path: "/s/o-r-1-1.json", Err: err})
	if !strings.Contains(line, "could not be parked") || strings.Contains(line, "is parked") {
		t.Errorf("line = %q, want the not-parked wording and no parked claim", line)
	}
}
