package drain

import (
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

func TestCloserCandidate(t *testing.T) {
	withID := Ledger{State: LedgerOurs, Row: worker.Row{Stage: worker.StageLaunched, GitHubRepo: "o/r", GitHubRepoID: 7}}
	noID := Ledger{State: LedgerOurs, Row: worker.Row{Stage: worker.StageLaunched}}
	cases := map[string]struct {
		q                   worker.QueueRow
		l                   Ledger
		candidate, identity bool
	}{
		"reported with identity":           {qrow("w", "/r", worker.QueueReported, t0), withID, true, true},
		"reported without identity":        {qrow("w", "/r", worker.QueueReported, t0), noID, true, false},
		"launched is not a candidate":      {qrow("w", "/r", worker.QueueLaunched, t0), withID, false, false},
		"failed is not a candidate":        {qrow("w", "/r", worker.QueueFailed, t0), withID, false, false},
		"closed ledger row is Settle's":    {qrow("w", "/r", worker.QueueReported, t0), Ledger{State: LedgerOurs, Row: worker.Row{Stage: worker.StageClosed, GitHubRepo: "o/r", GitHubRepoID: 7}}, false, false},
		"absent ledger row is Settle's":    {qrow("w", "/r", worker.QueueReported, t0), Ledger{State: LedgerAbsent}, false, false},
		"unreadable ledger waits":          {qrow("w", "/r", worker.QueueReported, t0), Ledger{State: LedgerUnreadable}, false, false},
		"another launch's row is Settle's": {qrow("w", "/r", worker.QueueReported, t0), Ledger{State: LedgerOther, Row: withID.Row}, false, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			candidate, identity := CloserCandidate(c.q, c.l)
			if candidate != c.candidate || identity != c.identity {
				t.Fatalf("CloserCandidate = %v, %v; want %v, %v", candidate, identity, c.candidate, c.identity)
			}
		})
	}
}

func TestCloserDue(t *testing.T) {
	if !CloserDue(time.Time{}, t0) {
		t.Fatal("a row never read must be due")
	}
	if CloserDue(t0, t0.Add(CloserReadEvery-time.Second)) {
		t.Fatal("due before 5 minutes")
	}
	if !CloserDue(t0, t0.Add(CloserReadEvery)) {
		t.Fatal("not due at 5 minutes")
	}
}

func TestDecideCloser(t *testing.T) {
	reported := qrow("w", "/r", worker.QueueReported, t0)
	closedAt := t0.Add(-time.Hour)
	withTimer := reported
	withTimer.PRClosedAt = &closedAt

	t.Run("merged closes and prices", func(t *testing.T) {
		d := DecideCloser(reported, PRRead{State: PRMerged, Number: 12}, t0)
		if !d.Close || !d.Merged || d.Change.Writes() || !strings.Contains(d.Why, "PR #12 merged") {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("closed unmerged records the time first", func(t *testing.T) {
		d := DecideCloser(reported, PRRead{State: PRClosed, Number: 12}, t0)
		if d.Close || !d.Change.SetPRClosedAt || d.Change.PRClosedAt == nil || !d.Change.PRClosedAt.Equal(t0) {
			t.Fatalf("%+v", d)
		}
		if d.Change.To != "" {
			t.Fatalf("recording pr_closed_at changed the state: %+v", d.Change)
		}
	})
	t.Run("closed unmerged waits out the grace", func(t *testing.T) {
		d := DecideCloser(withTimer, PRRead{State: PRClosed, Number: 12}, closedAt.Add(ClosedGrace-time.Second))
		if d.Close || d.Change.Writes() {
			t.Fatalf("closed before 24h: %+v", d)
		}
	})
	t.Run("closed unmerged closes at the grace", func(t *testing.T) {
		d := DecideCloser(withTimer, PRRead{State: PRClosed, Number: 12}, closedAt.Add(ClosedGrace))
		if !d.Close || d.Merged || !strings.Contains(d.Why, "closed unmerged") {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("reopened clears the timer", func(t *testing.T) {
		d := DecideCloser(withTimer, PRRead{State: PROpen, Number: 12}, t0)
		if d.Close || !d.Change.SetPRClosedAt || d.Change.PRClosedAt != nil {
			t.Fatalf("%+v", d)
		}
		r := withTimer
		d.Change.Apply(&r)
		if r.PRClosedAt != nil {
			t.Fatal("Apply kept pr_closed_at")
		}
	})
	t.Run("open with no timer does nothing", func(t *testing.T) {
		if d := DecideCloser(reported, PRRead{State: PROpen, Number: 12}, t0); d.Close || d.Change.Writes() {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("no PR leaves it reported", func(t *testing.T) {
		if d := DecideCloser(withTimer, PRRead{State: PRNone}, t0.Add(48*time.Hour)); d.Close || d.Change.Writes() {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("only reported rows", func(t *testing.T) {
		if d := DecideCloser(qrow("w", "/r", worker.QueueLaunched, t0), PRRead{State: PRMerged, Number: 1}, t0); d.Close {
			t.Fatalf("%+v", d)
		}
	})
}

func TestChangeCostAndTimer(t *testing.T) {
	r := qrow("w", "/r", worker.QueueReported, t0)
	cost := 1.25
	c := Change{Name: "w", From: r.State, CostUSD: &cost}
	if !c.Writes() {
		t.Fatal("a cost change must write")
	}
	c.Apply(&r)
	cost = 9
	if r.CostUSD == nil || *r.CostUSD != 1.25 {
		t.Fatalf("cost %v; Apply must copy the value", r.CostUSD)
	}
}
