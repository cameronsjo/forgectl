//go:build unix

package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/drain"
	"github.com/cameronsjo/forgectl/internal/surface/merge"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

const (
	closerRepo   = "/repo/a"
	closerGHRepo = "cameronsjo/forgectl"
	closerRepoID = 1252924951
	closerOpID   = 4084915
)

// seedReported puts a reported row named name in the queue with its own
// launched ledger row, which records the GitHub repository when identity is
// true, and a transcript.
func seedReported(t *testing.T, q *worker.Queue, name string, identity bool) {
	t.Helper()
	enqueueAt(t, q, name, closerRepo, drainT0.Add(-time.Hour))
	id := seedState(t, q, name, worker.QueueReported)
	seedLedger(t, closerRepo, name, id, worker.StageLaunched, func(r *worker.Row) {
		r.Transcript = "/t/" + name + ".jsonl"
		if identity {
			r.GitHubRepo, r.GitHubRepoID = closerGHRepo, closerRepoID
		}
	})
}

func eventsOf(f *fakeDrain, name, kind string) []drain.Event {
	var out []drain.Event
	for _, e := range f.events {
		if e.Name == name && e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestDrainCloserMerged(t *testing.T) {
	cases := map[string]struct {
		usage *statusUsage
		cost  *float64
	}{
		"priced":               {&statusUsage{CostUSD: 1.5, Priced: true}, ptr(1.5)},
		"cadence-hooks absent": {nil, nil},
		"partial price":        {&statusUsage{CostUSD: 0.4, Priced: false, UnpricedModels: []string{"x"}}, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f, d, q := newFakeDrain(t)
			seedReported(t, q, "w", true)
			f.prs["w"] = merge.Candidate{Number: 5, State: "MERGED"}
			f.usage = c.usage
			d.tick(t.Context())
			r := rowNamed(t, q, "w")
			if r.State != worker.QueueClosed || !strings.Contains(r.LastError, "PR #5 merged") {
				t.Fatalf("row %s %q; want closed naming the merge", r.State, r.LastError)
			}
			if (r.CostUSD == nil) != (c.cost == nil) || (r.CostUSD != nil && *r.CostUSD != *c.cost) {
				t.Fatalf("cost_usd %v, want %v", r.CostUSD, c.cost)
			}
			if strings.Join(f.closed, ",") != "w" {
				t.Fatalf("closed %v, want the close run once for w", f.closed)
			}
			if ev := eventsOf(f, "w", drain.EventState); len(ev) == 0 || ev[len(ev)-1].State != string(worker.QueueClosed) {
				t.Fatalf("no closed state event: %+v", f.events)
			}
		})
	}
}

func ptr(v float64) *float64 { return &v }

func TestDrainCloserCloseRefusedStaysReported(t *testing.T) {
	f, d, q := newFakeDrain(t)
	seedReported(t, q, "w", true)
	f.prs["w"] = merge.Candidate{Number: 5, State: "MERGED"}
	f.usage = &statusUsage{CostUSD: 2, Priced: true}
	f.closeRes = &closeResult{Name: "w", Closed: false, Workspace: closeWorkspaceRefused, Reason: "herdr could not be read (down)"}
	d.tick(t.Context())
	r := rowNamed(t, q, "w")
	if r.State != worker.QueueReported {
		t.Fatalf("row %s; a refused close must leave it reported", r.State)
	}
	if r.CostUSD == nil || *r.CostUSD != 2 {
		t.Fatalf("cost_usd %v; the price is kept even when the close refuses", r.CostUSD)
	}
	ev := eventsOf(f, "w", drain.EventError)
	if len(ev) != 1 || !strings.Contains(ev[0].Error, "close refused") || !strings.Contains(ev[0].Error, "herdr could not be read") {
		t.Fatalf("events %+v; want one error naming why", ev)
	}
	f.now = f.now.Add(drain.CloserReadEvery)
	d.tick(t.Context())
	if got := len(eventsOf(f, "w", drain.EventError)); got != 1 {
		t.Fatalf("%d refusal events after a second refusal; want the same reason once", got)
	}
	f.closeRes = nil
	f.now = f.now.Add(drain.CloserReadEvery)
	d.tick(t.Context())
	if r := rowNamed(t, q, "w"); r.State != worker.QueueClosed {
		t.Fatalf("row %s after the close stops refusing", r.State)
	}
}

func TestDrainCloserClosedUnmergedGrace(t *testing.T) {
	f, d, q := newFakeDrain(t)
	seedReported(t, q, "w", true)
	f.prs["w"] = merge.Candidate{Number: 7, State: "CLOSED"}
	d.tick(t.Context())
	r := rowNamed(t, q, "w")
	if r.State != worker.QueueReported || r.PRClosedAt == nil || !r.PRClosedAt.Equal(drainT0) {
		t.Fatalf("first sight: %s pr_closed_at %v; want reported with pr_closed_at %s", r.State, r.PRClosedAt, drainT0)
	}
	if ev := eventsOf(f, "w", drain.EventNote); len(ev) != 1 || !strings.Contains(ev[0].Error, "closed unmerged") {
		t.Fatalf("notes %+v", ev)
	}
	f.now = drainT0.Add(drain.ClosedGrace - drain.CloserReadEvery - time.Second)
	d.tick(t.Context())
	if r := rowNamed(t, q, "w"); r.State != worker.QueueReported || len(f.closed) != 0 {
		t.Fatalf("before 24h: %s, closed %v", r.State, f.closed)
	}
	f.now = drainT0.Add(drain.ClosedGrace)
	d.tick(t.Context())
	r = rowNamed(t, q, "w")
	if r.State != worker.QueueClosed || !strings.Contains(r.LastError, "closed unmerged") || r.CostUSD != nil {
		t.Fatalf("at 24h: %s %q cost %v; want closed, unpriced", r.State, r.LastError, r.CostUSD)
	}
}

func TestDrainCloserReopenClearsTimer(t *testing.T) {
	f, d, q := newFakeDrain(t)
	seedReported(t, q, "w", true)
	f.prs["w"] = merge.Candidate{Number: 7, State: "CLOSED"}
	d.tick(t.Context())
	f.prs["w"] = merge.Candidate{Number: 7, State: "OPEN"}
	f.now = f.now.Add(drain.CloserReadEvery)
	d.tick(t.Context())
	if r := rowNamed(t, q, "w"); r.PRClosedAt != nil || r.State != worker.QueueReported {
		t.Fatalf("reopened: %s pr_closed_at %v; want reported, cleared", r.State, r.PRClosedAt)
	}
	// Closed again: the timer starts over from the new sighting.
	f.prs["w"] = merge.Candidate{Number: 7, State: "CLOSED"}
	f.now = drainT0.Add(drain.ClosedGrace)
	d.tick(t.Context())
	r := rowNamed(t, q, "w")
	if r.State != worker.QueueReported || r.PRClosedAt == nil || !r.PRClosedAt.Equal(f.now) {
		t.Fatalf("closed again: %s pr_closed_at %v; want reported, timer from %s", r.State, r.PRClosedAt, f.now)
	}
}

func TestDrainCloserNoPRStaysReported(t *testing.T) {
	f, d, q := newFakeDrain(t)
	seedReported(t, q, "w", true)
	d.tick(t.Context())
	if r := rowNamed(t, q, "w"); r.State != worker.QueueReported || f.prReads["w"] != 1 {
		t.Fatalf("no PR: %s after %d reads", r.State, f.prReads["w"])
	}
}

func TestDrainCloserNoIdentitySkipped(t *testing.T) {
	f, d, q := newFakeDrain(t)
	seedReported(t, q, "w", false)
	f.prs["w"] = merge.Candidate{Number: 5, State: "MERGED"}
	d.tick(t.Context())
	f.now = f.now.Add(drain.CloserReadEvery)
	d.tick(t.Context())
	if r := rowNamed(t, q, "w"); r.State != worker.QueueReported || f.prReads["w"] != 0 {
		t.Fatalf("no identity: %s after %d reads; want reported, never read", r.State, f.prReads["w"])
	}
	if ev := eventsOf(f, "w", drain.EventNote); len(ev) != 1 || !strings.Contains(ev[0].Error, "records no GitHub repository") {
		t.Fatalf("notes %+v; want one", ev)
	}
}

func TestDrainCloserReadErrorChangesNothing(t *testing.T) {
	transient := &exec.CommandError{Name: "gh", Stderr: `Post "https://api.github.com/graphql": dial tcp: i/o timeout`, ExitCode: 1, Err: errors.New("exit status 1")}
	cases := map[string]struct {
		err  error
		kind string
	}{
		"transient":  {transient, drain.EventUnreadable},
		"refusal":    {errors.New("merge: unusable GitHub response: Could not resolve to a Repository"), drain.EventError},
		"repo moved": {merge.ErrRepoChanged, drain.EventError},
		"two open":   {merge.ErrAmbiguousPR, drain.EventError},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f, d, q := newFakeDrain(t)
			seedReported(t, q, "w", true)
			before := rowNamed(t, q, "w")
			f.prErr["w"] = c.err
			for i := 0; i < 3; i++ {
				d.tick(t.Context())
				f.now = f.now.Add(drain.CloserReadEvery)
			}
			after := rowNamed(t, q, "w")
			if after.State != worker.QueueReported || !after.StateAt.Equal(before.StateAt) || after.LastError != before.LastError || after.PRClosedAt != nil {
				t.Fatalf("a read error changed the row: %+v", after)
			}
			if f.prReads["w"] != 3 {
				t.Fatalf("%d reads; want one per due tick", f.prReads["w"])
			}
			if ev := eventsOf(f, "w", c.kind); len(ev) != 1 {
				t.Fatalf("%d %s events; want one for the condition", len(ev), c.kind)
			}
			if c.kind == drain.EventUnreadable && d.pauses.Paused() {
				t.Fatalf("a closer read failure paused claiming: %q", d.pauses.Reason())
			}
		})
	}
}

