//go:build unix

package desk

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// claimOne queues and claims one plain item, returning its name.
func claimOne(t *testing.T, d *Desk) string {
	t.Helper()
	dropPending(t, d, "01-hi.sh", script)
	scan(t, d)
	if _, err := d.Claim("01-hi", ""); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return "01-hi"
}

func skipReason(t *testing.T, d *Desk, name string) string {
	t.Helper()
	for _, it := range scan(t, d).Skipped {
		if it.Name == name {
			return it.Meta.SkipReason
		}
	}
	t.Fatalf("%s is not in skipped/", name)
	return ""
}

// A supervisor that cannot be started leaves no owner: Launch releases the
// claim to skipped/ (launch-failed) at once, and it can never be re-armed.
func TestLaunchFailureReleasesTheClaim(t *testing.T) {
	d := openDesk(t)
	name := claimOne(t, d)
	saved := supervisorArgv
	t.Cleanup(func() { supervisorArgv = saved })
	supervisorArgv = func(string, string) ([]string, error) {
		return []string{"/nonexistent/forgectl-supervisor"}, nil
	}
	if _, err := d.Launch(name); err == nil {
		t.Fatal("Launch of a missing supervisor succeeded")
	}
	if got := skipReason(t, d, name); got != SkipLaunchFailed {
		t.Fatalf("skip reason %q, want %q", got, SkipLaunchFailed)
	}
	if err := d.Unskip(name); err == nil {
		t.Fatal("a launch-failed item was re-armed")
	}
}

func TestLaunchArgvFailureReleasesTheClaim(t *testing.T) {
	d := openDesk(t)
	name := claimOne(t, d)
	saved := supervisorArgv
	t.Cleanup(func() { supervisorArgv = saved })
	supervisorArgv = func(string, string) ([]string, error) { return nil, errors.New("no binary") }
	if _, err := d.Launch(name); err == nil {
		t.Fatal("Launch succeeded with no supervisor argv")
	}
	if got := skipReason(t, d, name); got != SkipLaunchFailed {
		t.Fatalf("skip reason %q, want %q", got, SkipLaunchFailed)
	}
}

func TestReleaseRefusesARunWithAnOwner(t *testing.T) {
	d := openDesk(t)
	name := claimOne(t, d)
	run, err := d.BeginRun(name, 1) // pid 1 stands in for a live owner
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Events.Close() })
	if err := d.Release(name, SkipLaunchFailed); err == nil || !strings.Contains(err.Error(), "has an owner") {
		t.Fatalf("Release of an owned run = %v", err)
	}
}

// A claim with no owner is still starting inside ClaimGrace and lost past
// it: Scan shows it lost, Skip(SkipLost) moves it on, and a watcher reports
// RUN-LOST.
func TestOwnerlessClaimIsLostAfterTheGrace(t *testing.T) {
	d := openDesk(t)
	start := time.Now()
	d.now = func() time.Time { return start }
	name := claimOne(t, d)

	d.now = func() time.Time { return start.Add(ClaimGrace - time.Second) }
	if got := scan(t, d).Running[0].State; got != StateRunning {
		t.Fatalf("inside the grace: state %s, want running", got)
	}
	if err := d.Skip(name, SkipLost); err == nil {
		t.Fatal("inside the grace, Skip moved a claim that may still start")
	}
	w, err := d.NewWatcher(name, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, st, _ := w.Poll(); st != WatchWaiting {
		t.Fatalf("inside the grace: watch state %s, want waiting", st)
	}

	d.now = func() time.Time { return start.Add(ClaimGrace + time.Second) }
	if got := scan(t, d).Running[0].State; got != StateLost {
		t.Fatalf("past the grace: state %s, want lost", got)
	}
	lines, st, err := w.Poll()
	if err != nil || st != WatchLost {
		t.Fatalf("past the grace: watch state %s err %v, want lost", st, err)
	}
	if want := "RUN-LOST id=" + name + " pid=0"; len(lines) == 0 || lines[len(lines)-1] != want {
		t.Fatalf("watch lines %q, want a last line %q", lines, want)
	}
	if err := d.Skip(name, SkipLost); err != nil {
		t.Fatalf("Skip(lost) past the grace: %v", err)
	}
	if got := skipReason(t, d, name); got != SkipLost {
		t.Fatalf("skip reason %q, want %q", got, SkipLost)
	}
}

// A claim made before claimed_at existed falls back to the running/ file's
// mtime, which Claim writes last.
func TestOwnerlessClaimWithoutClaimedAtUsesTheFileTime(t *testing.T) {
	d := openDesk(t)
	name := claimOne(t, d)
	meta, _, err := d.readMeta(DirRunning, name)
	if err != nil {
		t.Fatal(err)
	}
	meta.ClaimedAt = nil
	if err := d.writeMeta(DirRunning, name, meta); err != nil {
		t.Fatal(err)
	}
	if d.lost(name, meta) {
		t.Fatal("a fresh claim with no claimed_at read as lost")
	}
	d.now = func() time.Time { return time.Now().Add(ClaimGrace + time.Minute) }
	if !d.lost(name, meta) {
		t.Fatal("an old claim with no claimed_at and no owner should be lost")
	}
}
