// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package runview

import (
	"reflect"
	"testing"
	"time"
)

// One test per row of the reducer table, against the example spec.

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// at is t0 plus sec seconds.
func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

// ev builds an event at second sec; kv are field key/value pairs.
func ev(sec int, name, step string, kv ...string) Event {
	e := Event{Time: at(sec), Name: name, Step: step}
	for i := 0; i+1 < len(kv); i += 2 {
		e.Fields = append(e.Fields, Field{Key: kv[i], Value: kv[i+1]})
	}
	return e
}

func fold(t *testing.T, events ...Event) RunState {
	t.Helper()
	return Fold(loadExample(t), nil, events, at(60))
}

func step(t *testing.T, s RunState, id string) StepState {
	t.Helper()
	for _, st := range s.Steps {
		if st.ID == id {
			return st
		}
	}
	t.Fatalf("no step %q in %+v", id, s.Steps)
	return StepState{}
}

func TestFoldStartsPending(t *testing.T) {
	s := fold(t)
	want := []StepState{
		{ID: "fetch", Note: "pull the inputs", Status: StepPending},
		{ID: "build", Note: "compile and package", Status: StepPending},
	}
	if !reflect.DeepEqual(s.Steps, want) {
		t.Errorf("Steps = %+v, want %+v", s.Steps, want)
	}
}

func TestFoldStartThenClose(t *testing.T) {
	s := fold(t, ev(1, "stage_started", "fetch"))
	got := step(t, s, "fetch")
	if got.Status != StepRunning || got.Attempt != 1 || !got.Start.Equal(at(1)) || !got.End.IsZero() || got.Resumed {
		t.Errorf("after start: %+v", got)
	}
	s = fold(t, ev(1, "stage_started", "fetch"), ev(5, "stage_done", "fetch"))
	got = step(t, s, "fetch")
	if got.Status != StepClosed || !got.End.Equal(at(5)) || !got.Start.Equal(at(1)) {
		t.Errorf("after close: %+v", got)
	}
	if b := step(t, s, "build"); b.Status != StepPending {
		t.Errorf("build = %+v, want pending", b)
	}
}

func TestFoldStartTakesAttemptFromEventElseIncrements(t *testing.T) {
	s := fold(t, ev(1, "stage_started", "build", "try", "4"))
	if got := step(t, s, "build").Attempt; got != 4 {
		t.Errorf("attempt from event = %d, want 4", got)
	}
	s = fold(t, ev(1, "stage_started", "build", "try", "4"), ev(2, "stage_gave_up", "build"),
		ev(3, "stage_started", "build"), ev(4, "stage_started", "build", "try", "zero"))
	if got := step(t, s, "build").Attempt; got != 6 {
		t.Errorf("attempt without a usable field = %d, want 6 (4, then +1, then +1)", got)
	}
}

func TestFoldStartClearsEnd(t *testing.T) {
	s := fold(t, ev(1, "stage_started", "build"), ev(2, "stage_gave_up", "build"), ev(3, "stage_started", "build"))
	got := step(t, s, "build")
	if got.Status != StepRunning || !got.End.IsZero() || !got.Start.Equal(at(3)) || got.Attempt != 2 {
		t.Errorf("restarted step: %+v", got)
	}
}

func TestFoldCloseOnlyFromRunning(t *testing.T) {
	s := fold(t, ev(1, "stage_done", "build"))
	if got := step(t, s, "build"); got.Status != StepPending || !got.End.IsZero() {
		t.Errorf("close on a pending step: %+v, want unchanged", got)
	}
}

func TestFoldRejectsCountWithoutFailingThenFail(t *testing.T) {
	events := []Event{
		ev(1, "stage_started", "build"),
		ev(2, "stage_refused", "build"),
		ev(3, "stage_refused", "build"),
		ev(4, "stage_refused", "build"),
	}
	s := fold(t, events...)
	got := step(t, s, "build")
	if got.Rejects != 3 || got.Status != StepRunning || !got.End.IsZero() {
		t.Errorf("after 3 rejects (max_rejects 3): %+v, want running with 3 rejects", got)
	}
	s = fold(t, append(events, ev(9, "stage_gave_up", "build"))...)
	got = step(t, s, "build")
	if got.Status != StepFailed || !got.End.Equal(at(9)) || got.Rejects != 3 {
		t.Errorf("after fail: %+v", got)
	}
}

