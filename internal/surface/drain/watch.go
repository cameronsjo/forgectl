package drain

import (
	"fmt"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/herdr/ready"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// ProbeState is how one screen read of a worker went.
type ProbeState int

const (
	// ProbeNone: the row was not read (no live ledger row to read).
	ProbeNone ProbeState = iota
	// ProbeRead: the pane was read and evaluated.
	ProbeRead
	// ProbeGone: herdr proved the workspace absent.
	ProbeGone
	// ProbeUnreadable: herdr could not be read. Never a verdict about the
	// worker.
	ProbeUnreadable
)

// Probe is one screen read of a live worker, through the readiness
// predicates, with no waiting.
type Probe struct {
	State   ProbeState
	Verdict ready.Verdict
	// Report is whether the screen shows the REPORT line for the worker's
	// marker.
	Report bool
	// Err says why the read failed.
	Err string
}

// NeedsProbe reports whether the tick should read q's worker: a launched or
// needs-you row whose own ledger row is launched.
func NeedsProbe(q worker.QueueRow, l Ledger) bool {
	return (q.State == worker.QueueLaunched || q.State == worker.QueueNeedsYou) &&
		l.State == LedgerOurs && l.Row.Stage == worker.StageLaunched
}

// Memo is what the drain remembers about one row between ticks. It lives in
// the drain process only; a restart forgets it.
type Memo struct {
	// ReadySince is when the worker was first seen at its prompt in the
	// current run of ready verdicts, or zero.
	ReadySince time.Time
	// Unreadable is whether the last read of the row was unreadable, so an
	// unreadable row is recorded once per change, not once per tick.
	Unreadable bool
}

// Watch decides what one launched or needs-you row becomes, from its ledger
// row and one read of its pane:
//
//   - its ledger row closed or gone: closed. Its ledger row failed, or the
//     workspace gone: failed.
//   - the REPORT line for its marker on screen: reported.
//   - a blocking screen: needs-you, notified once on entry.
//   - any verdict but blocked while needs-you for a blocking screen: launched.
//   - at its prompt with no report for idle: needs-you (ReasonIdle). A
//     needs-you row for that reason goes back to launched once the worker
//     is working again (not-ready).
//   - unreadable (ledger or pane): unchanged, with one Note when it starts.
func Watch(q worker.QueueRow, l Ledger, p Probe, m Memo, now time.Time, idle time.Duration) (Change, Memo) {
	c := change(q)
	if q.State != worker.QueueLaunched && q.State != worker.QueueNeedsYou {
		return c, m
	}
	switch l.State {
	case LedgerUnreadable:
		return unreadable(c, m, "the ledger could not be read: "+l.Err)
	case LedgerAbsent:
		return c.to(worker.QueueClosed, "the worker's ledger row is gone (surface close removed it)"), Memo{}
	case LedgerOther:
		return c.to(worker.QueueClosed, fmt.Sprintf("the ledger row named %q is another launch's: expected launch_id %s, saw %q",
			q.Name, q.LaunchID, l.Row.LaunchID)), Memo{}
	}
	switch l.Row.Stage {
	case worker.StageClosed:
		return c.to(worker.QueueClosed, ""), Memo{}
	case worker.StageFailed:
		return c.to(worker.QueueFailed, "the worker's ledger row failed: "+l.Row.Failure), Memo{}
	case worker.StageLaunched:
	default:
		// A launched queue row whose ledger row is not: nothing to read.
		return c, m
	}
	switch p.State {
	case ProbeGone:
		return c.to(worker.QueueFailed, "the worker's herdr workspace is gone"), Memo{}
	case ProbeUnreadable:
		return unreadable(c, m, "the worker's pane could not be read: "+p.Err)
	case ProbeNone:
		return c, m
	}
	if m.Unreadable {
		m.Unreadable = false
		c.Note = "readable again"
	}
	if p.Report {
		return c.to(worker.QueueReported, ""), Memo{}
	}
	blockedReason := q.State == worker.QueueNeedsYou && strings.HasPrefix(q.LastError, reasonBlockedPrefix)
	switch p.Verdict.State {
	case ready.StateBlocked:
		m.ReadySince = time.Time{}
		why := reasonBlockedPrefix + blockingName(p.Verdict)
		switch {
		case q.State != worker.QueueNeedsYou:
			c = c.to(worker.QueueNeedsYou, why)
			c.Notify = true
		case q.LastError != why:
			// Still waiting on the operator, for another screen now; the
			// row did not leave needs-you, so no second notification.
			c.Error, c.SetError = why, true
		}
		return c, m
	case ready.StateReady:
		if m.ReadySince.IsZero() || blockedReason {
			m.ReadySince = now
		}
		switch {
		case blockedReason:
			return c.to(worker.QueueLaunched, ""), m
		case q.State == worker.QueueLaunched && now.Sub(m.ReadySince) >= idle:
			c = c.to(worker.QueueNeedsYou, ReasonIdle)
			c.Notify = true
			return c, m
		}
		return c, m
	default:
		m.ReadySince = time.Time{}
		if q.State == worker.QueueNeedsYou {
			return c.to(worker.QueueLaunched, ""), m
		}
		return c, m
	}
}

func unreadable(c Change, m Memo, why string) (Change, Memo) {
	if !m.Unreadable {
		m.Unreadable = true
		c.Note = why
	}
	return c, m
}

func blockingName(v ready.Verdict) string {
	if v.Blocking != "" {
		return v.Blocking
	}
	if v.Reason != "" {
		return v.Reason
	}
	return "a blocking screen"
}
