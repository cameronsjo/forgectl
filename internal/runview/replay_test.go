// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package runview

import (
	"math/rand/v2"
	"reflect"
	"strconv"
	"testing"
)

// fixtureEvents generates n events in the desk vocabulary from a fixed seed:
// every action, unmapped and unknown-step events, and bad exits.
func fixtureEvents(n int) []Event {
	r := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // G404: deterministic test fixture, not crypto

	// deploy is not a defined step.
	steps := []string{"fetch", "build", "build", "deploy"}
	rcs := []string{"0", "1", "3", "x", "-1"}
	events := make([]Event, 0, n)
	for i := range n {
		e := Event{Seq: i, Time: at(i)}
		switch k := r.IntN(20); {
		case k < 6:
			e.Name, e.Step = "STEP-START", steps[r.IntN(len(steps))]
		case k < 10:
			e.Name, e.Step = "STEP-END", steps[r.IntN(len(steps))]
		case k < 13:
			e.Name, e.Step = "STEP-FAIL", steps[r.IntN(len(steps))]
		case k < 16:
			e.Name = "RUN-END"
			e.Fields = []Field{{Key: "rc", Value: rcs[r.IntN(len(rcs))]}}
		case k < 18:
			e.Name, e.Step = "STEP-SKIP", steps[r.IntN(len(steps))]
		default:
			e.Name, e.Step = "STEP-WARN", steps[r.IntN(len(steps))]
			e.Fields = []Field{{Key: "n", Value: strconv.Itoa(i)}}
		}
		events = append(events, e)
	}
	return events
}

// TestFixtureCrossesCheckpointsWithEveryAction guards the fixture itself:
// each checkpoint block must hold every action, or the equivalence test below
// would not exercise state carried across a checkpoint.
func TestFixtureCrossesCheckpointsWithEveryAction(t *testing.T) {
	events := fixtureEvents(2500)
	for block := range 3 {
		seen := map[string]bool{}
		for _, e := range events[block*checkpointEvery : min(len(events), (block+1)*checkpointEvery)] {
			seen[e.Name] = true
		}
		for _, name := range []string{"STEP-START", "STEP-END", "STEP-FAIL", "RUN-END", "STEP-SKIP", "STEP-WARN"} {
			if !seen[name] {
				t.Errorf("block %d has no %s event", block, name)
			}
		}
	}
}

func TestFolderAtEqualsFoldForEveryPrefix(t *testing.T) {
	events := fixtureEvents(2500)
	for _, defs := range [][]StepDef{nil, {{ID: "fetch"}, {ID: "build", After: []string{"fetch"}}}} {
		f := NewFolder(DeskSpec(), defs)
		// Uneven chunks, so checkpoints are taken inside an Append as well as
		// at its edges.
		for lo, chunk := 0, 1; lo < len(events); lo, chunk = lo+chunk, chunk*3 {
			f.Append(events[lo:min(len(events), lo+chunk)]...)
		}
		if f.Len() != len(events) {
			t.Fatalf("Len = %d, want %d", f.Len(), len(events))
		}
		for i := 0; i <= len(events); i++ {
			got, want := f.At(i), Fold(DeskSpec(), defs, events[:i])
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("defs %v: At(%d) != Fold(events[:%d]):\n got %+v\nwant %+v", defs, i, i, got, want)
			}
		}
	}
}

func TestFolderAtClampsOutOfRange(t *testing.T) {
	events := fixtureEvents(1200)
	f := NewFolder(DeskSpec(), nil)
	f.Append(events...)
	if got, want := f.At(-5), Fold(DeskSpec(), nil, nil); !reflect.DeepEqual(got, want) {
		t.Errorf("At(-5) = %+v, want the empty fold", got)
	}
	if got, want := f.At(5000), Fold(DeskSpec(), nil, events); !reflect.DeepEqual(got, want) {
		t.Errorf("At(5000) = %+v, want the full fold", got)
	}
}

func TestFolderEventsAreThoseAppended(t *testing.T) {
	events := fixtureEvents(1500)
	f := NewFolder(DeskSpec(), nil)
	if len(f.Events()) != 0 || f.Len() != 0 {
		t.Fatalf("a new Folder holds %d events", f.Len())
	}
	f.Append(events[:700]...)
	f.Append(events[700:]...)
	if !reflect.DeepEqual(f.Events(), events) {
		t.Errorf("Events() differs from what was appended")
	}
}

func TestFolderStateIsNotSharedWithCheckpoints(t *testing.T) {
	events := fixtureEvents(2500)
	defs := []StepDef{{ID: "fetch"}, {ID: "build"}}
	f := NewFolder(DeskSpec(), defs)
	f.Append(events[:1500]...)
	s := f.At(checkpointEvery)
	for i := range s.Steps {
		s.Steps[i].Status = StepFailed
	}
	if s.Exit != nil {
		*s.Exit = 200
	}
	for i := range s.Edges {
		s.Edges[i] = [2]string{"x", "y"}
	}
	f.Append(events[1500:]...)
	for _, i := range []int{checkpointEvery, checkpointEvery + 1, 2500} {
		if got, want := f.At(i), Fold(DeskSpec(), defs, events[:i]); !reflect.DeepEqual(got, want) {
			t.Errorf("At(%d) after editing a returned state differs from Fold", i)
		}
	}
}

func TestFolderAtAtACheckpointBoundary(t *testing.T) {
	events := fixtureEvents(2001)
	f := NewFolder(DeskSpec(), fetchBuild)
	f.Append(events...)
	for _, i := range []int{checkpointEvery - 1, checkpointEvery, checkpointEvery + 1, 2 * checkpointEvery, 2*checkpointEvery + 1} {
		if got, want := f.At(i), Fold(DeskSpec(), fetchBuild, events[:i]); !reflect.DeepEqual(got, want) {
			t.Errorf("At(%d) != Fold", i)
		}
	}
}

// The skip shows at the event that made it: not before, and not lost after
// a checkpoint.
func TestFolderShowsASkipAtTheRightEvent(t *testing.T) {
	defs := []StepDef{{ID: "fetch"}, {ID: "build", After: []string{"fetch"}}}
	// Filler that touches only fetch, so build is skipped at an index past a
	// checkpoint.
	var events []Event
	for i := range checkpointEvery + 5 {
		events = append(events, Event{Seq: i, Time: at(i), Name: "STEP-WARN", Step: "fetch"})
	}
	skipAt := len(events)
	events = append(events, ev(skipAt, "STEP-SKIP", "build"), ev(skipAt+1, "RUN-END", "", "rc", "1"))
	f := NewFolder(DeskSpec(), defs)
	f.Append(events...)

	for _, c := range []struct {
		i    int
		want StepStatus
	}{{0, StepPending}, {checkpointEvery, StepPending}, {skipAt, StepPending}, {skipAt + 1, StepSkipped}, {skipAt + 2, StepSkipped}} {
		if got := step(t, f.At(c.i), "build").Status; got != c.want {
			t.Errorf("At(%d): build = %s, want %s", c.i, got, c.want)
		}
		if got, want := f.At(c.i), Fold(DeskSpec(), defs, events[:c.i]); !reflect.DeepEqual(got, want) {
			t.Errorf("At(%d) != Fold", c.i)
		}
	}
}