func TestFoldRestartBeforeAnyStepIsTheInitialStart(t *testing.T) {
	s := fold(t, ev(0, "worker_boot", ""), ev(1, "stage_started", "fetch"))
	if len(s.Restarts) != 0 {
		t.Errorf("Restarts = %v, want none", s.Restarts)
	}
	if got := step(t, s, "fetch"); got.Resumed {
		t.Errorf("fetch = %+v, want not resumed", got)
	}
}

func TestFoldRestartMidStepResumesAndClearsExit(t *testing.T) {
	events := []Event{
		ev(0, "worker_boot", ""),
		ev(1, "stage_started", "fetch"),
		ev(2, "stage_done", "fetch"),
		ev(3, "stage_started", "build"),
		ev(4, "run_finished", "", "rc", "3"),
		ev(5, "worker_boot", ""),
	}
	s := fold(t, events...)
	if !reflect.DeepEqual(s.Restarts, []time.Time{at(5)}) {
		t.Errorf("Restarts = %v, want [%v]", s.Restarts, at(5))
	}
	if s.Exit != nil || s.ExitMeaning != "" {
		t.Errorf("Exit = %v %q, want cleared by the restart", s.Exit, s.ExitMeaning)
	}
	if got := step(t, s, "build"); got.Status != StepRunning || !got.Resumed || !got.End.IsZero() {
		t.Errorf("build after the restart = %+v, want running again and resumed", got)
	}
	if s.Live == LiveEnded {
		t.Errorf("Live = ended after a later restart")
	}

	s = fold(t, append(events, ev(6, "stage_started", "build"))...)
	if got := step(t, s, "build"); !got.Resumed || got.Attempt != 2 || got.Status != StepRunning {
		t.Errorf("build after restart = %+v, want resumed attempt 2", got)
	}
	if got := step(t, s, "fetch"); got.Status != StepClosed || got.Resumed {
		t.Errorf("fetch = %+v, want closed and untouched", got)
	}
}

// A worker that resumes a step without sending its start again still marks
// it resumed, and the step's own close still lands.
func TestFoldRestartResumesARunningStepWithoutANewStart(t *testing.T) {
	s := fold(t, ev(0, "worker_boot", ""), ev(1, "stage_started", "fetch"), ev(2, "worker_boot", ""), ev(3, "stage_done", "fetch"))
	if len(s.Restarts) != 1 {
		t.Errorf("Restarts = %v, want one", s.Restarts)
	}
	if got := step(t, s, "fetch"); got.Status != StepClosed || !got.Resumed || got.Attempt != 1 {
		t.Errorf("fetch = %+v, want closed, resumed, attempt 1", got)
	}
	if got := step(t, s, "build"); got.Resumed {
		t.Errorf("build = %+v; a step that was not running is not resumed", got)
	}
}

// The attempt can arrive on any event of the step, not only its start: a
// worker retrying after a reject sends no new start. The highest one wins.
func TestFoldAttemptComesFromAnyEventOfTheStep(t *testing.T) {
	s := fold(t, ev(1, "stage_started", "build", "try", "1"), ev(2, "stage_refused", "build", "try", "1"),
		ev(3, "worker_note", "build", "try", "2"), ev(4, "stage_refused", "build", "try", "2"))
	if got := step(t, s, "build"); got.Attempt != 2 || got.Rejects != 2 || got.Status != StepRunning {
		t.Errorf("after a retry with no start = %+v, want attempt 2, 2 rejects, running", got)
	}
	s = fold(t, ev(1, "stage_started", "build"), ev(2, "stage_done", "build", "try", "3"))
	if got := step(t, s, "build"); got.Attempt != 3 || got.Status != StepClosed {
		t.Errorf("close carrying try 3 = %+v, want attempt 3, closed", got)
	}
	s = fold(t, ev(1, "stage_started", "build", "try", "4"), ev(2, "stage_gave_up", "build", "try", "2"), ev(3, "worker_note", "build", "try", "x"))
	if got := step(t, s, "build"); got.Attempt != 4 || got.Status != StepFailed {
		t.Errorf("lower and unusable tries = %+v, want attempt 4 kept, failed", got)
	}
	s = fold(t, ev(1, "worker_note", "deploy", "try", "2"))
	if s.UnknownSteps != 0 {
		t.Errorf("an unmapped event for an unknown step counted UnknownSteps = %d", s.UnknownSteps)
	}
}

