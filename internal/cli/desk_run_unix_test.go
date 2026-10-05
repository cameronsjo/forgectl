// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/desk"
)

// Foreground runs of TTY items, without a terminal (TTY -1): the claim, the
// bytes that run, the record, signals, and a parent that dies.

const foregroundHelperEnv = "FORGECTL_TEST_FOREGROUND_HELPER"

func init() { testHelperMains = append(testHelperMains, foregroundHelper) }

// foregroundHelper is this binary re-executed as a desk running one
// foreground item, with the real signal subscription.
func foregroundHelper() (int, bool) {
	if os.Getenv(foregroundHelperEnv) != "1" {
		return 0, false
	}
	d, err := desk.Open(os.Getenv("FORGECTL_TEST_DESK"))
	if err != nil {
		fmt.Println("open:", err)
		return 2, true
	}
	rc, err := runForeground(d, os.Getenv("FORGECTL_TEST_NAME"), os.Getenv("FORGECTL_TEST_SHA"), IO{TTY: -1, Stdout: os.Stdout, Stderr: os.Stderr})
	fmt.Printf("rc=%d err=%v\n", rc, err)
	return 0, true
}

// fgDesk is a desk in a temp dir, with HOME and TMPDIR pointed at temp dirs
// so nothing an item does reaches the real home directory.
func fgDesk(t *testing.T) (*desk.Desk, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", t.TempDir())
	d, err := desk.Open(filepath.Join(t.TempDir(), "desk"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d, home
}

func fgQueue(t *testing.T, d *desk.Desk, body string) desk.Added {
	t.Helper()
	src := filepath.Join(t.TempDir(), "fg.sh")
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := d.Add(src, "run in the terminal", "it asks a question", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Scan(); err != nil {
		t.Fatal(err)
	}
	return a
}

func readOr(p string) string {
	b, err := os.ReadFile(p) //nolint:gosec // G304: a marker path the test made
	if err != nil {
		return ""
	}
	return string(b)
}

func eventsOf(t *testing.T, d *desk.Desk, name string) string {
	t.Helper()
	b, err := os.ReadFile(d.EventsPath(name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func waitFor(t *testing.T, limit time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("not ready after %v", limit)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAForegroundRunExecutesTheClaimedBytesOnFD3: the child's $0 is
// /dev/fd/3, it starts in $HOME, and a rewrite of the record after the claim
// does not reach what runs.
func TestAForegroundRunExecutesTheClaimedBytesOnFD3(t *testing.T) {
	d, home := fgDesk(t)
	dir := t.TempDir()
	zero, cwd, hijack := filepath.Join(dir, "zero"), filepath.Join(dir, "cwd"), filepath.Join(dir, "hijack")
	t.Setenv("FORGECTL_TEST_ZERO", zero)
	t.Setenv("FORGECTL_TEST_CWD", cwd)
	t.Setenv("FORGECTL_TEST_HIJACK", hijack)
	a := fgQueue(t, d, "printf '%s' \"$0\" > \"$FORGECTL_TEST_ZERO\"\npwd > \"$FORGECTL_TEST_CWD\"\n")

	rewrote := false
	afterForegroundClaim = func(c *desk.Claimed) {
		hijacked := []byte("echo hijacked > \"$FORGECTL_TEST_HIJACK\"\n")
		rewrote = os.WriteFile(c.RecordPath, hijacked, 0o600) == nil //nolint:gosec // G703: the claimed record in the test's own temp desk
	}
	t.Cleanup(func() { afterForegroundClaim = func(*desk.Claimed) {} })

	var out bytes.Buffer
	rc, err := runForeground(d, a.Name, a.SHA256, IO{TTY: -1, Stdout: &out, Stderr: &out})
	if err != nil || rc != 0 {
		t.Fatalf("runForeground = %d, %v; output %q", rc, err, out.String())
	}
	if !rewrote {
		t.Fatal("the record was not rewritten, so the test proves nothing")
	}
	if got := readOr(zero); got != desk.ScriptFDPath {
		t.Errorf("$0 = %q, want %q", got, desk.ScriptFDPath)
	}
	if got := strings.TrimSpace(readOr(cwd)); got != home {
		if r, err := filepath.EvalSymlinks(home); err != nil || got != r {
			t.Errorf("cwd = %q, want %q", got, home)
		}
	}
	if readOr(hijack) != "" {
		t.Error("the rewritten record ran")
	}
	if ev := eventsOf(t, d, a.Name); !strings.Contains(ev, "RUN-END rc=0 reason=ok") {
		t.Errorf("events = %q", ev)
	}
	if !strings.Contains(out.String(), "ran "+a.Name+": exit 0 (ok) — press any key") {
		t.Errorf("output = %q", out.String())
	}
	s, err := d.Scan()
	if err != nil || len(s.Done) != 1 || s.Done[0].Meta.PID != os.Getpid() {
		t.Errorf("after the run: %+v, %v", s, err)
	}
}

// TestAFailingForegroundItemIsRecordedAsFailed.
func TestAFailingForegroundItemIsRecordedAsFailed(t *testing.T) {
	d, _ := fgDesk(t)
	a := fgQueue(t, d, "exit 3\n")
	var out bytes.Buffer
	rc, err := runForeground(d, a.Name, a.SHA256, IO{TTY: -1, Stdout: &out})
	if err != nil || rc != 3 {
		t.Fatalf("runForeground = %d, %v", rc, err)
	}
	if ev := eventsOf(t, d, a.Name); !strings.Contains(ev, "RUN-END rc=3 reason=failed") {
		t.Errorf("events = %q", ev)
	}
	if !strings.Contains(out.String(), "exit 3 (failed)") {
		t.Errorf("output = %q", out.String())
	}
}

// TestAForegroundClaimNeedsTheApprovedHash: a short hash is refused and a
// different full one is a change; neither moves or runs anything.
func TestAForegroundClaimNeedsTheApprovedHash(t *testing.T) {
	d, _ := fgDesk(t)
	a := fgQueue(t, d, "exit 0\n")
	if _, err := runForeground(d, a.Name, a.SHA256[:63], IO{TTY: -1}); !errors.Is(err, desk.ErrRefused) {
		t.Errorf("a 63-digit hash: %v, want ErrRefused", err)
	}
	if _, err := runForeground(d, a.Name, strings.Repeat("0", 64), IO{TTY: -1}); !errors.Is(err, desk.ErrChanged) {
		t.Errorf("another hash: %v, want ErrChanged", err)
	}
	if s, err := d.Scan(); err != nil || len(s.Pending) != 1 {
		t.Errorf("after the refusals: %+v, %v", s, err)
	}
}

// TestAForegroundClaimThatCannotBeginIsGivenBack: BeginRun failing after a
// successful claim leaves the item in skipped/ as launch-failed.
func TestAForegroundClaimThatCannotBeginIsGivenBack(t *testing.T) {
	d, _ := fgDesk(t)
	a := fgQueue(t, d, "exit 0\n")
	// A done/ log under the same name makes BeginRun refuse a reused name.
	if err := os.WriteFile(d.LogPath(a.Name), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runForeground(d, a.Name, a.SHA256, IO{TTY: -1})
	if err == nil {
		t.Fatal("runForeground ran an item whose run could not begin")
	}
	s, serr := d.Scan()
	if serr != nil || len(s.Skipped) != 1 || s.Skipped[0].Meta.SkipReason != "launch-failed" || len(s.Running) != 0 {
		t.Fatalf("after the failed begin: %+v, %v", s, serr)
	}
}

// startHelper re-executes this binary as a desk running a's item in the
// foreground, and waits until the item has written ready.
func startHelper(t *testing.T, d *desk.Desk, a desk.Added, ready string) (*exec.Cmd, *bytes.Buffer, int) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0]) //nolint:gosec // G204: the test binary itself
	cmd.Env = append(os.Environ(), foregroundHelperEnv+"=1", "FORGECTL_TEST_DESK="+d.Path(),
		"FORGECTL_TEST_NAME="+a.Name, "FORGECTL_TEST_SHA="+a.SHA256)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	// The item inherits the output pipe and can outlive a killed helper;
	// Wait must not wait for it to close.
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	waitFor(t, 20*time.Second, func() bool { return strings.TrimSpace(readOr(ready)) != "" })
	pid, err := strconv.Atoi(strings.TrimSpace(readOr(ready)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Kill(-pid, unix.SIGKILL) })
	return cmd, &out, pid
}

// TestAForwardedSIGHUPReachesTheItemAndTheRunIsFinished: the desk's own
// SIGHUP goes to the item's group, which traps it; the run ends interrupted
// with RUN-END written.
func TestAForwardedSIGHUPReachesTheItemAndTheRunIsFinished(t *testing.T) {
	d, _ := fgDesk(t)
	dir := t.TempDir()
	ready, got := filepath.Join(dir, "ready"), filepath.Join(dir, "got")
	t.Setenv("FORGECTL_TEST_READY", ready)
	t.Setenv("FORGECTL_TEST_GOT", got)
	a := fgQueue(t, d, "trap 'echo hup > \"$FORGECTL_TEST_GOT\"; exit 129' HUP\necho $$ > \"$FORGECTL_TEST_READY\"\nwhile :; do sleep 0.1; done\n")

	cmd, out, _ := startHelper(t, d, a, ready)
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper: %v; output %q", err, out.String())
	}
	if strings.TrimSpace(readOr(got)) != "hup" {
		t.Errorf("the item never saw SIGHUP; helper said %q", out.String())
	}
	if ev := eventsOf(t, d, a.Name); !strings.Contains(ev, "RUN-END rc=129 reason=interrupted") {
		t.Errorf("events = %q; helper said %q", ev, out.String())
	}
	if !strings.Contains(out.String(), "rc=129 err=<nil>") {
		t.Errorf("helper said %q", out.String())
	}
}

// TestAKilledForegroundDeskLeavesTheItemLost: a desk SIGKILLed mid-run
// cannot finish it; its recorded pid is dead, so the item reads as lost and
// can be skipped.
func TestAKilledForegroundDeskLeavesTheItemLost(t *testing.T) {
	d, _ := fgDesk(t)
	ready := filepath.Join(t.TempDir(), "ready")
	t.Setenv("FORGECTL_TEST_READY", ready)
	a := fgQueue(t, d, "echo $$ > \"$FORGECTL_TEST_READY\"\nsleep 30\n")

	cmd, _, _ := startHelper(t, d, a, ready)
	s, err := d.Scan()
	if err != nil || len(s.Running) != 1 || s.Running[0].State != desk.StateRunning {
		t.Fatalf("while the desk lives: %+v, %v", s, err)
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	s, err = d.Scan()
	if err != nil || len(s.Running) != 1 || s.Running[0].State != desk.StateLost {
		t.Fatalf("after the desk died: %+v, %v", s, err)
	}
	if err := d.Skip(a.Name, desk.SkipOperator); err != nil {
		t.Errorf("Skip on the lost item: %v", err)
	}
}

// TestTakingTheTerminalBackUsesThisGroup: the take-back names this
// process's group and passes the terminal's error through, so EIO can end
// the run as a closed pane.
func TestTakingTheTerminalBackUsesThisGroup(t *testing.T) {
	var gotTTY, gotPgid int
	err := takeTerminal(IO{TTY: 7, SetForeground: func(tty, pgid int) error {
		gotTTY, gotPgid = tty, pgid
		return unix.EIO
	}})
	if gotTTY != 7 || gotPgid != unix.Getpgrp() || !errors.Is(err, unix.EIO) {
		t.Errorf("SetForeground(%d, %d) -> %v; want (7, %d) and EIO", gotTTY, gotPgid, err, unix.Getpgrp())
	}
}

func TestExitMeaning(t *testing.T) {
	for _, c := range []struct {
		rc          int
		interrupted bool
		want        string
	}{
		{0, false, "ok"}, {1, false, "failed"}, {129, true, "interrupted"},
		{127, false, "command not found"}, {137, false, "killed by SIGKILL"},
	} {
		if got := exitMeaning(c.rc, c.interrupted); got != c.want {
			t.Errorf("exitMeaning(%d, %v) = %q, want %q", c.rc, c.interrupted, got, c.want)
		}
	}
}
