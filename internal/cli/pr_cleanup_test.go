package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
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
