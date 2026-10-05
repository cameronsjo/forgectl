// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/desk"
)

// fgResult is one runForeground call's outcome.
type fgResult struct {
	rc  int
	err error
}

// runInBackground runs a's item in the foreground path on a goroutine and
// returns its pid (written by the item to ready) and the result channel.
func runInBackground(t *testing.T, d *desk.Desk, a desk.Added, ready string) (int, <-chan fgResult) {
	t.Helper()
	done := make(chan fgResult, 1)
	go func() {
		rc, err := runForeground(d, a.Name, a.SHA256, IO{TTY: -1})
		done <- fgResult{rc, err}
	}()
	waitFor(t, 20*time.Second, func() bool { return strings.TrimSpace(readOr(ready)) != "" })
	pid, err := strconv.Atoi(strings.TrimSpace(readOr(ready)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Kill(-pid, unix.SIGKILL) })
	return pid, done
}

func waitResult(t *testing.T, pid int, done <-chan fgResult) fgResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(10 * time.Second):
		_ = unix.Kill(-pid, unix.SIGKILL)
		r := <-done
		t.Fatalf("runForeground was still waiting after 10 s (rc %d after a kill)", r.rc)
		return r
	}
}

// TestCtrlZDoesNotWedgeAForegroundRun: SIGTSTP is ignored in the item's
// group, so a stop request (Ctrl-Z, here the item's own kill -TSTP) does not
// leave the desk waiting on a stopped group.
func TestCtrlZDoesNotWedgeAForegroundRun(t *testing.T) {
	d, _ := fgDesk(t)
	dir := t.TempDir()
	ready, after := filepath.Join(dir, "ready"), filepath.Join(dir, "after")
	t.Setenv("FORGECTL_TEST_READY", ready)
	t.Setenv("FORGECTL_TEST_AFTER", after)
	a := fgQueue(t, d, "echo $$ > \"$FORGECTL_TEST_READY\"\nkill -TSTP $$\nsleep 0.2\necho after > \"$FORGECTL_TEST_AFTER\"\n")
	pid, done := runInBackground(t, d, a, ready)
	r := waitResult(t, pid, done)
	if r.err != nil || r.rc != 0 || strings.TrimSpace(readOr(after)) != "after" {
		t.Fatalf("rc %d, err %v, after %q", r.rc, r.err, readOr(after))
	}
}

// TestCtrlCInAForegroundRunIsInterrupted: Ctrl-C reaches the item's group,
// not the desk; the item killed by SIGINT is recorded as interrupted.
func TestCtrlCInAForegroundRunIsInterrupted(t *testing.T) {
	d, _ := fgDesk(t)
	ready := filepath.Join(t.TempDir(), "ready")
	t.Setenv("FORGECTL_TEST_READY", ready)
	a := fgQueue(t, d, "echo $$ > \"$FORGECTL_TEST_READY\"\nsleep 30\n")
	pid, done := runInBackground(t, d, a, ready)
	if err := unix.Kill(-pid, unix.SIGINT); err != nil {
		t.Fatal(err)
	}
	r := waitResult(t, pid, done)
	if r.err != nil || r.rc != 130 {
		t.Fatalf("rc %d, err %v; want 130", r.rc, r.err)
	}
	if ev := eventsOf(t, d, a.Name); !strings.Contains(ev, "RUN-END rc=130 reason=interrupted") {
		t.Errorf("events = %q", ev)
	}
}

// TestAForegroundItemLeavesNothingRunning: a background child the item
// started is ended with its group once the item exits, before the run is
// recorded as ended.
func TestAForegroundItemLeavesNothingRunning(t *testing.T) {
	d, _ := fgDesk(t)
	marker := filepath.Join(t.TempDir(), "after")
	t.Setenv("FORGECTL_TEST_AFTER", marker)
	a := fgQueue(t, d, "(sleep 1; : > \"$FORGECTL_TEST_AFTER\") &\nexit 0\n")
	rc, err := runForeground(d, a.Name, a.SHA256, IO{TTY: -1})
	if err != nil || rc != 0 {
		t.Fatalf("runForeground = %d, %v", rc, err)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("the item's background child outlived the run")
	}
}

// TestAnItemThatCannotStartIsAnError: a start failure is returned, so the
// dashboard says so and waits for a key, and the run is recorded as failed.
func TestAnItemThatCannotStartIsAnError(t *testing.T) {
	d, home := fgDesk(t)
	a := fgQueue(t, d, "exit 0\n")
	if err := os.Remove(home); err != nil { // cwd is $HOME: chdir fails
		t.Fatal(err)
	}
	var out bytes.Buffer
	rc, err := runForeground(d, a.Name, a.SHA256, IO{TTY: -1, Stdout: &out, Stderr: &out})
	if err == nil || rc != 127 || !strings.Contains(err.Error(), "could not start") {
		t.Fatalf("runForeground = %d, %v; want 127 and a start error", rc, err)
	}
	if ev := eventsOf(t, d, a.Name); !strings.Contains(ev, "RUN-END rc=127 reason=failed") {
		t.Errorf("events = %q", ev)
	}
}

// TestOnlyATTYScriptRunsInTheForeground: a claim that is not a TTY script is
// given back, and nothing runs.
func TestOnlyATTYScriptRunsInTheForeground(t *testing.T) {
	d, _ := fgDesk(t)
	marker := filepath.Join(t.TempDir(), "ran")
	src := filepath.Join(t.TempDir(), "bg.sh")
	if err := os.WriteFile(src, []byte("touch "+marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := d.Add(src, "a background item", "kind check", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Scan(); err != nil {
		t.Fatal(err)
	}
	if _, err := runForeground(d, a.Name, a.SHA256, IO{TTY: -1}); err == nil {
		t.Fatal("a non-TTY item ran in the foreground")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the item ran")
	}
	s, serr := d.Scan()
	if serr != nil || len(s.Skipped) != 1 || s.Skipped[0].Meta.SkipReason != desk.SkipLaunchFailed {
		t.Fatalf("after the refusal: %+v, %v", s, serr)
	}
}
