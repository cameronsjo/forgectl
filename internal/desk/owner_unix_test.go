//go:build unix

package desk

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// deadPID returns the pid of a process that has already exited and been
// reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// noHang fails the test when fn has not returned within a few seconds.
func noHang(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s blocked; a FIFO must be refused, not opened for a writer that never comes", what)
	}
}

func mkfifo(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestReadersRefuseAFIFO plants a FIFO at every file the dashboard reads
// each second, and at the l pager's log. None of them may block.
func TestReadersRefuseAFIFO(t *testing.T) {
	d := openDesk(t)
	root := d.Path()
	// A running batch whose status.tsv is a FIFO.
	writeFile(t, filepath.Join(root, DirRunning, "01-batch.manifest"), "a -- true\n", 0o600)
	mkfifo(t, filepath.Join(root, DirDone, "01-batch.d", "status.tsv"))
	// A finished item whose log and record are FIFOs.
	mkfifo(t, filepath.Join(root, DirDone, "02-x.log"))
	writeFile(t, filepath.Join(root, DirDone, "03-y.log"), "EXIT=0\n", 0o600)
	mkfifo(t, filepath.Join(root, DirDone, "03-y.sh"))
	// A running item with a dead owner whose events file is a FIFO, so lost()
	// reaches hasRunEnd.
	writeFile(t, filepath.Join(root, DirRunning, "04-z.sh"), script, 0o600)
	if err := d.writeMeta(DirRunning, "04-z", Meta{SHA256: "x", PID: deadPID(t)}); err != nil {
		t.Fatal(err)
	}
	mkfifo(t, filepath.Join(root, DirDone, "04-z.events"))
	// A skipped item and a running record that are FIFOs.
	mkfifo(t, filepath.Join(root, DirSkipped, "05-s.sh"))
	mkfifo(t, filepath.Join(root, DirRunning, "06-r.sh"))

	noHang(t, "Scan", func() {
		if _, err := d.Scan(); err != nil {
			t.Errorf("Scan: %v", err)
		}
	})
	noHang(t, "BatchStatus", func() {
		if _, err := d.BatchStatus("01-batch"); err == nil {
			t.Error("BatchStatus read a FIFO")
		}
	})
	noHang(t, "LogTail", func() {
		if _, _, err := d.LogTail("02-x", 1024); err == nil {
			t.Error("LogTail read a FIFO")
		}
	})
	noHang(t, "Record", func() {
		if _, _, err := d.Record("03-y"); err == nil {
			t.Error("Record read a FIFO")
		}
	})
	noHang(t, "Watcher.Poll", func() { _, _, _ = mustWatcher(t, d, "04-z").Poll() })
}

func mustWatcher(t *testing.T, d *Desk, name string) *Watcher {
	t.Helper()
	w, err := d.NewWatcher(name, 0)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// Claim moves the item before its meta. In that window running/ holds an
// item with no meta; it is a claim in progress, never lost, however old the
// file is.
func TestClaimInProgressIsNeverLost(t *testing.T) {
	d := openDesk(t)
	p := filepath.Join(d.Path(), DirRunning, "01-hi.sh")
	writeFile(t, p, script, 0o600)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	d.now = func() time.Time { return time.Now().Add(ClaimGrace * 10) }
	if got := scan(t, d).Running[0].State; got != StateRunning {
		t.Fatalf("a claim in progress reads as %s, want running", got)
	}
	if err := d.Skip("01-hi", SkipLost); err == nil {
		t.Fatal("Skip moved a claim in progress")
	}
	if _, st, _ := mustWatcher(t, d, "01-hi").Poll(); st != WatchWaiting {
		t.Fatalf("watch state %s, want waiting", st)
	}
}

// Claim writes claimed_at into the meta before it moves the item, so the
// meta that follows the item into running/ already carries it.
func TestClaimStampsClaimedAtBeforeTheMove(t *testing.T) {
	d := openDesk(t)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	d.now = func() time.Time { return at }
	name := claimOne(t, d)
	meta, ok, err := d.readMeta(DirRunning, name)
	if err != nil || !ok || meta.ClaimedAt == nil || !meta.ClaimedAt.Equal(at) {
		t.Fatalf("running meta = %+v ok=%v err=%v, want claimed_at %s", meta, ok, err, at)
	}
}

// An owner that holds the lock is alive, whatever the pid in meta says and
// however long it has run. Once the lock is gone (the owner died), the run is
// lost and Skip moves it, always as SkipLost.
func TestLiveOwnerPastTheGraceCannotBeSkipped(t *testing.T) {
	d := openDesk(t)
	name := claimOne(t, d)
	run, err := d.BeginRun(name, deadPID(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Events.Close() })
	d.now = func() time.Time { return time.Now().Add(ClaimGrace * 10) }

	if got := scan(t, d).Running[0].State; got != StateRunning {
		t.Fatalf("with its lock held the run reads as %s, want running", got)
	}
	if err := d.Skip(name, SkipOperator); err == nil || !strings.Contains(err.Error(), "holds the lock") {
		t.Fatalf("Skip of a live owner = %v, want a refusal", err)
	}
	if err := d.Release(name, SkipLaunchFailed); err == nil {
		t.Fatal("Release of a live owner succeeded")
	}
	if _, st, _ := mustWatcher(t, d, name).Poll(); st != WatchRunning {
		t.Fatalf("watch state %s, want running", st)
	}

	run.lock.release(false) // the owner dies: the kernel drops its flock
	run.lock = nil
	if got := scan(t, d).Running[0].State; got != StateLost {
		t.Fatalf("after the owner died the run reads as %s, want lost", got)
	}
	if err := d.Skip(name, SkipOperator); err != nil {
		t.Fatalf("Skip of a lost run: %v", err)
	}
	if got := skipReason(t, d, name); got != SkipLost {
		t.Fatalf("a run skipped from running/ recorded %q, want %q", got, SkipLost)
	}
	if err := d.Unskip(name); err == nil {
		t.Fatal("a lost run was re-armed")
	}
}

// When BeginRun fails after writing its pid, the pid comes back out and the
// lock is dropped, so the claim can still be released.
func TestBeginRunFailureLeavesNoOwner(t *testing.T) {
	d := openDesk(t)
	name := claimOne(t, d)
	// Fail the step after the pid is written (opening the events file).
	saved := beforeRunStart
	t.Cleanup(func() { beforeRunStart = saved })
	written := false
	beforeRunStart = func(n string) error {
		meta, _, _ := d.readMeta(DirRunning, n)
		written = meta.PID == os.Getpid()
		return errors.New("events unavailable")
	}
	if _, err := d.BeginRun(name, os.Getpid()); err == nil {
		t.Fatal("BeginRun succeeded with no events file")
	}
	if !written {
		t.Fatal("the failure came before the pid was written; the test does not reach the undo")
	}
	meta, _, err := d.readMeta(DirRunning, name)
	if err != nil || meta.PID != 0 {
		t.Fatalf("after a failed BeginRun meta pid = %d (err %v), want 0", meta.PID, err)
	}
	if d.ownerAlive(name) {
		t.Fatal("a failed BeginRun left its lock held")
	}
	if err := d.Release(name, SkipLaunchFailed); err != nil {
		t.Fatalf("Release after a failed BeginRun: %v", err)
	}
}

// Launch refuses a TTY item (it runs in the desk's foreground) and ends the
// claim, rather than leave it ownerless until the grace runs out.
func TestLaunchReleasesARefusedTTYItem(t *testing.T) {
	d := openDesk(t)
	dropPending(t, d, "01-tty.sh", "#!/bin/bash\n# WHAT: x\n# WHY: y\n# TTY: yes\necho hi\n")
	scan(t, d)
	if _, err := d.Claim("01-tty", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Launch("01-tty"); err == nil {
		t.Fatal("Launch ran a TTY item detached")
	}
	if got := skipReason(t, d, "01-tty"); got != SkipLaunchFailed {
		t.Fatalf("skip reason %q, want %q", got, SkipLaunchFailed)
	}
}

// lostClaim claims an item that never gets an owner and moves the clock past
// the grace, so it is lost and Skip may move it.
func lostClaim(t *testing.T, d *Desk) string {
	t.Helper()
	name := claimOne(t, d)
	d.now = func() time.Time { return time.Now().Add(ClaimGrace * 10) }
	return name
}

// holdLock holds name's owner lock shared, as ownerAlive's probe does, from
// another open file, and lets go after hold. It returns once the lock is held.
func holdLock(t *testing.T, d *Desk, name string, hold time.Duration) {
	t.Helper()
	f, err := d.openLock(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(hold, func() { _ = f.Close() })
	t.Cleanup(func() { _ = f.Close() })
}

// A probe holding the lock for a moment does not make Skip refuse: Skip
// retries over a short window and goes ahead once the probe lets go.
func TestSkipWaitsOutABriefProbe(t *testing.T) {
	d := openDesk(t)
	name := lostClaim(t, d)
	holdLock(t, d, name, 30*time.Millisecond)
	if err := d.Skip(name, SkipLost); err != nil {
		t.Fatalf("Skip refused behind a 30ms probe: %v", err)
	}
	if got := skipReason(t, d, name); got != SkipLost {
		t.Fatalf("skip reason %q, want %q", got, SkipLost)
	}
}

func TestReleaseWaitsOutABriefProbe(t *testing.T) {
	d := openDesk(t)
	name := claimOne(t, d)
	holdLock(t, d, name, 30*time.Millisecond)
	if err := d.Release(name, SkipLaunchFailed); err != nil {
		t.Fatalf("Release refused behind a 30ms probe: %v", err)
	}
}

// A lock held for the whole retry window is a live owner: Skip and Release
// still refuse.
func TestALockHeldThroughoutStillRefuses(t *testing.T) {
	d := openDesk(t)
	name := lostClaim(t, d)
	holdLock(t, d, name, time.Duration(ownerLockTries+5)*ownerLockWait)
	if err := d.Skip(name, SkipLost); err == nil || !strings.Contains(err.Error(), "holds the lock") {
		t.Fatalf("Skip behind a held lock = %v, want a refusal", err)
	}
	if err := d.Release(name, SkipLaunchFailed); err == nil || !strings.Contains(err.Error(), "lock is held") {
		t.Fatalf("Release behind a held lock = %v, want a refusal", err)
	}
}

// When the meta cannot follow the item into running/, Claim fails and the
// item ends in skipped/ (launch-failed) with its hash, not in running/ for
// ever as a claim in progress; no orphaned meta is left in pending/.
func TestClaimMetaFailureReleasesTheItem(t *testing.T) {
	d := openDesk(t)
	dropPending(t, d, "01-hi.sh", script)
	scan(t, d)
	saved := claimMeta
	t.Cleanup(func() { claimMeta = saved })
	claimMeta = func(*Desk, string) error { return errors.New("meta rename failed") }
	if _, err := d.Claim("01-hi", ""); err == nil {
		t.Fatal("Claim succeeded with its meta left behind")
	}
	snap := scan(t, d)
	if len(snap.Running) != 0 || len(snap.Pending) != 0 {
		t.Fatalf("running %d, pending %d; want the item in skipped/ only", len(snap.Running), len(snap.Pending))
	}
	if len(snap.Skipped) != 1 || snap.Skipped[0].Meta.SkipReason != SkipLaunchFailed || snap.Skipped[0].Meta.SHA256 == "" {
		t.Fatalf("skipped = %+v, want one launch-failed item with its hash", snap.Skipped)
	}
	if _, err := os.Lstat(filepath.Join(d.Path(), DirPending, "01-hi"+extMeta)); err == nil {
		t.Error("the pending meta was left behind")
	}
}
