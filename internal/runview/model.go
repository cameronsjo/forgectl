// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

// Package runview models a run (steps, a reject gate, checks, an exit) and
// folds a run's events into its state. Fold is pure, so replaying to event i
// is Fold over the first i events; a Folder serves that from checkpoints.
// Every source (a spec-described log, a desk batch) produces the same Event
// and reads the same RunState.
package runview

import "time"

// Field is one event field, kept in the order the source read it.
type Field struct{ Key, Value string }

// Event is one line of a run's log. Time is zero for desk events, which carry
// no timestamp of their own. Step is empty when the event names no step.
type Event struct {
	Seq    int
	Time   time.Time
	Name   string
	Step   string
	Fields []Field
}

// field returns the value of the first field named key.
func (e Event) field(key string) (string, bool) {
	for _, f := range e.Fields {
		if f.Key == key {
			return f.Value, true
		}
	}
	return "", false
}

// StepDef is a step a run is expected to have. After names the steps it
// waits on; when no def in a run has After, the steps form a chain in order.
type StepDef struct {
	ID, Note string
	After    []string
}

// StepStatus is where a step is.
type StepStatus string

const (
	StepPending StepStatus = "pending"
	StepRunning StepStatus = "running"
	StepClosed  StepStatus = "closed"
	StepFailed  StepStatus = "failed"
	// StepInterrupted is a step that was still running when the run ended.
	StepInterrupted StepStatus = "interrupted"
)

// StepState is a step after the events so far. Resumed reports that a
// restart happened while the step was running, or between its latest start
// and the one before it. Attempt is the highest attempt any of the step's
// events carried, or its count of starts.
type StepState struct {
	ID, Note   string
	Status     StepStatus
	Attempt    int
	Rejects    int
	Start, End time.Time
	Resumed    bool
}

// CheckState is a check after the events so far. Seen is false until its
// event arrives.
type CheckState struct {
	ID, Label    string
	Seen, Passed bool
}

// LiveState says whether a run is still going. Fold sets ended, live or
// stale; the desk source sets the others from the desk's own state.
type LiveState string

const (
	LiveLive    LiveState = "live"
	LiveEnded   LiveState = "ended"
	LiveStale   LiveState = "stale"
	LiveWaiting LiveState = "waiting"
	LiveRunning LiveState = "running"
	LiveLost    LiveState = "lost"
	LiveSkipped LiveState = "skipped"
)

// RunState is a run after the events so far.
type RunState struct {
	Steps  []StepState
	Edges  [][2]string // from, to
	Checks []CheckState

	Exit        *int
	ExitMeaning string
	Restarts    []time.Time

	Live      LiveState
	LastEvent time.Time

	// UnknownSteps counts step events naming a step not in the defs.
	// BadExits counts end events whose exit is not an integer 0–255.
	UnknownSteps, BadExits int
}
