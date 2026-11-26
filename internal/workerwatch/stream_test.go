package workerwatch

import (
	"testing"
	"time"
)

func TestDiffTransitions(t *testing.T) {
	t0 := time.Date(2026, 11, 26, 16, 0, 0, 0, time.UTC)
	t1 := t0.Add(5 * time.Second)

	prev := Snapshot{
		Taken: t0,
		Workers: []Worker{
			{
				Name:    "cos-a",
				Address: "t1",
				State:   "live",
				Status:  StatusWorking,
				Reports: Reports{Files: []ReportFile{{Name: "report.md", Complete: false}}},
				PR:      &PR{Repo: "cameronsjo/cadence", Number: 42, State: "OPEN", Checks: "PENDING"},
			},
			{
				Name:    "cos-drop",
				Address: "t2",
				State:   "live",
				Status:  StatusDone,
			},
			{
				Name:    "cos-glitch",
				Address: "t3",
				State:   "live",
				Status:  StatusWorking,
			},
		},
	}

	cur := Snapshot{
		Taken: t1,
		Workers: []Worker{
			{
				Name:    "cos-a",
				Address: "t1",
				State:   "live",
				Status:  StatusBlocked,
				Reports: Reports{Files: []ReportFile{{Name: "report.md", Complete: true}}},
				PR:      &PR{Repo: "cameronsjo/cadence", Number: 42, State: "OPEN", Checks: "SUCCESS"},
			},
			{
				Name:    "cos-glitch",
				Address: "t3",
				HCError: "hc get failed",
			},
			{
				Name:    "cos-new",
				Address: "t4",
				State:   "live",
				Status:  StatusWorking,
			},
		},
	}

	got := Diff(prev, cur)
	if len(got) != 5 {
		t.Fatalf("expected 5 events, got %d: %+v", len(got), got)
	}

	if got[0].Type != EventStatusChanged || got[0].Worker != "cos-a" || !got[0].NeedsYou || got[0].Prev != "live/working" || got[0].Status != StatusBlocked {
		t.Errorf("unexpected event[0]: %+v", got[0])
	}
	if got[1].Type != EventReportComplete || got[1].Worker != "cos-a" || got[1].Report != "report.md" {
		t.Errorf("unexpected event[1]: %+v", got[1])
	}
	if got[2].Type != EventPRChanged || got[2].Worker != "cos-a" || got[2].PR != 42 || got[2].Checks != "SUCCESS" {
		t.Errorf("unexpected event[2]: %+v", got[2])
	}
	if got[3].Type != EventWorkerAppeared || got[3].Worker != "cos-new" || got[3].Address != "t4" {
		t.Errorf("unexpected event[3]: %+v", got[3])
	}
	if got[4].Type != EventWorkerGone || got[4].Worker != "cos-drop" || got[4].Prev != "live/done" {
		t.Errorf("unexpected event[4]: %+v", got[4])
	}
}

func TestDiffHCAvailability(t *testing.T) {
	t0 := time.Date(2026, 11, 26, 16, 0, 0, 0, time.UTC)
	down := Diff(Snapshot{Taken: t0}, Snapshot{Taken: t0.Add(time.Second), HCUnavailable: "hc not found"})
	if len(down) != 1 || down[0].Type != EventHCUnavailable || down[0].Reason != "hc not found" {
		t.Fatalf("expected hc_unavailable, got %+v", down)
	}
	up := Diff(Snapshot{Taken: t0, HCUnavailable: "hc not found"}, Snapshot{Taken: t0.Add(time.Second)})
	if len(up) != 1 || up[0].Type != EventHCAvailable {
		t.Fatalf("expected hc_available, got %+v", up)
	}
}

func TestDiffNoOpOnIdenticalSnapshots(t *testing.T) {
	t0 := time.Date(2026, 11, 26, 16, 0, 0, 0, time.UTC)
	snap := Snapshot{
		Taken: t0,
		Workers: []Worker{
			{
				Name:    "cos-a",
				Address: "t1",
				State:   "live",
				Status:  StatusWorking,
				Reports: Reports{Files: []ReportFile{{Name: "report.md", Complete: true}}},
				PR:      &PR{Repo: "cameronsjo/cadence", Number: 42, State: "OPEN", Checks: "SUCCESS"},
			},
		},
	}
	if got := Diff(snap, snap); len(got) != 0 {
		t.Fatalf("expected 0 events on identical snapshot, got %+v", got)
	}
}