// An end with a step still running turns that step interrupted, ended at
// the end event; a restart after it makes the step running again.
func TestFoldEndInterruptsARunningStep(t *testing.T) {
	events := []Event{ev(1, "stage_started", "fetch"), ev(2, "stage_done", "fetch"), ev(3, "stage_started", "build"), ev(4, "run_finished", "", "rc", "1")}
	s := fold(t, events...)
	if got := step(t, s, "build"); got.Status != StepInterrupted || !got.End.Equal(at(4)) {
		t.Errorf("build = %+v, want interrupted, ended at %v", got, at(4))
	}
	if got := step(t, s, "fetch"); got.Status != StepClosed {
		t.Errorf("fetch = %+v, want closed", got)
	}
	s = fold(t, append(events, ev(5, "worker_boot", ""), ev(6, "stage_done", "build"))...)
	if got := step(t, s, "build"); got.Status != StepClosed || !got.Resumed || !got.End.Equal(at(6)) {
		t.Errorf("build after a restart and its close = %+v, want closed at %v, resumed", got, at(6))
	}
}

func TestFoldStartWithoutRestartIsNotResumed(t *testing.T) {
	s := fold(t, ev(1, "stage_started", "build"), ev(2, "worker_boot", ""), ev(3, "stage_started", "build"),
		ev(4, "stage_gave_up", "build"), ev(5, "stage_started", "build"))
	if got := step(t, s, "build"); got.Resumed {
		t.Errorf("build = %+v, want not resumed: no restart since its previous start", got)
	}
}

func TestFoldEndSetsExitAndMeaning(t *testing.T) {
	cases := []struct {
		rc      string
		exit    int
		meaning string
	}{
		{"0", 0, "finished, audit passed"},
		{"3", 3, "audit rejected"},
		{"1", 1, ""},
		{"255", 255, ""},
	}
	for _, c := range cases {
		s := fold(t, ev(1, "run_finished", "", "rc", c.rc))
		if s.Exit == nil || *s.Exit != c.exit || s.ExitMeaning != c.meaning || s.BadExits != 0 {
			t.Errorf("rc=%s: Exit %v meaning %q bad %d, want %d %q", c.rc, s.Exit, s.ExitMeaning, s.BadExits, c.exit, c.meaning)
		}
		if s.Live != LiveEnded {
			t.Errorf("rc=%s: Live = %s, want ended", c.rc, s.Live)
		}
	}
}

func TestFoldEndWithBadExitIsIgnoredAndCounted(t *testing.T) {
	for _, rc := range []string{"-1", "256", "x", "1.5", ""} {
		kv := []string{"rc", rc}
		if rc == "" {
			kv = nil // the field is missing
		}
		s := fold(t, ev(1, "run_finished", "", "rc", "0"), ev(2, "run_finished", "", kv...))
		if s.BadExits != 1 {
			t.Errorf("rc=%q: BadExits = %d, want 1", rc, s.BadExits)
		}
		if s.Exit == nil || *s.Exit != 0 || s.ExitMeaning != "finished, audit passed" {
			t.Errorf("rc=%q: Exit %v %q, want the earlier valid exit kept", rc, s.Exit, s.ExitMeaning)
		}
	}
}

func TestFoldCheckPassAndFail(t *testing.T) {
	s := fold(t)
	if want := []CheckState{{ID: "audit", Label: "audit ok?"}}; !reflect.DeepEqual(s.Checks, want) {
		t.Errorf("unseen Checks = %+v, want %+v", s.Checks, want)
	}
	s = fold(t, ev(1, "audit_result", "", "rc", "0"))
	if want := []CheckState{{ID: "audit", Label: "audit ok?", Seen: true, Passed: true}}; !reflect.DeepEqual(s.Checks, want) {
		t.Errorf("passing Checks = %+v, want %+v", s.Checks, want)
	}
	for _, rc := range []string{"3", "x"} {
		s = fold(t, ev(1, "audit_result", "", "rc", rc))
		if want := []CheckState{{ID: "audit", Label: "audit ok?", Seen: true}}; !reflect.DeepEqual(s.Checks, want) {
			t.Errorf("rc=%s: Checks = %+v, want %+v", rc, s.Checks, want)
		}
		if s.Exit != nil {
			t.Errorf("rc=%s: a check set Exit %d; only end sets it", rc, *s.Exit)
		}
	}
}

func TestFoldUnmappedEventLeavesStateAlone(t *testing.T) {
	base := []Event{ev(1, "stage_started", "fetch"), ev(2, "stage_done", "fetch")}
	without := fold(t, base...)
	with := fold(t, append(base, ev(3, "worker_note", "fetch", "rc", "1"))...)
	if !with.LastEvent.Equal(at(3)) {
		t.Errorf("LastEvent = %v, want the unmapped event's time %v", with.LastEvent, at(3))
	}
	with.LastEvent = without.LastEvent
	if !reflect.DeepEqual(with, without) {
		t.Errorf("unmapped event changed state:\n with %+v\n without %+v", with, without)
	}
}

