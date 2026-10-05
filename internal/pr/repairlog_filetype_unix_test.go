//go:build unix

package pr

// Test plan for openRepairLogFile (forgectl#614)
//
// Every opener of repair.jsonl goes through one helper that refuses a symlink
// at the open (O_NOFOLLOW) and anything but a regular file after it (Fstat),
// without ever blocking in the open itself (O_NONBLOCK).
//
//   [x] A FIFO log: prune, the history read and the append each fail fast
//   [x] A symlink to a regular file: refused by all three, target untouched,
//       and prune leaves the link a link
//   [x] A symlink to /dev/zero: refused fast by all three
//   [x] A missing log still reads as empty (history and prune)
//   [x] A normal log reads and appends as before, and the reader's descriptor
//       is left blocking
//   [x] A socket and a directory are the typed refusal for every opener, even
//       where the kernel refuses the open itself (ENXIO, EISDIR) (#621)
//   [x] ELOOP names the log only when the log is the symlink; a loop in a
//       directory above it names the directory instead (#621)
//   [x] The refusal says "audit log" once, not twice (#621)
//   [x] A hard-linked log is ACCEPTED, as documented on openRepairLogFile (#621)

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// failFastWait is how long an opener may take before the test calls it a
// hang. Every refusal here is one open plus one fstat, so this is generous.
const failFastWait = 5 * time.Second

// mustFailFast runs fn off the test goroutine and fails the test if it does
// not return within failFastWait. A hung opener leaks its goroutine, which is
// the lesser evil: the alternative is a test binary that never exits.
func mustFailFast(t *testing.T, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(failFastWait):
		t.Fatalf("%s did not return within %s: it blocked or spun on the log", what, failFastWait)
		return nil
	}
}

// wantNotRegular asserts err is the helper's refusal and names the log.
func wantNotRegular(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: err = nil, want a refusal of the non-regular log", what)
	}
	if !errors.Is(err, errRepairLogNotRegular) {
		t.Fatalf("%s: err = %v, want errRepairLogNotRegular", what, err)
	}
	if !strings.Contains(err.Error(), repairLogName) {
		t.Errorf("%s: error %q does not name the log", what, err)
	}
}

// fifoLog replaces the log with a FIFO nobody writes to.
func fifoLog(t *testing.T, c *Client) {
	t.Helper()
	if err := syscall.Mkfifo(c.repairLogPath(), 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
}

// symlinkLog replaces the log with a symlink to target.
func symlinkLog(t *testing.T, c *Client, target string) {
	t.Helper()
	if err := os.Symlink(target, c.repairLogPath()); err != nil {
		t.Fatalf("symlink: %v", err)
	}
}

// devZero skips when the host has no /dev/zero to point a link at.
func devZero(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skipf("no /dev/zero: %v", err)
	}
	return "/dev/zero"
}

// The three openers, each exercised through its production entry point where
// that entry point surfaces the error. Prune reports its log refusal in the
// report rather than as an error, so pruneLogErr lifts it out.
func historyErr(c *Client) error {
	_, err := c.RepairHistory(context.Background())
	return err
}

func appendErr(c *Client) error {
	return c.appendRepairRowLocked(RepairRow{ID: "a", Outcome: repairOutcomeIntent})
}

func pruneLogErr(c *Client) (PruneReport, error) {
	return c.Prune(context.Background(), PruneOpts{
		OlderThan: 30 * 24 * time.Hour, LogRetention: 90 * 24 * time.Hour, Yes: true,
	})
}

// wantPruneRefusedLog asserts prune refused the log with the helper's error.
func wantPruneRefusedLog(t *testing.T, report PruneReport, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if report.Log.Outcome != pruneOutcomeRefused {
		t.Fatalf("log outcome = %q (error %q), want %q", report.Log.Outcome, report.Log.Error, pruneOutcomeRefused)
	}
	if !strings.Contains(report.Log.Error, "not a regular file") {
		t.Errorf("log error = %q, want it to say the log is not a regular file", report.Log.Error)
	}
}