func TestDrainCloserReadRate(t *testing.T) {
	f, d, q := newFakeDrain(t)
	names := []string{"r1", "r2", "r3", "r4", "r5", "r6"}
	for _, n := range names {
		seedReported(t, q, n, true)
	}
	total := func() int {
		n := 0
		for _, c := range f.prReads {
			n += c
		}
		return n
	}
	d.tick(t.Context())
	if got := total(); got != drain.CloserReadsPerTick {
		t.Fatalf("first tick read %d PRs, want the per-tick cap %d", got, drain.CloserReadsPerTick)
	}
	d.tick(t.Context())
	if got := total(); got != len(names) {
		t.Fatalf("second tick: %d reads in all, want %d (the rows not yet read)", got, len(names))
	}
	f.now = f.now.Add(drain.CloserReadEvery - time.Second)
	d.tick(t.Context())
	if got := total(); got != len(names) {
		t.Fatalf("before 5 minutes: %d reads; no row is due", got)
	}
	f.now = drainT0.Add(drain.CloserReadEvery)
	d.tick(t.Context())
	for _, n := range names[:drain.CloserReadsPerTick] {
		if f.prReads[n] != 2 {
			t.Fatalf("at 5 minutes %s was read %d times, want 2 (oldest read first)", n, f.prReads[n])
		}
	}
}

