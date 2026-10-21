package workstate

import (
	"fmt"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/desk"
	"github.com/cameronsjo/forgectl/internal/surface/drain"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

func TestForQueueRow(t *testing.T) {
	closedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	merged := drain.DecideCloser(worker.QueueRow{State: worker.QueueReported}, drain.PRRead{State: drain.PRMerged, Number: 41}, closedAt).Why
	cases := []struct {
		name string
		row  worker.QueueRow
		want Work
	}{
		{"queued", worker.QueueRow{State: worker.QueueQueued}, Work{Status: Queued}},
		{"claimed", worker.QueueRow{State: worker.QueueClaimed}, Work{Status: Working}},
		{"launched", worker.QueueRow{State: worker.QueueLaunched}, Work{Status: Working}},
		{"needs-you", worker.QueueRow{State: worker.QueueNeedsYou}, Work{Status: Blocked, On: Operator, Reason: "worker at a prompt or idle with no report"}},
		{"reported, operator verifies", worker.QueueRow{State: worker.QueueReported, Batch: "fixes"}, Work{Status: Delivered, On: Operator, Reason: "review the report and PR"}},
		{"reported, chief of staff verifies", worker.QueueRow{State: worker.QueueReported, Batch: "cos-2026-10-19"}, Work{Status: Delivered, On: ChiefOfStaff, Reason: "review the report and PR"}},
		{"reported, PR closed unmerged", worker.QueueRow{State: worker.QueueReported, Batch: "cos-2026-10-19", PRClosedAt: &closedAt}, Work{Status: Delivered, On: Operator, Reason: "PR closed unmerged; the drain closes the worker after a grace period"}},
		{"failed is not an end", worker.QueueRow{State: worker.QueueFailed}, Work{Status: Blocked, On: Operator, Reason: "failed: retry or drop"}},
		{"closed after merge", worker.QueueRow{State: worker.QueueClosed, LastError: merged}, Work{Status: Resolved, Evidence: "merged PR #41"}},
		{"closed after merge, worktree kept", worker.QueueRow{State: worker.QueueClosed, LastError: merged + "; worktree kept: dirty"}, Work{Status: Resolved, Evidence: "merged PR #41"}},
		{"closed unmerged", worker.QueueRow{State: worker.QueueClosed, PRClosedAt: &closedAt, LastError: "PR #41 closed unmerged at …"}, Work{Status: Dropped, Reason: "PR closed unmerged"}},
		{"closed by hand", worker.QueueRow{State: worker.QueueClosed, LastError: "the worker's ledger row is gone (surface close removed it)"}, Work{Status: Dropped, Reason: "closed with no merge recorded"}},
		{"closed, merge text not at the start", worker.QueueRow{State: worker.QueueClosed, LastError: "note: PR #41 merged; the drain closed the worker"}, Work{Status: Dropped, Reason: "closed with no merge recorded"}},
		{"expired", worker.QueueRow{State: worker.QueueExpired}, Work{Status: Expired, Reason: "sat queued too long and was never launched"}},
		{"unknown", worker.QueueRow{State: "parked"}, Work{Status: Stalled, Reason: "unknown queue state parked"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ForQueueRow(c.row); got != c.want {
				t.Errorf("ForQueueRow = %+v, want %+v", got, c.want)
			}
		})
	}
}

// Every queue state maps to a status, and the queue's own terminal states
// split as the vocabulary says: reported and failed stay open.
func TestForQueueRowEndsOnlyRealEnds(t *testing.T) {
	ends := map[worker.QueueState]bool{worker.QueueClosed: true, worker.QueueExpired: true}
	for _, s := range []worker.QueueState{worker.QueueQueued, worker.QueueClaimed, worker.QueueLaunched, worker.QueueNeedsYou,
		worker.QueueReported, worker.QueueFailed, worker.QueueClosed, worker.QueueExpired} {
		w := ForQueueRow(worker.QueueRow{State: s})
		if w.Status.Ended() != ends[s] {
			t.Errorf("%s: ended = %v, want %v", s, w.Status.Ended(), ends[s])
		}
		if (w.Status == Blocked || w.Status == Delivered) != (w.On != "") {
			t.Errorf("%s: on = %q with status %s; on is set exactly for blocked and delivered", s, w.On, w.Status)
		}
	}
}

func TestForDrain(t *testing.T) {
	cases := []struct {
		name    string
		status  string
		pause   string
		waiting int
		want    Work
		claim   bool
	}{
		{"running", drain.StatusRunning, "", 0, Work{Status: Working}, true},
		{"paused on herdr retries itself", drain.StatusPaused, "herdr: socket gone", 2, Work{Status: Blocked, On: System, Reason: "paused: herdr"}, true},
		{"paused on github auth", drain.StatusPaused, "github-auth: bad credentials", 2, Work{Status: Blocked, On: Operator, Reason: "paused: github-auth"}, true},
		{"one operator pause among system ones", drain.StatusPaused, "github: 502; herdr: gone; config: bad [surface.drain]", 0, Work{Status: Blocked, On: Operator, Reason: "paused: github, herdr, config"}, true},
		{"a reason holding '; ' adds no kind", drain.StatusPaused, "herdr: dial: refused; retrying", 0, Work{Status: Blocked, On: System, Reason: "paused: herdr"}, true},
		{"unknown pause kind is the operator's", drain.StatusPaused, "quota: spent", 0, Work{Status: Blocked, On: Operator, Reason: "paused: quota"}, true},
		{"paused with no reason", drain.StatusPaused, "", 0, Work{Status: Blocked, On: Operator, Reason: "paused"}, true},
		{"stale", drain.StatusStale, "", 0, Work{Status: Stalled, Reason: "the drain's last tick is stale"}, true},
		{"stopped with rows waiting", drain.StatusStopped, "", 1, Work{Status: Blocked, On: Operator, Reason: "drain stopped with rows waiting"}, true},
		{"stopped with nothing waiting", drain.StatusStopped, "", 0, Work{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, claim := ForDrain(c.status, c.pause, c.waiting)
			if got != c.want || claim != c.claim {
				t.Errorf("ForDrain = %+v, %v; want %+v, %v", got, claim, c.want, c.claim)
			}
		})
	}
}

// The pause kinds ForDrain parses are the ones drain.Pauses writes.
func TestPauseKindsReadPausesReason(t *testing.T) {
	p := drain.Pauses{}
	p.Set(drain.PauseGitHubAuth, "bad credentials; run gh auth login")
	p.Set(drain.PauseHerdr, "socket: gone")
	got := fmt.Sprint(pauseKinds(p.Reason()))
	if got != "[github-auth herdr]" {
		t.Errorf("pauseKinds(%q) = %s", p.Reason(), got)
	}
}

func TestForDeskItem(t *testing.T) {
	zero, one := 0, 1
	cases := []struct {
		name string
		it   desk.Item
		want Work
	}{
		{"waiting", desk.Item{State: desk.StateWaiting}, Work{Status: Blocked, On: Operator, Reason: "approve or skip"}},
		{"refused", desk.Item{State: desk.StateRefused, Refusal: "too large"}, Work{Status: Blocked, On: Operator, Reason: "refused: too large"}},
		{"held", desk.Item{State: desk.StateHeld, Hold: "after 03-build"}, Work{Status: Queued, Reason: "held: after 03-build"}},
		{"running", desk.Item{State: desk.StateRunning}, Work{Status: Working}},
		{"lost", desk.Item{State: desk.StateLost}, Work{Status: Stalled, Reason: "run lost: its owner died before it ended"}},
		{"done, exit 0", desk.Item{State: desk.StateDone, ExitCode: &zero}, Work{Status: Resolved, Evidence: "exit 0"}},
		{"done, exit 1 is not resolved", desk.Item{State: desk.StateDone, ExitCode: &one}, Work{Status: Blocked, On: Operator, Reason: "failed: exit 1"}},
		{"done, no exit code", desk.Item{State: desk.StateDone}, Work{Status: Blocked, On: Operator, Reason: "ended with no exit code recorded"}},
		{"skipped", desk.Item{State: desk.StateSkipped, Meta: desk.Meta{SkipReason: "stale"}}, Work{Status: Dropped, Reason: "skipped: stale"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ForDeskItem(c.it); got != c.want {
				t.Errorf("ForDeskItem = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestNeedsOperator(t *testing.T) {
	cases := map[Work]bool{
		{Status: Blocked, On: Operator}:        true,
		{Status: Delivered, On: Operator}:      true,
		{Status: Stalled}:                      true,
		{Status: Delivered, On: ChiefOfStaff}:  false,
		{Status: Blocked, On: System}:          false,
		{Status: Working}:                      false,
		{Status: Resolved, Evidence: "exit 0"}: false,
	}
	for w, want := range cases {
		if got := w.NeedsOperator(); got != want {
			t.Errorf("%+v.NeedsOperator() = %v, want %v", w, got, want)
		}
	}
}
