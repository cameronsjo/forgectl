// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package runview

import (
	"slices"
	"strconv"
	"time"
)

// Fold reduces events into the run's state. It is pure: the same spec, defs,
// events and now give the same RunState, and nothing passed in is changed.
// defs nil means spec.Steps. now only decides Live.
func Fold(spec *Spec, defs []StepDef, events []Event, now time.Time) RunState {
	r := newReducer(spec, defs)
	for _, e := range events {
		r.apply(e)
	}
	return r.finish(now)
}

// reducer is the fold's working state: the RunState under construction plus
// the bookkeeping the transitions need and RunState does not show.
type reducer struct {
	spec   *Spec
	checks map[string][]int // event name -> indexes into state.Checks
	steps  map[string]int   // step id -> index into state.Steps
	state  RunState

	begun   bool      // a known step has started; restarts before that are the initial start
	ended   bool      // an end event arrived and no counted restart followed it
	endedAt time.Time // the time of that end event
	// restartsAtStart is len(state.Restarts) at each step's latest start.
	restartsAtStart []int
}

func newReducer(spec *Spec, defs []StepDef) *reducer {
	if spec == nil {
		spec = &Spec{}
	}
	if defs == nil {
		defs = spec.Steps
	}
	r := &reducer{
		spec:            spec,
		steps:           make(map[string]int, len(defs)),
		restartsAtStart: make([]int, len(defs)),
		state: RunState{
			Steps: make([]StepState, len(defs)),
			Edges: edges(defs),
		},
	}
	for i, d := range defs {
		r.steps[d.ID] = i
		r.state.Steps[i] = StepState{ID: d.ID, Note: d.Note, Status: StepPending}
	}
	for i, c := range spec.Checks {
		if r.checks == nil {
			r.checks = map[string][]int{}
		}
		r.checks[c.Event] = append(r.checks[c.Event], i)
		r.state.Checks = append(r.state.Checks, CheckState{ID: c.ID, Label: c.Label})
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

// apply folds one event. An event no action or check maps to changes only
// LastEvent.
func (r *reducer) apply(e Event) {
	if !e.Time.IsZero() {
		r.state.LastEvent = e.Time
	}
	for _, i := range r.checks[e.Name] {
		r.check(i, e)
	}
	action := r.spec.On[e.Name]
	if action != ActionStart {
		r.attempt(e)
	}
	switch action {
	case ActionStart:
		if i, ok := r.step(e); ok {
			r.start(i, e)
		}
	case ActionClose:
		if i, ok := r.step(e); ok && r.state.Steps[i].Status == StepRunning {
			r.state.Steps[i].Status = StepClosed
			r.state.Steps[i].End = e.Time
		}
	case ActionReject:
		// Reaching max_rejects is drawn, not enforced: only a fail event
		// fails the step.
		if i, ok := r.step(e); ok {
			r.state.Steps[i].Rejects++
		}
	case ActionFail:
		if i, ok := r.step(e); ok {
			r.state.Steps[i].Status = StepFailed
			r.state.Steps[i].End = e.Time
		}
	case ActionRestart:
		r.restart(e)
	case ActionEnd:
		r.end(e)
	}
}

// step resolves the event's step, counting an id the defs do not have
// (an empty one included) in UnknownSteps.
func (r *reducer) step(e Event) (int, bool) {
	i, ok := r.steps[e.Step]
	if !ok {
		r.state.UnknownSteps++
	}
	return i, ok
}

// eventAttempt reads the attempt field of e, when the spec names one and the
// value is a positive integer.
func (r *reducer) eventAttempt(e Event) (int, bool) {
	if r.spec.Log.Attempt == "" {
		return 0, false
	}
	v, ok := e.field(r.spec.Log.Attempt)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	return n, err == nil && n > 0
}

// attempt raises a started step's attempt to the one a non-start event
// carries: a worker that retries a step after a reject sends no new start,
// so the attempt shows up only on the step's other events. The highest seen
// is kept. A pending step is left alone, so its first start still counts 1.
// An unknown step is not counted here; the action, if any, counts it.
func (r *reducer) attempt(e Event) {
	i, ok := r.steps[e.Step]
	if !ok || r.state.Steps[i].Status == StepPending {
		return
	}
	if n, ok := r.eventAttempt(e); ok {
		r.state.Steps[i].Attempt = max(r.state.Steps[i].Attempt, n)
	}
}

func (r *reducer) start(i int, e Event) {
	s := &r.state.Steps[i]
	attempt := s.Attempt + 1
	if n, ok := r.eventAttempt(e); ok {
		attempt = n
	}
	// Resumed compares against this step's previous start, so a step's first
	// start is never resumed.
	s.Resumed = s.Attempt > 0 && len(r.state.Restarts) > r.restartsAtStart[i]
	r.restartsAtStart[i] = len(r.state.Restarts)
	s.Status = StepRunning
	s.Attempt = attempt
	s.Start = e.Time
	s.End = time.Time{}
	r.begun = true
}

// restart counts a restart and marks every running step resumed: a worker
// may pick a step up again without sending its start.
func (r *reducer) restart(e Event) {
	if !r.begun {
		return // the initial start, not a restart
	}
	for i := range r.state.Steps {
		if r.state.Steps[i].Status == StepRunning {
			r.state.Steps[i].Resumed = true
		}
	}
	r.state.Restarts = append(r.state.Restarts, e.Time)
	r.state.Exit = nil
	r.state.ExitMeaning = ""
	r.ended = false
}

// end marks the run ended whether or not its exit is usable; a missing,
// non-integer or out-of-range exit is counted and the previous exit kept.
func (r *reducer) end(e Event) {
	r.ended, r.endedAt = true, e.Time
	v, ok := e.field(r.spec.End.ExitField)
	if !ok {
		r.state.BadExits++
		return
	}
	code, ok := exitCode(v)
	if !ok {
		r.state.BadExits++
		return
	}
	r.state.Exit = &code
	r.state.ExitMeaning = r.spec.Exits[code]
}

// exitCode reads an exit from an event field: an integer 0–255.
func exitCode(v string) (int, bool) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 255 {
		return 0, false
	}
	return n, true
}

// check records a check's outcome. A failure only colors the check; the run's
// exit still comes from its end event.
func (r *reducer) check(i int, e Event) {
	v, _ := e.field(r.spec.Checks[i].Field)
	n, err := strconv.ParseInt(v, 10, 64)
	r.state.Checks[i].Seen = true
	r.state.Checks[i].Passed = err == nil && n == 0
}

// finish returns the state as of now. The result shares no memory with the
// reducer, so a caller may keep or edit it while folding continues.
//
// A step still running when the run ended reads interrupted, ended at the
// end event. Only the result says so: the reducer keeps the step running,
// so a later restart resumes it and its own close still lands.
func (r *reducer) finish(now time.Time) RunState {
	s := r.clone().state
	if r.ended {
		for i := range s.Steps {
			if s.Steps[i].Status == StepRunning {
				s.Steps[i].Status, s.Steps[i].End = StepInterrupted, r.endedAt
			}
		}
	}
	switch {
	case r.ended:
		s.Live = LiveEnded
	case r.spec.Live.StaleAfter == 0:
		s.Live = LiveLive
	case !s.LastEvent.IsZero() && now.Sub(s.LastEvent) <= r.spec.Live.StaleAfter:
		s.Live = LiveLive
	default:
		s.Live = LiveStale
	}
	return s
}

// clone deep-copies the reducer's mutable state. spec, steps and checks are
// read-only after newReducer and are shared.
func (r *reducer) clone() *reducer {
	c := *r
	c.state.Steps = slices.Clone(r.state.Steps)
	c.state.Edges = slices.Clone(r.state.Edges)
	c.state.Checks = slices.Clone(r.state.Checks)
	c.state.Restarts = slices.Clone(r.state.Restarts)
	if r.state.Exit != nil {
		code := *r.state.Exit
		c.state.Exit = &code
	}
	c.restartsAtStart = slices.Clone(r.restartsAtStart)
	return &c
}