// discoverJSON is a DiscoverQuery answer for worker/w on cameronsjo/forgectl
// holding nodes.
func discoverJSON(nodes string) string {
	return `{"data":{"viewer":{"login":"cameronsjo","databaseId":4084915},"repository":{"databaseId":1252924951,"nameWithOwner":"cameronsjo/forgectl","defaultBranchRef":{"name":"main"},"pullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[` + nodes + `]}}}}`
}

const (
	// forkNode is a PR on the same head name from the operator's own fork.
	forkNodeFmt = `{"number":901,"state":"%s","isCrossRepository":true,"createdAt":"2026-10-09T17:00:00Z","headRefName":"worker/w","headRepository":{"databaseId":999000111},"author":{"__typename":"User","login":"cameronsjo","databaseId":4084915}}`
	ownMerged   = `{"number":12,"state":"MERGED","isCrossRepository":false,"createdAt":"2026-10-09T16:00:00Z","headRefName":"worker/w","headRepository":{"databaseId":1252924951},"author":{"__typename":"User","login":"cameronsjo","databaseId":4084915}}`
	// ownMergedEarlier is the operator's merged PR on worker/w from a launch
	// before the ledger row's started_at (drainT0): a reused name's old PR.
	ownMergedEarlier = `{"number":11,"state":"MERGED","isCrossRepository":false,"createdAt":"2026-10-07T11:59:59Z","headRefName":"worker/w","headRepository":{"databaseId":1252924951},"author":{"__typename":"User","login":"cameronsjo","databaseId":4084915}}`
)