// Mutation that turns it red: drop O_NONBLOCK from the open in
// repairlog_unix.go — the O_RDONLY opens (history, prune) block waiting for a
// writer and hit the timeout. Dropping the IsRegular check instead turns all
// three red a different way: the readers accept the FIFO and read it as an
// empty log, and the append fails on a seek rather than refusing the file.
func TestRepairLog_AFIFOFailsFast(t *testing.T) {
	t.Run("history", func(t *testing.T) {
		c := testClient(t, nil)
		fifoLog(t, c)
		wantNotRegular(t, "RepairHistory", mustFailFast(t, "RepairHistory", func() error { return historyErr(c) }))
	})
	t.Run("append", func(t *testing.T) {
		c := testClient(t, nil)
		fifoLog(t, c)
		wantNotRegular(t, "appendRepairRowLocked", mustFailFast(t, "appendRepairRowLocked", func() error { return appendErr(c) }))
	})
	t.Run("prune", func(t *testing.T) {
		c := pruneClient(t, repairRunner(nil))
		fifoLog(t, c)
		var report PruneReport
		err := mustFailFast(t, "Prune", func() error {
			var perr error
			report, perr = pruneLogErr(c)
			return perr
		})
		wantPruneRefusedLog(t, report, err)
	})
}

// Mutation that turns it red: drop O_NOFOLLOW from the open in
// repairlog_unix.go. The link resolves to a regular file, so the Fstat check
// passes it: the readers return the target's row, the append writes through
// the link into the target, and prune reads it back.
func TestRepairLog_ASymlinkToARegularFileIsRefused(t *testing.T) {
	const content = `{"id":"target","outcome":"intent"}` + "\n"
	setup := func(t *testing.T, c *Client) string {
		t.Helper()
		target := filepath.Join(t.TempDir(), "elsewhere.jsonl")
		if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		symlinkLog(t, c, target)
		return target
	}
	targetUnchanged := func(t *testing.T, target string) {
		t.Helper()
		got, err := os.ReadFile(target) //nolint:gosec // the test's own t.TempDir
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != content {
			t.Errorf("the link's target changed: %q, want %q", got, content)
		}
	}
	t.Run("history", func(t *testing.T) {
		c := testClient(t, nil)
		target := setup(t, c)
		wantNotRegular(t, "RepairHistory", mustFailFast(t, "RepairHistory", func() error { return historyErr(c) }))
		targetUnchanged(t, target)
	})
	t.Run("append", func(t *testing.T) {
		c := testClient(t, nil)
		target := setup(t, c)
		wantNotRegular(t, "appendRepairRowLocked", mustFailFast(t, "appendRepairRowLocked", func() error { return appendErr(c) }))
		targetUnchanged(t, target)
	})
	t.Run("prune", func(t *testing.T) {
		c := pruneClient(t, repairRunner(nil))
		target := setup(t, c)
		var report PruneReport
		err := mustFailFast(t, "Prune", func() error {
			var perr error
			report, perr = pruneLogErr(c)
			return perr
		})
		wantPruneRefusedLog(t, report, err)
		targetUnchanged(t, target)
		info, lerr := os.Lstat(c.repairLogPath())
		if lerr != nil {
			t.Fatal(lerr)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("the log is now mode %s, want the symlink left in place", info.Mode().Type())
		}
	})
}

// Mutation that turns it red: drop BOTH O_NOFOLLOW and the IsRegular check.
// With either one alone the link is still refused (O_NOFOLLOW at the open, the
// Fstat on the char device after it), so this test pins that at least one of
// the two layers stands. With both gone the readers spin on an endless stream
// of zero bytes and hit the timeout, and the append writes into /dev/zero and
// succeeds.
func TestRepairLog_ASymlinkToDevZeroIsRefusedFast(t *testing.T) {
	zero := devZero(t)
	t.Run("history", func(t *testing.T) {
		c := testClient(t, nil)
		symlinkLog(t, c, zero)
		wantNotRegular(t, "RepairHistory", mustFailFast(t, "RepairHistory", func() error { return historyErr(c) }))
	})
	t.Run("append", func(t *testing.T) {
		c := testClient(t, nil)
		symlinkLog(t, c, zero)
		wantNotRegular(t, "appendRepairRowLocked", mustFailFast(t, "appendRepairRowLocked", func() error { return appendErr(c) }))
	})
	t.Run("prune", func(t *testing.T) {
		c := pruneClient(t, repairRunner(nil))
		symlinkLog(t, c, zero)
		var report PruneReport
		err := mustFailFast(t, "Prune", func() error {
			var perr error
			report, perr = pruneLogErr(c)
			return perr
		})
		wantPruneRefusedLog(t, report, err)
	})
}

