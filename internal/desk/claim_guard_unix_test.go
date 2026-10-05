//go:build unix

package desk

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
)

// claimedBy rewrites the claim stamp on name to pid, as if another desk
// process had claimed it.
func claimedBy(t *testing.T, d *Desk, name string, pid int) {
	t.Helper()
	m, _, err := d.readMeta(DirRunning, name)
	if err != nil {
		t.Fatal(err)
	}
	m.ClaimPID, m.ClaimStart = pid, ownStart(pid)
	if err := d.writeMeta(DirRunning, name, m); err != nil {
		t.Fatal(err)
	}
}

// A TTY item claimed for a desk's own foreground has no owner until that
// desk's BeginRun. A stray `_supervise` given the stamped sha, run by a
// process that is neither the claimer nor its child, refuses in place: the
// item stays in running/ for the desk. Once the claimer is gone, the same
// supervisor moves it to skipped/, which shows the test can go red.
func TestAStraySupervisorLeavesAForegroundClaimAlone(t *testing.T) {
	d := openDesk(t)
	c := queue(t, d, "fg.sh", "# TTY: yes\nread -r x\n")
	claimer := exec.CommandContext(context.Background(), "sleep", "30")
	if err := claimer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = claimer.Process.Kill(); _ = claimer.Wait() })
	claimedBy(t, d, c.Name, claimer.Process.Pid)
	metaPath := filepath.Join(d.Path(), DirRunning, c.Name+".meta.json")
	before := readFile(t, metaPath)

	if rc, err := d.supervise(c.Name, c.SHA256, c.Kind); rc != 2 || err == nil {
		t.Fatalf("supervise = %d, %v; want a refusal", rc, err)
	}
	if s := scan(t, d); len(s.Running) != 1 || len(s.Skipped) != 0 {
		t.Fatalf("running %d, skipped %d; want the claim left in running/", len(s.Running), len(s.Skipped))
	}
	if after := readFile(t, metaPath); after != before {
		t.Fatalf("meta changed: %s -> %s", before, after)
	}

	_ = claimer.Process.Kill()
	_ = claimer.Wait()
	if rc, _ := d.supervise(c.Name, c.SHA256, c.Kind); rc != 2 {
		t.Fatalf("supervise after the claimer died = %d, want 2", rc)
	}
	if s := scan(t, d); len(s.Running) != 0 || len(s.Skipped) != 1 {
		t.Fatalf("running %d, skipped %d; want a dead claimer's claim skipped", len(s.Running), len(s.Skipped))
	}
}

// A Claim that fails after its stamp gives the claim back: the item leaves
// running/ as skipped:claim-failed instead of reading as running until the
// desk exits.
func TestAClaimThatFailsAfterItsStampGivesItBack(t *testing.T) {
	d := openDesk(t)
	a := addScript(t, d, "x.sh", "echo hi\n")
	scan(t, d)
	orig := stageRecord
	stageRecord = func(*Desk, string, []byte) (string, error) { return "", errors.New("disk full") }
	t.Cleanup(func() { stageRecord = orig })

	if _, err := d.Claim(a.Name, a.SHA256); err == nil {
		t.Fatal("Claim succeeded with its record write failing")
	}
	s := scan(t, d)
	if len(s.Running) != 0 || len(s.Skipped) != 1 || s.Skipped[0].Meta.SkipReason != SkipClaimFailed {
		t.Fatalf("running %d, skipped %+v; want it skipped as %s", len(s.Running), s.Skipped, SkipClaimFailed)
	}
}
