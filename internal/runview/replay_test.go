// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package runview

import (
	"math/rand/v2"
	"reflect"
	"strconv"
	"testing"
)

// fixtureEvents generates n events in the example vocabulary from a fixed
// seed: every action, unmapped and unknown-step events, and bad exits.
func fixtureEvents(n int) []Event {
	r := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // G404: deterministic test fixture, not crypto

	// deploy is not a defined step.
	steps := []string{"fetch", "build", "build", "deploy"}
	rcs := []string{"0", "1", "3", "x", "-1"}
	events := make([]Event, 0, n)
	for i := range n {
		e := Event{Seq: i, Time: at(i)}
		switch k := r.IntN(20); {
		case k < 5:
			e.Name, e.Step = "stage_started", steps[r.IntN(len(steps))]
			if r.IntN(3) == 0 {
				e.Fields = []Field{{Key: "try", Value: strconv.Itoa(1 + r.IntN(4))}}
			}
		case k < 8:
			e.Name, e.Step = "stage_done", steps[r.IntN(len(steps))]
		case k < 11:
			e.Name, e.Step = "stage_refused", steps[r.IntN(len(steps))]
		case k < 12:
			e.Name, e.Step = "stage_gave_up", steps[r.IntN(len(steps))]
		case k < 14:
			e.Name = "worker_boot"
		case k < 16:
			e.Name = "run_finished"
			e.Fields = []Field{{Key: "rc", Value: rcs[r.IntN(len(rcs))]}}
		case k < 18:
			e.Name = "audit_result"
			e.Fields = []Field{{Key: "rc", Value: rcs[r.IntN(len(rcs))]}}
		default:
			e.Name, e.Step = "worker_note", steps[r.IntN(len(steps))]
		}
		events = append(events, e)
	}
	return events
}

// TestFixtureCrossesCheckpointsWithEveryAction guards the fixture itself:
// each checkpoint block must hold restarts and ends, or the equivalence test
// below would not exercise state carried across a checkpoint.
func TestFixtureCrossesCheckpointsWithEveryAction(t *testing.T) {
	events := fixtureEvents(2500)
	for block := 0; block < 3; block++ {
		seen := map[string]bool{}
		for _, e := range events[block*checkpointEvery : min(len(events), (block+1)*checkpointEvery)] {
			seen[e.Name] = true
		}
		for _, name := range []string{"stage_started", "stage_done", "stage_refused", "stage_gave_up", "worker_boot", "run_finished", "audit_result", "worker_note"} {
			if !seen[name] {
				t.Errorf("block %d has no %s event", block, name)
			}
		}
	}
}

func TestFolderAtEqualsFoldForEveryPrefix(t *testing.T) {
	spec := loadExample(t)
	events := fixtureEvents(2500)
	now := at(2400)
	for _, defs := range [][]StepDef{nil, {{ID: "fetch"}, {ID: "build", After: []string{"fetch"}}}} {
		f := NewFolder(spec, defs)
		// Uneven chunks, so checkpoints are taken inside an Append as well as
		// at its edges.
		for lo, chunk := 0, 1; lo < len(events); lo, chunk = lo+chunk, chunk*3 {
			f.Append(events[lo:min(len(events), lo+chunk)]...)
		}
		if f.Len() != len(events) {
			t.Fatalf("Len = %d, want %d", f.Len(), len(events))
		}
		for i := 0; i <= len(events); i++ {
			got, want := f.At(i, now), Fold(spec, defs, events[:i], now)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("defs %v: At(%d) != Fold(events[:%d]):\n got %+v\nwant %+v", defs, i, i, got, want)
			}
		}
	}
}

func TestFolderAtClampsOutOfRange(t *testing.T) {
	spec := loadExample(t)
	events := fixtureEvents(1200)
	f := NewFolder(spec, nil)
	f.Append(events...)
	now := at(1300)
	if got, want := f.At(-5, now), Fold(spec, nil, nil, now); !reflect.DeepEqual(got, want) {
		t.Errorf("At(-5) = %+v, want the empty fold", got)
	}
	if got, want := f.At(5000, now), Fold(spec, nil, events, now); !reflect.DeepEqual(got, want) {
		t.Errorf("At(5000) = %+v, want the full fold", got)
	}
}

func TestFolderStateIsNotSharedWithCheckpoints(t *testing.T) {
	spec := loadExample(t)
	events := fixtureEvents(2500)
	f := NewFolder(spec, nil)
	f.Append(events[:1500]...)
	now := at(3000)
	s := f.At(checkpointEvery, now)
	for i := range s.Steps {
		s.Steps[i].Status = StepFailed
		s.Steps[i].Rejects = -1
	}
	if s.Exit != nil {
		*s.Exit = 200
	}
	for i := range s.Checks {
		s.Checks[i].Passed = !s.Checks[i].Passed
	}
	if len(s.Restarts) > 0 {
		s.Restarts[0] = t0
	}
	f.Append(events[1500:]...)
	for _, i := range []int{checkpointEvery, checkpointEvery + 1, 2500} {
		if got, want := f.At(i, now), Fold(spec, nil, events[:i], now); !reflect.DeepEqual(got, want) {
			t.Errorf("At(%d) after editing a returned state differs from Fold", i)
		}
	}
}

func TestFolderAppendKeepsItsOwnEvents(t *testing.T) {
	spec := loadExample(t)
	events := fixtureEvents(10)
	f := NewFolder(spec, nil)
	f.Append(events...)
	want := Fold(spec, nil, fixtureEvents(10), at(20))
	events[0] = Event{Name: "worker_note"} // the caller reuses its slice
	if got := f.At(10, at(20)); !reflect.DeepEqual(got, want) {
		t.Errorf("At(10) changed when the caller's slice did")
	}
}
