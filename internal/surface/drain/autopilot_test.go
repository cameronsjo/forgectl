package drain

import (
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

func TestAutopilotCandidate(t *testing.T) {
	q := worker.QueueRow{Name: "w", State: worker.QueueReported, LaunchID: "launch-1"}
	l := Ledger{State: LedgerOurs, Row: worker.Row{Name: "w", Stage: worker.StageLaunched, LaunchID: "launch-1", BranchFrom: worker.BranchNew,
		GitHubRepo: "cameronsjo/forgectl", GitHubRepoID: 7}}
	if !AutopilotCandidate(q, l) {
		t.Fatal("the baseline row is not a candidate")
	}
	cases := map[string]func(*worker.QueueRow, *Ledger){
		"queue launched":     func(q *worker.QueueRow, _ *Ledger) { q.State = worker.QueueLaunched },
		"queue closed":       func(q *worker.QueueRow, _ *Ledger) { q.State = worker.QueueClosed },
		"no launch id":       func(q *worker.QueueRow, _ *Ledger) { q.LaunchID = "" },
		"another launch":     func(_ *worker.QueueRow, l *Ledger) { l.Row.LaunchID = "launch-2" },
		"ledger other":       func(_ *worker.QueueRow, l *Ledger) { l.State = LedgerOther },
		"ledger unreadable":  func(_ *worker.QueueRow, l *Ledger) { l.State = LedgerUnreadable },
		"ledger closed":      func(_ *worker.QueueRow, l *Ledger) { l.Row.Stage = worker.StageClosed },
		"branch from local":  func(_ *worker.QueueRow, l *Ledger) { l.Row.BranchFrom = "local" },
		"branch from origin": func(_ *worker.QueueRow, l *Ledger) { l.Row.BranchFrom = "origin" },
		"no repository":      func(_ *worker.QueueRow, l *Ledger) { l.Row.GitHubRepo = "" },
		"no repository id":   func(_ *worker.QueueRow, l *Ledger) { l.Row.GitHubRepoID = 0 },
	}
	for name, mutate := range cases {
		qq, ll := q, l
		mutate(&qq, &ll)
		if AutopilotCandidate(qq, ll) {
			t.Errorf("%s: a candidate", name)
		}
	}
}

func TestAutopilotDue(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if !AutopilotDue(time.Time{}, now) || AutopilotDue(now.Add(-AutopilotEvery+time.Second), now) || !AutopilotDue(now.Add(-AutopilotEvery), now) {
		t.Fatal("due")
	}
}