// Mutation that turns it red: drop the fs.ErrNotExist branch in
// readRepairLogTail or openRepairLog, or have the helper wrap ENOENT in a
// type that errors.Is no longer matches — a first run with no log then fails
// instead of reading as empty.
func TestRepairLog_AMissingLogReadsAsEmpty(t *testing.T) {
	c := testClient(t, nil)
	trail, err := c.RepairHistory(context.Background())
	if err != nil {
		t.Fatalf("RepairHistory on a missing log: %v", err)
	}
	if len(trail.Rows) != 0 || trail.Omitted != 0 || trail.Skipped != 0 {
		t.Errorf("trail = %+v, want empty", trail)
	}

	pc := pruneClient(t, repairRunner(nil))
	report, err := pruneLogErr(pc)
	if err != nil {
		t.Fatalf("Prune on a missing log: %v", err)
	}
	if report.Log.Outcome != pruneOutcomeUnchanged || report.Log.Error != "" {
		t.Errorf("log outcome = %q (error %q), want %q", report.Log.Outcome, report.Log.Error, pruneOutcomeUnchanged)
	}
	if _, err := os.Lstat(pc.repairLogPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a no-op prune created the log: %v", err)
	}
}

// Mutation that turns it red: remove the SetNonblock(fd, false) in
// repairlog_unix.go — the descriptor the helper hands back is left
// non-blocking, which no caller of a regular file expects. The round trip
// (append, then read back through history) turns red if the helper refuses a
// regular file, e.g. by inverting the IsRegular check.
func TestRepairLog_ARegularLogIsUnchanged(t *testing.T) {
	c := testClient(t, nil)
	for _, id := range []string{"a", "b"} {
		if err := c.appendRepairRowLocked(RepairRow{ID: id, Outcome: repairOutcomeIntent}); err != nil {
			t.Fatalf("appendRepairRowLocked(%s): %v", id, err)
		}
	}
	trail, err := c.RepairHistory(context.Background())
	if err != nil {
		t.Fatalf("RepairHistory: %v", err)
	}
	if len(trail.Rows) != 2 || trail.Rows[0].ID != "a" || trail.Rows[1].ID != "b" {
		t.Errorf("rows = %+v, want a then b", trail.Rows)
	}

	f, err := openRepairLogFile(c.repairLogPath(), os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("openRepairLogFile on a regular log: %v", err)
	}
	defer func() { _ = f.Close() }()
	// Read the flag through SyscallConn, not f.Fd(): Fd() itself switches the
	// descriptor to blocking mode, which would make this check pass vacuously.
	rc, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var nonblock bool
	var nberr error
	if err := rc.Control(func(fd uintptr) {
		var fl int
		fl, nberr = unix.FcntlInt(fd, unix.F_GETFL, 0)
		nonblock = fl&unix.O_NONBLOCK != 0
	}); err != nil {
		t.Fatal(err)
	}
	if nberr != nil {
		t.Fatal(nberr)
	}
	if nonblock {
		t.Error("the helper returned a non-blocking descriptor; O_NONBLOCK is for the open only")
	}
}

// shortSessionsDir is a sessions dir short enough for a unix socket address:
// sun_path is 104 bytes on macOS, which a t.TempDir path there overruns, so a
// socket test under t.TempDir would skip on the macOS runner and prove nothing.
func shortSessionsDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fsock-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// socketLog replaces the log with a listening unix socket, skipping when the
// path is still too long for a socket address.
func socketLog(t *testing.T, c *Client) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", c.repairLogPath())
	if err != nil {
		t.Skipf("cannot bind a unix socket at the log path: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
}

// dirLog replaces the log with a directory.
func dirLog(t *testing.T, c *Client) {
	t.Helper()
	if err := os.Mkdir(c.repairLogPath(), 0o700); err != nil {
		t.Fatal(err)
	}
}

// Mutation that turns it red: remove the ENXIO/EOPNOTSUPP/EISDIR arm in
// openRepairLogNoFollow. A socket then fails every opener with a bare
// "no such device or address" (Linux) or "operation not supported" (macOS),
// and the append's O_RDWR on a directory with a bare "is a directory" —
// neither the typed refusal. The socket cases run in a short sessions dir so
// the macOS runner binds rather than skips.
func TestRepairLog_ASocketOrDirectoryIsTheTypedRefusal(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		place func(*testing.T, *Client)
		short bool
	}{
		{"socket", socketLog, true},
		{"directory", dirLog, false},
	} {
		sessionsDir := func(t *testing.T) string {
			if tc.short {
				return shortSessionsDir(t)
			}
			return t.TempDir()
		}
		t.Run(tc.kind+"/history", func(t *testing.T) {
			c := testClientAt(t, nil, sessionsDir(t))
			tc.place(t, c)
			err := mustFailFast(t, "RepairHistory", func() error { return historyErr(c) })
			wantNotRegular(t, "RepairHistory", err)
			if !strings.Contains(err.Error(), "is a "+tc.kind) {
				t.Errorf("error %q does not name the %s", err, tc.kind)
			}
		})
		t.Run(tc.kind+"/append", func(t *testing.T) {
			c := testClientAt(t, nil, sessionsDir(t))
			tc.place(t, c)
			err := mustFailFast(t, "appendRepairRowLocked", func() error { return appendErr(c) })
			wantNotRegular(t, "appendRepairRowLocked", err)
			if !strings.Contains(err.Error(), "is a "+tc.kind) {
				t.Errorf("error %q does not name the %s", err, tc.kind)
			}
		})
		t.Run(tc.kind+"/prune", func(t *testing.T) {
			c := pruneClient(t, repairRunner(nil), WithSessionsDir(sessionsDir(t)))
			tc.place(t, c)
			var report PruneReport
			err := mustFailFast(t, "Prune", func() error {
				var perr error
				report, perr = pruneLogErr(c)
				return perr
			})
			wantPruneRefusedLog(t, report, err)
		})
	}
}

