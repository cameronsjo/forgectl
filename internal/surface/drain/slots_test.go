package drain

import (
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

func TestDecideSlots(t *testing.T) {
	cases := map[string]struct {
		check    SlotsCheck
		prev     string
		hold     bool
		cond     string
		emit     bool
		kind     string
		errHas   string
		reasonIn string
	}{
		"free":                      {check: SlotsCheck{Exit: 0}},
		"free after a hold":         {check: SlotsCheck{Exit: 0}, prev: "held"},
		"held, first time":          {check: SlotsCheck{Exit: 1, Reason: "3 of 3 sessions live"}, hold: true, cond: "held", emit: true, kind: EventSlotsHeld, errHas: "check 1 exited 1: 3 of 3 sessions live", reasonIn: "3 of 3 sessions live"},
		"held again":                {check: SlotsCheck{Exit: 1, Reason: "3 of 3 sessions live"}, prev: "held", hold: true, cond: "held", reasonIn: "3 of 3"},
		"names the asked count":     {check: SlotsCheck{Need: 3, Exit: 1}, hold: true, cond: "held", emit: true, kind: EventSlotsHeld, errHas: "claude-slots check 3 exited 1"},
		"held with no output":       {check: SlotsCheck{Exit: 1}, hold: true, cond: "held", emit: true, kind: EventSlotsHeld, errHas: "no reason given"},
		"odd exit holds":            {check: SlotsCheck{Exit: 2, Reason: "usage"}, hold: true, cond: "exit 2", emit: true, kind: EventSlotsHeld, errHas: "exited 2", reasonIn: "exited 2"},
		"odd exit again is quiet":   {check: SlotsCheck{Exit: 2}, prev: "exit 2", hold: true, cond: "exit 2"},
		"another odd exit is new":   {check: SlotsCheck{Exit: 3}, prev: "exit 2", hold: true, cond: "exit 3", emit: true, kind: EventSlotsHeld, errHas: "exited 3"},
		"signal (no exit code)":     {check: SlotsCheck{Exit: -1}, hold: true, cond: "exit -1", emit: true, kind: EventSlotsHeld},
		"timeout holds":             {check: SlotsCheck{Exit: -1, TimedOut: true}, hold: true, cond: "timeout", emit: true, kind: EventSlotsHeld, errHas: "check 1 did not finish in time; holding the row", reasonIn: "did not finish in time"},
		"timeout again is quiet":    {check: SlotsCheck{Exit: -1, TimedOut: true}, prev: "timeout", hold: true, cond: "timeout"},
		"run failure holds":         {check: SlotsCheck{Exit: -1, Err: "permission denied"}, hold: true, cond: "run failed", emit: true, kind: EventSlotsHeld, errHas: "permission denied"},
		"hold after timeout is new": {check: SlotsCheck{Exit: 1}, prev: "timeout", hold: true, cond: "held", emit: true, kind: EventSlotsHeld},
		"timeout after hold is new": {check: SlotsCheck{TimedOut: true, Exit: -1}, prev: "held", hold: true, cond: "timeout", emit: true, kind: EventSlotsHeld},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := DecideSlots(c.check, c.prev)
			if d.Hold != c.hold || d.Cond != c.cond || d.Emit != c.emit {
				t.Fatalf("DecideSlots = %+v; want hold %v cond %q emit %v", d, c.hold, c.cond, c.emit)
			}
			if c.emit && (d.Event.Kind != c.kind || !strings.Contains(d.Event.Error, c.errHas)) {
				t.Fatalf("event %+v; want kind %s naming %q", d.Event, c.kind, c.errHas)
			}
			if !strings.Contains(d.Reason, c.reasonIn) {
				t.Fatalf("reason %q does not name %q", d.Reason, c.reasonIn)
			}
		})
	}
}

func TestUnclaim(t *testing.T) {
	q := qrow("w", "/r", worker.QueueClaimed, t0)
	q.Attempts = 2
	c := Unclaim(q, "waiting for a claude session slot: full")
	r := q
	c.Apply(&r)
	if r.State != worker.QueueQueued || r.LaunchID != "" || r.Attempts != 2 || r.LastError != "waiting for a claude session slot: full" {
		t.Fatalf("unclaimed row %+v; want queued, no launch id, attempts unchanged", r)
	}
}
