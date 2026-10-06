// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

// Package runview models a multi-step run (its steps, the edges between them,
// its exit) and folds the run's events into its state. Fold is pure, so the
// state after event i is Fold over the first i events; a Folder serves that
// from checkpoints, which is what replay reads.
//
// Two sources produce runs: the desk (its items' event files, batch manifests
// and status files) and a JSONL log a person names. Both produce the same
// Event and read the same RunState. The design is ADR-0013.
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
	// StepSkipped is a step the runner skipped without starting it (a
	// dependency failed, or fail-fast stopped the run).
	StepSkipped StepStatus = "skipped"
	// StepInterrupted is a step that was still running when the run ended.
	StepInterrupted StepStatus = "interrupted"
)

// StepState is a step after the events so far.
type StepState struct {
	ID, Note   string
	Status     StepStatus
	Start, End time.Time
}

// LiveState says whether a run is still going. Fold sets ended or live; the
// desk source sets the others from the desk's own state.
type LiveState string

const (
	LiveLive    LiveState = "live"
	LiveEnded   LiveState = "ended"
	LiveWaiting LiveState = "waiting"
	LiveRunning LiveState = "running"
	LiveLost    LiveState = "lost"
	LiveSkipped LiveState = "skipped"
	// LiveUnknown is a JSONL log's state: with no step model, nothing in the
	// log says whether its run is over.
	LiveUnknown LiveState = "unknown"
)

// RunState is a run after the events so far.
type RunState struct {
	Steps []StepState
	Edges [][2]string // from, to

	Exit *int

	Live      LiveState
	LastEvent time.Time

	// UnknownSteps counts step events naming a step not in the defs.
	// BadExits counts end events whose exit is not an integer 0–255.
	UnknownSteps, BadExits int
}
