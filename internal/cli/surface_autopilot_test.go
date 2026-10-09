//go:build unix

package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/surface/drain"
	"github.com/cameronsjo/forgectl/internal/surface/merge"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// seedMergeable puts a reported row in the queue whose own ledger row the
// autopilot can vouch for: launched, the queue's launch id, a branch the
// drain made new, and a recorded GitHub repository.
func seedMergeable(t *testing.T, q *worker.Queue, name string, at time.Time) {
	t.Helper()
	enqueueAt(t, q, name, closerRepo, at)
	id := seedState(t, q, name, worker.QueueReported)
	seedLedger(t, closerRepo, name, id, worker.StageLaunched, func(r *worker.Row) {
		r.BranchFrom = worker.BranchNew
		r.GitHubRepo, r.GitHubRepoID = closerGHRepo, closerRepoID
	})
}

func autoSettings() config.MergeSettings {
	s := manualSettings()
	s.Mode = config.MergeAuto
	return s
}

func TestDrainAutopilotOnlyInAuto(t *testing.T) {
	for _, mode := range []config.MergeMode{"", config.MergeOff, config.MergeManual} {
		t.Run(string(mode), func(t *testing.T) {
			f, d, q := newFakeDrain(t)
			seedMergeable(t, q, "w", drainT0.Add(-time.Hour))
			if mode != "" {
				f.mergeSettings = manualSettings()
				f.mergeSettings.Mode = mode
			}
			d.tick(t.Context())
			if len(f.landed) != 0 {
				t.Fatalf("mode %q: the autopilot tried %+v", mode, f.landed)
			}
		})
	}
}

func TestDrainAutopilotMergesThenTheClosersClose(t *testing.T) {
	f, d, q := newFakeDrain(t)
	seedMergeable(t, q, "w", drainT0.Add(-time.Hour))
	f.mergeSettings = autoSettings()
	f.landOut = merge.Outcome{Result: merge.LandMerged, PR: 9, Head: status1204Head, MergeCommit: "1111111111111111111111111111111111111111", AuditLine: "abcdef0123456789"}
	d.tick(t.Context())
	if len(f.landed) != 1 || f.landBy[0] != merge.ByDrain {
		t.Fatalf("landed %+v by %v", f.landed, f.landBy)
	}
	r := f.landed[0]
	if r.Name != "w" || r.QueueLaunchID == "" || r.LaunchID != r.QueueLaunchID || r.BranchFrom != worker.BranchNew || r.Stage != string(worker.StageLaunched) ||
		r.QueueState != string(worker.QueueReported) || r.GitHubRepoID != closerRepoID {
		t.Fatalf("the row the autopilot passed: %+v", r)
	}
	ev := eventsOf(f, "w", drain.EventMerged)
	if len(ev) != 1 || !strings.Contains(ev[0].Error, "PR #9 merged at head 3afe70e8bff8") {
		t.Fatalf("merged events %+v", ev)
	}
	// The closers read the row again at the next tick, inside their own
	// 5-minute spacing, and close it.
	f.prs["w"] = merge.Candidate{Number: 9, State: "MERGED"}
	f.now = f.now.Add(time.Second)
	d.tick(t.Context())
	if row := rowNamed(t, q, "w"); row.State != worker.QueueClosed {
		t.Fatalf("after the merge: %s", row.State)
	}
}

func TestDrainAutopilotOneAttemptATick(t *testing.T) {
	f, d, q := newFakeDrain(t)
	seedMergeable(t, q, "a", drainT0.Add(-2*time.Hour))
	seedMergeable(t, q, "b", drainT0.Add(-time.Hour))
	f.mergeSettings = autoSettings()
	f.landOut = merge.Outcome{Result: merge.LandRefused, PR: 1, Head: status1204Head, Reasons: []string{"the PR is a draft"}}
	d.tick(t.Context())
	d.tick(t.Context())
	d.tick(t.Context())
	if len(f.landed) != 2 || f.landed[0].Name == f.landed[1].Name {
		t.Fatalf("landed %+v; want one each, one a tick, then neither inside %s", f.landed, drain.AutopilotEvery)
	}
	f.now = f.now.Add(drain.AutopilotEvery)
	d.tick(t.Context())
	if len(f.landed) != 3 {
		t.Fatalf("landed %d after %s", len(f.landed), drain.AutopilotEvery)
	}
}

func TestDrainAutopilotRefusalEventsOncePerHeadAndReasons(t *testing.T) {
	f, d, q := newFakeDrain(t)
	seedMergeable(t, q, "w", drainT0.Add(-time.Hour))
	f.mergeSettings = autoSettings()
	f.landOut = merge.Outcome{Result: merge.LandRefused, PR: 3, Head: status1204Head, Reasons: []string{"b", "a"}}
	step := func() {
		d.tick(t.Context())
		f.now = f.now.Add(drain.AutopilotEvery)
	}
	step()
	f.landOut.Reasons = []string{"a", "b", "a"} // the same set
	step()
	if ev := eventsOf(f, "w", drain.EventMergeRefused); len(ev) != 1 || !strings.Contains(ev[0].Error, "PR #3 at head 3afe70e8bff8: refused, a; b") {
		t.Fatalf("refusal events %+v; want one", ev)
	}
	f.landOut.Reasons = []string{"c"}
	step()
	f.landOut.Head = "4444444444444444444444444444444444444444"
	step()
	f.landOut = merge.Outcome{Result: merge.LandUnconfirmed, PR: 3, Head: status1204Head, Reasons: []string{"not on main"}}
	step()
	if got := len(eventsOf(f, "w", drain.EventMergeRefused)); got != 4 {
		t.Fatalf("%d refusal events; want one per change of head, reasons or result", got)
	}
	f.landOut = merge.Outcome{Result: merge.LandUnreadable, Reasons: []string{"GitHub could not be read: HTTP 502"}}
	step()
	step()
	if ev := eventsOf(f, "w", drain.EventUnreadable); len(ev) != 1 {
		t.Fatalf("unreadable events %+v; want one", ev)
	}
}