func TestFoldUnknownStepIsCountedNotAdded(t *testing.T) {
	s := fold(t, ev(1, "stage_started", "deploy"), ev(2, "stage_done", "deploy"), ev(3, "stage_started", ""))
	if s.UnknownSteps != 3 {
		t.Errorf("UnknownSteps = %d, want 3", s.UnknownSteps)
	}
	if len(s.Steps) != 2 {
		t.Errorf("Steps = %+v, want only the two defined steps", s.Steps)
	}
	for _, st := range s.Steps {
		if st.Status != StepPending {
			t.Errorf("step %s = %s, want pending", st.ID, st.Status)
		}
	}
}

func TestFoldEdgesLinearChain(t *testing.T) {
	spec := loadExample(t)
	if s := Fold(spec, nil, nil, t0); !reflect.DeepEqual(s.Edges, [][2]string{{"fetch", "build"}}) {
		t.Errorf("spec Edges = %v", s.Edges)
	}
	defs := []StepDef{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	if s := Fold(spec, defs, nil, t0); !reflect.DeepEqual(s.Edges, [][2]string{{"a", "b"}, {"b", "c"}}) {
		t.Errorf("chain Edges = %v", s.Edges)
	}
	if s := Fold(spec, []StepDef{{ID: "a"}}, nil, t0); s.Edges != nil {
		t.Errorf("one step Edges = %v, want none", s.Edges)
	}
}

func TestFoldEdgesFromAfter(t *testing.T) {
	defs := []StepDef{{ID: "a"}, {ID: "b"}, {ID: "c", After: []string{"a", "b"}}, {ID: "d", After: []string{"c"}}}
	s := Fold(loadExample(t), defs, nil, t0)
	want := [][2]string{{"a", "c"}, {"b", "c"}, {"c", "d"}}
	if !reflect.DeepEqual(s.Edges, want) {
		t.Errorf("Edges = %v, want %v", s.Edges, want)
	}
	if len(s.Steps) != 4 || s.Steps[3].ID != "d" {
		t.Errorf("Steps = %+v, want the passed defs", s.Steps)
	}
}

func TestFoldLive(t *testing.T) {
	spec := loadExample(t) // stale_after = 10m
	running := []Event{ev(0, "stage_started", "fetch")}
	cases := []struct {
		name   string
		events []Event
		now    time.Time
		stale  time.Duration
		want   LiveState
	}{
		{"ended", []Event{ev(0, "run_finished", "", "rc", "0")}, at(3600), 10 * time.Minute, LiveEnded},
		{"ended with a bad exit", []Event{ev(0, "run_finished", "", "rc", "x")}, at(3600), 10 * time.Minute, LiveEnded},
		{"restart after end", []Event{ev(0, "stage_started", "fetch"), ev(1, "run_finished", "", "rc", "0"), ev(2, "worker_boot", "")}, at(60), 10 * time.Minute, LiveLive},
		{"within stale_after", running, at(599), 10 * time.Minute, LiveLive},
		{"at stale_after", running, at(600), 10 * time.Minute, LiveLive},
		{"past stale_after", running, at(601), 10 * time.Minute, LiveStale},
		{"no events", nil, at(1), 10 * time.Minute, LiveStale},
		{"no stale_after", running, at(86400), 0, LiveLive},
	}
	for _, c := range cases {
		spec.Live.StaleAfter = c.stale
		if got := Fold(spec, nil, c.events, c.now).Live; got != c.want {
			t.Errorf("%s: Live = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestFoldReturnsIndependentState(t *testing.T) {
	spec := loadExample(t)
	events := []Event{ev(0, "stage_started", "fetch"), ev(1, "worker_boot", ""), ev(2, "run_finished", "", "rc", "0")}
	a := Fold(spec, nil, events, at(5))
	a.Steps[0].Status = StepFailed
	*a.Exit = 9
	a.Restarts[0] = t0
	b := Fold(spec, nil, events, at(5))
	if b.Steps[0].Status != StepInterrupted || *b.Exit != 0 || !b.Restarts[0].Equal(at(1)) {
		t.Errorf("a second Fold saw the first result's edits: %+v", b)
	}
	if spec.Steps[0].ID != "fetch" || len(spec.Steps) != 2 {
		t.Errorf("Fold changed the spec: %+v", spec.Steps)
	}
}