// Mutation that turns it red: drop the Lstat in the ELOOP arm of
// openRepairLogNoFollow and report every ELOOP as "is a symlink" — the
// parent-loop case then names the log and reads as the typed refusal.
func TestRepairLog_ELOOPNamesTheRightComponent(t *testing.T) {
	dir := t.TempDir()
	t.Run("the log itself is a symlink", func(t *testing.T) {
		link := filepath.Join(dir, "self.jsonl")
		if err := os.Symlink(filepath.Join(dir, "elsewhere"), link); err != nil {
			t.Fatal(err)
		}
		_, err := openRepairLogFile(link, os.O_RDONLY, 0)
		if !errors.Is(err, errRepairLogNotRegular) || !strings.Contains(err.Error(), "is a symlink") {
			t.Errorf("err = %v, want the typed refusal naming the log as a symlink", err)
		}
	})
	t.Run("a directory above the log loops", func(t *testing.T) {
		loop := filepath.Join(dir, "loop")
		if err := os.Symlink("loop", loop); err != nil {
			t.Fatal(err)
		}
		_, err := openRepairLogFile(filepath.Join(loop, repairLogName), os.O_RDONLY, 0)
		if err == nil || errors.Is(err, errRepairLogNotRegular) || strings.Contains(err.Error(), "is a symlink") {
			t.Fatalf("err = %v, want an untyped error that does not call the log a symlink", err)
		}
		if !errors.Is(err, syscall.ELOOP) || !strings.Contains(err.Error(), "a directory above") {
			t.Errorf("err = %v, want ELOOP naming a directory above the log", err)
		}
	})
}

// Mutation that turns it red: restore the old sentinel text ("the repair
// audit log is not a regular file") — the append's refusal then reads
// "open repair audit log: the repair audit log is not a regular file: …".
func TestRepairLog_TheRefusalDoesNotRepeatItself(t *testing.T) {
	c := testClient(t, nil)
	fifoLog(t, c)
	err := mustFailFast(t, "appendRepairRowLocked", func() error { return appendErr(c) })
	wantNotRegular(t, "appendRepairRowLocked", err)
	if n := strings.Count(err.Error(), "audit log"); n != 1 {
		t.Errorf("error %q says \"audit log\" %d times, want once", err, n)
	}
}

// TestRepairLog_AHardLinkedLogIsAccepted pins the documented decision on
// openRepairLogFile: a hard link is a regular file and is read and appended to
// like one. If this goes red, the decision changed and the doc comment must
// change with it.
//
// Mutation that turns it red: refuse Nlink > 1 in openRepairLogFile.
func TestRepairLog_AHardLinkedLogIsAccepted(t *testing.T) {
	c := testClient(t, nil)
	other := filepath.Join(c.SessionsDir(), "other-name.jsonl")
	if err := os.WriteFile(other, []byte(`{"id":"linked","outcome":"intent"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(other, c.repairLogPath()); err != nil {
		t.Skipf("hard links unsupported: %v", err)
	}
	if err := appendErr(c); err != nil {
		t.Fatalf("appendRepairRowLocked on a hard-linked log: %v", err)
	}
	trail, err := c.RepairHistory(context.Background())
	if err != nil {
		t.Fatalf("RepairHistory on a hard-linked log: %v", err)
	}
	if len(trail.Rows) != 2 || trail.Rows[0].ID != "linked" || trail.Rows[1].ID != "a" {
		t.Errorf("rows = %+v, want linked then a", trail.Rows)
	}
}