// TestDrainAutopilotKillSwitch pins that the mode is read every tick:
// leaving auto stops the next attempt with no restart.
func TestDrainAutopilotKillSwitch(t *testing.T) {
	f, d, q := newFakeDrain(t)
	seedMergeable(t, q, "w", drainT0.Add(-time.Hour))
	f.mergeSettings = autoSettings()
	f.landOut = merge.Outcome{Result: merge.LandRefused, PR: 3, Reasons: []string{"x"}}
	d.tick(t.Context())
	f.mergeSettings.Mode = config.MergeManual
	f.now = f.now.Add(drain.AutopilotEvery)
	d.tick(t.Context())
	f.mergeSettings = config.MergeSettings{Mode: config.MergeOff, OffReason: "the config file is not valid"}
	f.now = f.now.Add(drain.AutopilotEvery)
	d.tick(t.Context())
	if len(f.landed) != 1 {
		t.Fatalf("landed %d; the autopilot ran after the mode left auto", len(f.landed))
	}
}

func TestDrainAutopilotSkipsRowsItCannotVouchFor(t *testing.T) {
	f, d, q := newFakeDrain(t)
	enqueueAt(t, q, "local", closerRepo, drainT0.Add(-time.Hour))
	id := seedState(t, q, "local", worker.QueueReported)
	seedLedger(t, closerRepo, "local", id, worker.StageLaunched, func(r *worker.Row) {
		r.BranchFrom = "local"
		r.GitHubRepo, r.GitHubRepoID = closerGHRepo, closerRepoID
	})
	seedReported(t, q, "noid", false)
	enqueueAt(t, q, "live", closerRepo, drainT0.Add(-time.Hour))
	id = seedState(t, q, "live", worker.QueueLaunched)
	seedLedger(t, closerRepo, "live", id, worker.StageLaunched, func(r *worker.Row) {
		r.BranchFrom = worker.BranchNew
		r.GitHubRepo, r.GitHubRepoID = closerGHRepo, closerRepoID
	})
	f.mergeSettings = autoSettings()
	d.tick(t.Context())
	if len(f.landed) != 0 {
		t.Fatalf("the autopilot tried %+v", f.landed)
	}
}

// TestDrainAutopilotAuditErrors pins that a refusal whose audit line could
// not be written is an error event, while a refusal the audit skipped as a
// repeat is not (T10.4 security review).
func TestDrainAutopilotAuditErrors(t *testing.T) {
	f, d, q := newFakeDrain(t)
	seedMergeable(t, q, "w", drainT0.Add(-time.Hour))
	f.mergeSettings = autoSettings()
	f.landOut = merge.Outcome{Result: merge.LandRefused, PR: 3, Head: status1204Head, Reasons: []string{"x"},
		AuditNote: "the same refusal is already in the audit file"}
	d.tick(t.Context())
	if ev := eventsOf(f, "w", drain.EventError); len(ev) != 0 {
		t.Fatalf("a repeated refusal raised %+v", ev)
	}
	full := errors.New("worker: merge-audit.jsonl is full")
	f.landOut.AuditNote, f.landOut.AuditErr = "the refusal could not be audited: "+full.Error(), full
	f.now = f.now.Add(drain.AutopilotEvery)
	d.tick(t.Context())
	if ev := eventsOf(f, "w", drain.EventError); len(ev) != 1 || !strings.Contains(ev[0].Error, "merge audit: the refusal could not be audited") {
		t.Fatalf("error events %+v; want one for the unwritten refusal", ev)
	}
}

// TestDrainAutopilotSkipsRowsItMerged pins that a row the autopilot merged
// is not tried again while it stays reported (GitHub, or the closers, not yet
// showing it merged).
func TestDrainAutopilotSkipsRowsItMerged(t *testing.T) {
	f, d, q := newFakeDrain(t)
	seedMergeable(t, q, "w", drainT0.Add(-time.Hour))
	f.mergeSettings = autoSettings()
	f.landOut = merge.Outcome{Result: merge.LandMerged, PR: 9, Head: status1204Head, MergeCommit: "1111111111111111111111111111111111111111", AuditLine: "abcdef0123456789"}
	f.prs["w"] = merge.Candidate{Number: 9, State: "OPEN"} // the closers' read lags
	d.tick(t.Context())
	for range 3 {
		f.now = f.now.Add(drain.AutopilotEvery)
		d.tick(t.Context())
	}
	if row := rowNamed(t, q, "w"); row.State != worker.QueueReported {
		t.Fatalf("the row is %s, want still reported", row.State)
	}
	if len(f.landed) != 1 {
		t.Fatalf("landed %d; the autopilot tried a row it merged again", len(f.landed))
	}
}
