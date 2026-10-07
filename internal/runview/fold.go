// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package runview

import (
	"maps"
	"slices"
	"strconv"
	"time"
)

// Action is what an event does to a run.
type Action string

const (
	ActionStart Action = "start"
	ActionClose Action = "close"
	ActionFail  Action = "fail"
	ActionSkip  Action = "skip"
	ActionEnd   Action = "end"
	// ActionLost ends a run whose owner is gone with no end event: a step
	// still running reads interrupted, and the run reads lost.
	ActionLost Action = "lost"
)

// Spec says how a run's events fold: which event names start, close or fail
// a step or end the run, and which field of the end event holds the exit.
// The desk's is built in code (DeskSpec); a lens builds one from its file
// (Lens.Spec, ADR-0014). A run read with an empty Spec shows its events and
// no step state.
type Spec struct {
	On        map[string]Action // event name -> action
	ExitField string
	// ActionField, when set, names an event field holding the event's
	// action; it decides over On. A lens writes it at ingest, where its
	// rules matched the line.
	ActionField string
	// ExitOptional reads an end event with no exit as an end with an
	// unknown exit, not a bad one: a lens's end rule may match a line that
	// carries no exit ("shutting down").
	ExitOptional bool
	// Discover adds a step the first time an event names it, in the order
	// they appear, with no edges. Without it, a step the defs do not have
	// is counted in UnknownSteps.
	Discover bool
}

// Fold reduces events into the run's state. It is pure: the same spec, defs
// and events give the same RunState, and nothing passed in is changed.
func Fold(spec *Spec, defs []StepDef, events []Event) RunState {
	r := newReducer(spec, defs)
	for _, e := range events {
		r.apply(e)
	}
	return r.finish()
}

// reducer is the fold's working state: the RunState under construction plus
// the bookkeeping the transitions need and RunState does not show.
type reducer struct {
	spec  *Spec
	steps map[string]int // step id -> index into state.Steps
	state RunState

	ended   bool      // an end event arrived
	lost    bool      // the run ended as lost (ActionLost), not with an end event
	endedAt time.Time // the time of that end event
}

func newReducer(spec *Spec, defs []StepDef) *reducer {
	if spec == nil {
		spec = &Spec{}
	}
	r := &reducer{
		spec:  spec,
		steps: make(map[string]int, len(defs)),
		state: RunState{
			Steps: make([]StepState, len(defs)),
			Edges: edges(defs),
		},
	}
	for i, d := range defs {
		r.steps[d.ID] = i
		r.state.Steps[i] = StepState{ID: d.ID, Note: d.Note, Status: StepPending}
	}
	return r
}

// edges draws the defs' After lists when any def has one, else the defs in
// order as a chain.
func edges(defs []StepDef) [][2]string {
	var out [][2]string
	if slices.ContainsFunc(defs, func(d StepDef) bool { return len(d.After) > 0 }) {
		for _, d := range defs {
			for _, a := range d.After {
				out = append(out, [2]string{a, d.ID})
			}
		}
		return out
	}
	for i := 1; i < len(defs); i++ {
		out = append(out, [2]string{defs[i-1].ID, defs[i].ID})
	}
	return out
}

// apply folds one event. An event no action maps to changes only LastEvent.
func (r *reducer) apply(e Event) {
	if e.Time.After(r.state.LastEvent) {
		r.state.LastEvent = e.Time // the newest, so an out-of-order log still sorts by its latest
	}
	action := r.spec.On[e.Name]
	if r.spec.ActionField != "" {
		if v, ok := e.field(r.spec.ActionField); ok {
			action = Action(v)
		}
	}
	switch action {
	case ActionStart:
		if r.ended && r.spec.ActionField != "" {
			// A lens's log went on past an end: the app started again, so
			// the run is live again, its new exit not known yet.
			r.ended, r.lost, r.state.Exit = false, false, nil
		}
		if i, ok := r.step(e); ok {
			s := &r.state.Steps[i]
			s.Status, s.Start, s.End = StepRunning, e.Time, time.Time{}
		}
	case ActionClose:
		if i, ok := r.step(e); ok && r.state.Steps[i].Status == StepRunning {
			r.state.Steps[i].Status = StepClosed
			r.state.Steps[i].End = e.Time
		}
	case ActionFail:
		if i, ok := r.step(e); ok {
			r.state.Steps[i].Status = StepFailed
			r.state.Steps[i].End = e.Time
		}
	case ActionSkip:
		// Only a step that never started is skipped; a runner never skips a
		// step it already ran.
		if i, ok := r.step(e); ok && r.state.Steps[i].Status == StepPending {
			r.state.Steps[i].Status = StepSkipped
		}
	case ActionEnd:
		r.end(e)
	case ActionLost:
		if !r.ended {
			r.ended, r.lost, r.endedAt = true, true, e.Time
		}
	}
}

// step resolves the event's step, counting an id the defs do not have (an
// empty one included) in UnknownSteps. Under Discover a named step the defs
// do not have is added, pending, after the others.
func (r *reducer) step(e Event) (int, bool) {
	i, ok := r.steps[e.Step]
	if !ok && r.spec.Discover && e.Step != "" {
		i, ok = len(r.state.Steps), true
		r.steps[e.Step] = i
		r.state.Steps = append(r.state.Steps, StepState{ID: e.Step, Status: StepPending})
	}
	if !ok {
		r.state.UnknownSteps++
	}
	return i, ok
}

// end marks the run ended whether or not its exit is usable; a missing,
// non-integer or out-of-range exit is counted and the previous exit kept.
func (r *reducer) end(e Event) {
	// A RUN-END after a RUN-LOST wins: the owner was slow, not gone.
	r.ended, r.lost, r.endedAt = true, false, e.Time
	v, ok := e.field(r.spec.ExitField)
	if !ok {
		if !r.spec.ExitOptional {
			r.state.BadExits++
		}
		return
	}
	code, ok := exitCode(v)
	if !ok {
		r.state.BadExits++
		return
	}
	r.state.Exit = &code
}

// exitCode reads an exit from an event field: an integer 0–255.
func exitCode(v string) (int, bool) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 255 {
		return 0, false
	}
	return n, true
}

// finish returns the state so far. The result shares no memory with the
// reducer, so a caller may keep or edit it while folding continues.
//
// A step still running when the run ended reads interrupted, ended at the
// end event. Only the result says so: the reducer keeps the step running, so
// a later close still lands.
func (r *reducer) finish() RunState {
	s := r.clone().state
	s.Live = LiveLive
	if r.ended {
		s.Live = LiveEnded
		if r.lost {
			s.Live = LiveLost
		}
		for i := range s.Steps {
			if s.Steps[i].Status == StepRunning {
				s.Steps[i].Status, s.Steps[i].End = StepInterrupted, r.endedAt
			}
		}
	}
	return s
}

// clone deep-copies the reducer's mutable state. spec is read-only and
// shared; steps is too, unless the spec discovers steps, which adds to it.
func (r *reducer) clone() *reducer {
	c := *r
	if r.spec.Discover {
		c.steps = maps.Clone(r.steps)
	}
	c.state.Steps = slices.Clone(r.state.Steps)
	c.state.Edges = slices.Clone(r.state.Edges)
	if r.state.Exit != nil {
		code := *r.state.Exit
		c.state.Exit = &code
	}
	return &c
}
