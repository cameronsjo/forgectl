// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package runview

import (
	"reflect"
	"testing"
	"time"
)

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

var fetchBuild = []StepDef{{ID: "fetch", Note: "pull the inputs"}, {ID: "build", Note: "compile and package"}}

func fold(events ...Event) RunState { return Fold(DeskSpec(), fetchBuild, events) }

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
	s := fold()
	want := []StepState{
		{ID: "fetch", Note: "pull the inputs", Status: StepPending},
		{ID: "build", Note: "compile and package", Status: StepPending},
	}
	if !reflect.DeepEqual(s.Steps, want) {
		t.Errorf("Steps = %+v, want %+v", s.Steps, want)
	}
	if s.Live != LiveLive || s.Exit != nil {
		t.Errorf("Live %s Exit %v, want live and no exit", s.Live, s.Exit)
	}
}

func TestFoldNilSpecShowsNoStepState(t *testing.T) {
	s := Fold(nil, fetchBuild, []Event{ev(1, "STEP-START", "fetch"), ev(2, "RUN-END", "", "rc", "0")})
	if s.Live != LiveLive || s.Exit != nil || step(t, s, "fetch").Status != StepPending {
		t.Errorf("nil spec changed state: %+v", s)
	}
}

func TestFoldStartThenClose(t *testing.T) {
	s := fold(ev(1, "STEP-START", "fetch"))
	got := step(t, s, "fetch")
	if got.Status != StepRunning || !got.Start.Equal(at(1)) || !got.End.IsZero() {
		t.Errorf("after start: %+v", got)
	}
	s = fold(ev(1, "STEP-START", "fetch"), ev(5, "STEP-END", "fetch"))
	got = step(t, s, "fetch")
	if got.Status != StepClosed || !got.End.Equal(at(5)) || !got.Start.Equal(at(1)) {
		t.Errorf("after close: %+v", got)
	}
	if b := step(t, s, "build"); b.Status != StepPending {
		t.Errorf("build = %+v, want pending", b)
	}
}

func TestFoldStartClearsEnd(t *testing.T) {
	s := fold(ev(1, "STEP-START", "build"), ev(2, "STEP-FAIL", "build"), ev(3, "STEP-START", "build"))
	got := step(t, s, "build")
	if got.Status != StepRunning || !got.End.IsZero() || !got.Start.Equal(at(3)) {
		t.Errorf("restarted step: %+v", got)
	}
}

func TestFoldCloseOnlyFromRunning(t *testing.T) {
	s := fold(ev(1, "STEP-END", "build"))
	if got := step(t, s, "build"); got.Status != StepPending || !got.End.IsZero() {
		t.Errorf("close on a pending step: %+v, want unchanged", got)
	}
}

func TestFoldFailFromAnyStatus(t *testing.T) {
	s := fold(ev(4, "STEP-FAIL", "build"))
	if got := step(t, s, "build"); got.Status != StepFailed || !got.End.Equal(at(4)) {
		t.Errorf("fail on a pending step: %+v, want failed at %v", got, at(4))
	}
	s = fold(ev(1, "STEP-START", "build"), ev(2, "STEP-END", "build"), ev(3, "STEP-FAIL", "build"))
	if got := step(t, s, "build"); got.Status != StepFailed || !got.End.Equal(at(3)) {
		t.Errorf("fail after close: %+v, want failed at %v", got, at(3))
	}
}

func TestFoldSkipMovesAPendingStepToSkipped(t *testing.T) {
	s := fold(ev(1, "STEP-FAIL", "fetch"), ev(2, "STEP-SKIP", "build"))
	got := step(t, s, "build")
	if got.Status != StepSkipped || !got.Start.IsZero() || !got.End.IsZero() {
		t.Errorf("build = %+v, want skipped with no times", got)
	}
	if !s.LastEvent.Equal(at(2)) {
		t.Errorf("LastEvent = %v, want the skip's time", s.LastEvent)
	}
	if s.UnknownSteps != 0 {
		t.Errorf("UnknownSteps = %d, want 0", s.UnknownSteps)
	}
}

func TestFoldSkipIsIgnoredOnAStepThatAlreadyStarted(t *testing.T) {
	for _, c := range []struct {
		name   string
		events []Event
		want   StepStatus
	}{
		{"running", []Event{ev(1, "STEP-START", "build")}, StepRunning},
		{"closed", []Event{ev(1, "STEP-START", "build"), ev(2, "STEP-END", "build")}, StepClosed},
		{"failed", []Event{ev(1, "STEP-FAIL", "build")}, StepFailed},
		{"already skipped", []Event{ev(1, "STEP-SKIP", "build")}, StepSkipped},
	} {
		s := fold(append(c.events, ev(9, "STEP-SKIP", "build"))...)
		got, base := step(t, s, "build"), step(t, fold(c.events...), "build")
		if got != base || got.Status != c.want {
			t.Errorf("%s: after a skip %+v, before %+v; want %s unchanged", c.name, got, base, c.want)
		}
	}
}

