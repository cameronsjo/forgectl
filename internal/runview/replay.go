// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package runview

// checkpointEvery is how many events apart the Folder keeps a checkpoint, so
// At replays at most this many events.
const checkpointEvery = 1000

// Folder keeps a run's events and serves the state after any prefix of them.
// At(i) equals Fold(spec, defs, events[:i]); it replays from the nearest
// checkpoint at or before i instead of from the start.
type Folder struct {
	events []Event
	tip    *reducer   // the fold of every event so far
	checks []*reducer // checks[k] is the fold of events[:k*checkpointEvery]
}

// NewFolder starts an empty replay.
func NewFolder(spec *Spec, defs []StepDef) *Folder {
	tip := newReducer(spec, defs)
	return &Folder{tip: tip, checks: []*reducer{tip.clone()}}
}

// Append adds events in order, taking a checkpoint at every multiple of
// checkpointEvery.
func (f *Folder) Append(evs ...Event) {
	for _, e := range evs {
		f.events = append(f.events, e)
		f.tip.apply(e)
		if len(f.events)%checkpointEvery == 0 {
			f.checks = append(f.checks, f.tip.clone())
		}
	}
}

// At returns the state after the first i events. i is clamped to [0, Len()].
func (f *Folder) At(i int) RunState {
	i = max(0, min(i, len(f.events)))
	k := i / checkpointEvery
	r := f.checks[k].clone()
	for _, e := range f.events[k*checkpointEvery : i] {
		r.apply(e)
	}
	return r.finish()
}

// Events is the events appended so far. The caller must not change them.
func (f *Folder) Events() []Event { return f.events }

// Len is the number of events appended.
func (f *Folder) Len() int { return len(f.events) }