func TestDrainCloserForkPRIgnored(t *testing.T) {
	cases := map[string]struct {
		nodes string
		want  worker.QueueState
	}{
		// An open fork PR must not hide the worker's merged one.
		"open fork beside the merged PR": {strings.Replace(forkNodeFmt, "%s", "OPEN", 1) + "," + ownMerged, worker.QueueClosed},
		// A merged fork PR is not the worker's PR.
		"merged fork PR alone": {strings.Replace(forkNodeFmt, "%s", "MERGED", 1), worker.QueueReported},
		// The operator's merged PR from before this launch started is an
		// earlier launch's under the same name: it must not close this one.
		"own merged PR from an earlier launch": {ownMergedEarlier, worker.QueueReported},
		// One created after the launch started is this launch's.
		"own merged PR from this launch beside an earlier one": {ownMergedEarlier + "," + ownMerged, worker.QueueClosed},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f, d, q := newFakeDrain(t)
			seedReported(t, q, "w", true)
			gh := &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
				if name != "gh" || !strings.Contains(strings.Join(args, " "), "head=worker/w") {
					t.Fatalf("unexpected call %s %v", name, args)
				}
				return discoverJSON(c.nodes), nil
			}}
			d.io.prRead = merge.Reader{GH: gh}.Discover
			d.tick(t.Context())
			if r := rowNamed(t, q, "w"); r.State != c.want {
				t.Fatalf("row %s, want %s (closed %v)", r.State, c.want, f.closed)
			}
		})
	}
}

func TestDrainDailyPruneOncePerDay(t *testing.T) {
	f, d, q := newFakeDrain(t)
	_ = q
	f.herdrErr = errors.New("down")
	d.tick(t.Context())
	d.tick(t.Context())
	if f.pruneRuns != 1 || f.pruneDay != "2026-10-07" {
		t.Fatalf("prune ran %d times, day %q; want once, recorded", f.pruneRuns, f.pruneDay)
	}
	// A restart reads the recorded day and does not run it again.
	restarted := newDrainer(d.io, drainTestSession, func() bool { return false })
	restarted.tick(t.Context())
	if f.pruneRuns != 1 {
		t.Fatalf("a restart the same day pruned again (%d runs)", f.pruneRuns)
	}
	f.now = time.Date(2026, 10, 8, 0, 0, 1, 0, time.UTC)
	f.pruneErr = errors.New("queue unreadable")
	restarted.tick(t.Context())
	if f.pruneRuns != 2 || f.pruneDay != "2026-10-08" {
		t.Fatalf("next UTC day: %d runs, day %q", f.pruneRuns, f.pruneDay)
	}
	failed := false
	for _, e := range f.events {
		failed = failed || (e.Kind == drain.EventError && strings.Contains(e.Error, "daily prune: queue unreadable"))
	}
	if !failed {
		t.Fatalf("no event for the failed prune: %+v", f.events)
	}
	restarted.tick(t.Context())
	if f.pruneRuns != 2 {
		t.Fatalf("a failed prune ran again the same day (%d runs)", f.pruneRuns)
	}
}