func TestFoldSkipOfAnUnknownStepIsCounted(t *testing.T) {
	s := fold(ev(1, "STEP-SKIP", "deploy"), ev(2, "STEP-SKIP", ""))
	if s.UnknownSteps != 2 {
		t.Errorf("UnknownSteps = %d, want 2", s.UnknownSteps)
	}
	for _, st := range s.Steps {
		if st.Status != StepPending {
			t.Errorf("step %s = %s, want pending", st.ID, st.Status)
		}
	}
}

func TestFoldStartAfterASkipRunsTheStep(t *testing.T) {
	s := fold(ev(1, "STEP-SKIP", "build"), ev(2, "STEP-START", "build"), ev(3, "RUN-END", "", "rc", "0"))
	if got := step(t, s, "build"); got.Status != StepInterrupted || !got.Start.Equal(at(2)) {
		t.Errorf("build = %+v, want started at %v then interrupted by the end", got, at(2))
	}
}

func TestFoldSkippedStepSurvivesTheEnd(t *testing.T) {
	s := fold(ev(1, "STEP-SKIP", "build"), ev(2, "RUN-END", "", "rc", "1"))
	if got := step(t, s, "build"); got.Status != StepSkipped {
		t.Errorf("build = %+v, want skipped still after the run ended", got)
	}
}

func TestFoldEndInterruptsARunningStep(t *testing.T) {
	events := []Event{ev(1, "STEP-START", "fetch"), ev(2, "STEP-END", "fetch"), ev(3, "STEP-START", "build"), ev(4, "RUN-END", "", "rc", "1")}
	s := fold(events...)
	if got := step(t, s, "build"); got.Status != StepInterrupted || !got.End.Equal(at(4)) {
		t.Errorf("build = %+v, want interrupted, ended at %v", got, at(4))
	}
	if got := step(t, s, "fetch"); got.Status != StepClosed {
		t.Errorf("fetch = %+v, want closed", got)
	}
	// Only the result says interrupted: the reducer keeps the step running, so
	// a later close still lands.
	s = fold(append(events, ev(6, "STEP-END", "build"))...)
	if got := step(t, s, "build"); got.Status != StepClosed || !got.End.Equal(at(6)) {
		t.Errorf("build after a late close = %+v, want closed at %v", got, at(6))
	}
}