// TestDrainCloserLedgerWriteFailureStaysReported pins that a close whose
// ledger row could not be marked closed or removed leaves the queue row
// reported, with the note as an error event and as last_error, and that the
// next read closes again and settles it.
func TestDrainCloserLedgerWriteFailureStaysReported(t *testing.T) {
	for name, note := range map[string]string{
		"not marked closed": "the ledger row could not be marked closed: disk full",
		"not removed":       "the ledger row could not be removed: disk full",
	} {
		t.Run(name, func(t *testing.T) {
			f, d, q := newFakeDrain(t)
			seedReported(t, q, "w", true)
			f.prs["w"] = merge.Candidate{Number: 5, State: "MERGED"}
			f.usage = &statusUsage{CostUSD: 1, Priced: true}
			f.closeRes = &closeResult{Name: "w", Closed: true, Workspace: closeWorkspaceClosed, Worktree: closeWorktreeKept, Note: note, ledgerFailed: true}
			d.tick(t.Context())
			r := rowNamed(t, q, "w")
			if r.State != worker.QueueReported || !strings.Contains(r.LastError, note) || r.CostUSD == nil || *r.CostUSD != 1 {
				t.Fatalf("row %s %q cost %v; want reported, the note as last_error, the price kept", r.State, r.LastError, r.CostUSD)
			}
			if ev := eventsOf(f, "w", drain.EventError); len(ev) != 1 || !strings.Contains(ev[0].Error, note) {
				t.Fatalf("events %+v; want one error carrying the note", ev)
			}
			f.closeRes = nil
			f.now = f.now.Add(drain.CloserReadEvery)
			d.tick(t.Context())
			if r := rowNamed(t, q, "w"); r.State != worker.QueueClosed {
				t.Fatalf("row %s after the ledger write works; want closed", r.State)
			}
		})
	}
}

// TestDrainCloserCloseNoteRecorded pins that a close that worked but carries
// a note (a recovery tag to check by hand) records it: a note event and the
// closed row's last_error.
func TestDrainCloserCloseNoteRecorded(t *testing.T) {
	f, d, q := newFakeDrain(t)
	seedReported(t, q, "w", true)
	f.prs["w"] = merge.Candidate{Number: 5, State: "MERGED"}
	const note = "a failed launch may have left a herdr workspace labeled fx-1"
	f.closeRes = &closeResult{Name: "w", Closed: true, Workspace: closeWorkspaceNone, Worktree: closeWorktreeRemoved, Forgotten: true, Note: note}
	d.tick(t.Context())
	r := rowNamed(t, q, "w")
	if r.State != worker.QueueClosed || !strings.Contains(r.LastError, "PR #5 merged") || !strings.Contains(r.LastError, note) {
		t.Fatalf("row %s %q; want closed naming the merge and the note", r.State, r.LastError)
	}
	if ev := eventsOf(f, "w", drain.EventNote); len(ev) != 1 || !strings.Contains(ev[0].Error, note) {
		t.Fatalf("notes %+v; want one carrying the close's note", ev)
	}
}

// TestDrainDailyPruneUnreadableDay pins that an unreadable drain-prune-day
// is one error event, the prune runs, and the day is rewritten with today
// first, so a restart that day does not prune again.
func TestDrainDailyPruneUnreadableDay(t *testing.T) {
	f, d, _ := newFakeDrain(t)
	f.herdrErr = errors.New("down")
	f.pruneDayErr = errors.New("drain-prune-day: not a day")
	d.tick(t.Context())
	if f.pruneRuns != 1 || f.pruneDay != "2026-10-07" {
		t.Fatalf("prune ran %d times, day %q; want once, rewritten with today", f.pruneRuns, f.pruneDay)
	}
	n := 0
	for _, e := range f.events {
		if e.Kind == drain.EventError && strings.Contains(e.Error, "not a day") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d error events for the unreadable day; want 1: %+v", n, f.events)
	}
	f.pruneDayErr = nil
	restarted := newDrainer(d.io, drainTestSession, func() bool { return false })
	restarted.tick(t.Context())
	if f.pruneRuns != 1 {
		t.Fatalf("a restart after the rewrite pruned again (%d runs)", f.pruneRuns)
	}
}