func TestFoldEndSetsExit(t *testing.T) {
	for _, c := range []struct {
		rc   string
		exit int
	}{{"0", 0}, {"3", 3}, {"255", 255}} {
		s := fold(ev(1, "RUN-END", "", "rc", c.rc))
		if s.Exit == nil || *s.Exit != c.exit || s.BadExits != 0 {
			t.Errorf("rc=%s: Exit %v bad %d, want %d", c.rc, s.Exit, s.BadExits, c.exit)
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
		s := fold(ev(1, "RUN-END", "", "rc", "0"), ev(2, "RUN-END", "", kv...))
		if s.BadExits != 1 {
			t.Errorf("rc=%q: BadExits = %d, want 1", rc, s.BadExits)
		}
		if s.Exit == nil || *s.Exit != 0 || s.Live != LiveEnded {
			t.Errorf("rc=%q: Exit %v Live %s, want the earlier valid exit kept and ended", rc, s.Exit, s.Live)
		}
	}
	// A run whose only end is bad is ended with no exit.
	if s := fold(ev(1, "RUN-END", "", "rc", "x")); s.Live != LiveEnded || s.Exit != nil || s.BadExits != 1 {
		t.Errorf("only a bad end: %+v", s)
	}
}

func TestFoldUnmappedEventChangesOnlyLastEvent(t *testing.T) {
	base := []Event{ev(1, "STEP-START", "fetch"), ev(2, "STEP-END", "fetch")}
	without := fold(base...)
	with := fold(append(base, ev(3, "STEP-WARN", "fetch", "rc", "1"), ev(4, "RUN-NOTE", ""), ev(5, "STEP-NOTE", "build"))...)
	if !with.LastEvent.Equal(at(5)) {
		t.Errorf("LastEvent = %v, want %v", with.LastEvent, at(5))
	}
	with.LastEvent = without.LastEvent
	if !reflect.DeepEqual(with, without) {
		t.Errorf("unmapped events changed state:\n with %+v\n without %+v", with, without)
	}
}

func TestFoldZeroTimeEventKeepsLastEvent(t *testing.T) {
	s := fold(ev(1, "STEP-START", "fetch"), Event{Name: "STEP-END", Step: "fetch"})
	if !s.LastEvent.Equal(at(1)) {
		t.Errorf("LastEvent = %v, want the last event that had a time (%v)", s.LastEvent, at(1))
	}
	if got := step(t, s, "fetch"); got.Status != StepClosed || !got.End.IsZero() {
		t.Errorf("fetch = %+v, want closed with a zero End", got)
	}
}

func TestFoldUnknownStepIsCountedNotAdded(t *testing.T) {
	// start, close and fail each count; an empty step counts too.
	s := fold(ev(1, "STEP-START", "deploy"), ev(2, "STEP-END", "deploy"), ev(3, "STEP-FAIL", "deploy"), ev(4, "STEP-START", ""))
	if s.UnknownSteps != 4 {
		t.Errorf("UnknownSteps = %d, want 4", s.UnknownSteps)
	}
	if len(s.Steps) != 2 {
		t.Errorf("Steps = %+v, want only the two defined steps", s.Steps)
	}
	for _, st := range s.Steps {
		if st.Status != StepPending {
			t.Errorf("step %s = %s, want pending", st.ID, st.Status)
		}
	}
	// An unmapped event naming an unknown step is not counted.
	if s := fold(ev(1, "STEP-WARN", "deploy")); s.UnknownSteps != 0 {
		t.Errorf("an unmapped event counted UnknownSteps = %d", s.UnknownSteps)
	}
}

func TestFoldEdgesLinearChain(t *testing.T) {
	if s := Fold(DeskSpec(), fetchBuild, nil); !reflect.DeepEqual(s.Edges, [][2]string{{"fetch", "build"}}) {
		t.Errorf("two-step Edges = %v", s.Edges)
	}
	defs := []StepDef{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	if s := Fold(DeskSpec(), defs, nil); !reflect.DeepEqual(s.Edges, [][2]string{{"a", "b"}, {"b", "c"}}) {
		t.Errorf("chain Edges = %v", s.Edges)
	}
	if s := Fold(DeskSpec(), []StepDef{{ID: "a"}}, nil); s.Edges != nil {
		t.Errorf("one step Edges = %v, want none", s.Edges)
	}
	if s := Fold(DeskSpec(), nil, nil); s.Edges != nil || len(s.Steps) != 0 {
		t.Errorf("no defs: %+v", s)
	}
}

func TestFoldEdgesFromAfter(t *testing.T) {
	defs := []StepDef{{ID: "a"}, {ID: "b"}, {ID: "c", After: []string{"a", "b"}}, {ID: "d", After: []string{"c"}}}
	s := Fold(DeskSpec(), defs, nil)
	want := [][2]string{{"a", "c"}, {"b", "c"}, {"c", "d"}}
	if !reflect.DeepEqual(s.Edges, want) {
		t.Errorf("Edges = %v, want %v (After wins; no chain edge a->b)", s.Edges, want)
	}
	if len(s.Steps) != 4 || s.Steps[3].ID != "d" {
		t.Errorf("Steps = %+v, want the passed defs", s.Steps)
	}
}

func TestFoldReturnsIndependentState(t *testing.T) {
	events := []Event{ev(0, "STEP-START", "fetch"), ev(2, "RUN-END", "", "rc", "0")}
	defs := []StepDef{{ID: "fetch"}, {ID: "build", After: []string{"fetch"}}}
	a := Fold(DeskSpec(), defs, events)
	a.Steps[0].Status = StepFailed
	*a.Exit = 9
	a.Edges[0] = [2]string{"x", "y"}
	b := Fold(DeskSpec(), defs, events)
	if b.Steps[0].Status != StepInterrupted || *b.Exit != 0 || b.Edges[0] != [2]string{"fetch", "build"} {
		t.Errorf("a second Fold saw the first result's edits: %+v", b)
	}
	if defs[0].ID != "fetch" || len(events) != 2 || events[0].Name != "STEP-START" {
		t.Errorf("Fold changed its inputs")
	}
}

// A lost run (RUN-LOST, no RUN-END) reads lost, and the step it stopped in
// reads interrupted, not running (#1106).
func TestFoldLostRunInterruptsItsStep(t *testing.T) {
	s := fold(ev(1, "STEP-START", "fetch"), ev(9, "RUN-LOST", ""))
	if s.Live != LiveLost || s.Exit != nil {
		t.Errorf("Live %s Exit %v, want lost and no exit", s.Live, s.Exit)
	}
	if got := step(t, s, "fetch"); got.Status != StepInterrupted || !got.End.Equal(at(9)) {
		t.Errorf("fetch = %+v, want interrupted at 9", got)
	}
	// A RUN-END already seen wins: the run ended, it was not lost.
	s = fold(ev(1, "STEP-START", "fetch"), ev(5, "RUN-END", "", "rc", "1"), ev(9, "RUN-LOST", ""))
	if s.Live != LiveEnded || s.Exit == nil || *s.Exit != 1 {
		t.Errorf("RUN-END then RUN-LOST: Live %s Exit %v", s.Live, s.Exit)
	}
}
