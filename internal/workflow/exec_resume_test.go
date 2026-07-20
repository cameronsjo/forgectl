package workflow

import (
	"context"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// spyRecorder records the (index, step) pairs the Executor checkpoints, without
// touching disk — the executor's recorder contract, isolated from state I/O.
type spyRecorder struct {
	recorded []int
	steps    []PlanStep
}

func (s *spyRecorder) Record(index int, step PlanStep) error {
	s.recorded = append(s.recorded, index)
	s.steps = append(s.steps, step)
	return nil
}

// TestExecutor_Recorder_RecordsEveryStep locks the fresh-run checkpoint contract:
// each step is recorded once, in order, after it succeeds.
func TestExecutor_Recorder_RecordsEveryStep(t *testing.T) {
	fake := &exec.FakeRunner{}
	spy := &spyRecorder{}

	plan := Plan{Name: "three", Steps: []PlanStep{
		{Uses: "run", Cmd: "a"},
		{Uses: "run", Cmd: "b"},
		{Uses: "run", Cmd: "c"},
	}}

	exe := NewExecutor(fake, testRegistry(t), WithRecorder(spy))
	if err := exe.Run(context.Background(), plan, NewContext(nil)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(fake.Calls) != 3 {
		t.Fatalf("expected 3 Runner calls, got %d", len(fake.Calls))
	}
	if len(spy.recorded) != 3 || spy.recorded[0] != 0 || spy.recorded[1] != 1 || spy.recorded[2] != 2 {
		t.Fatalf("expected checkpoints for steps 0,1,2 in order, got %v", spy.recorded)
	}
	if spy.steps[0].Cmd != "a" || spy.steps[2].Cmd != "c" {
		t.Errorf("recorder received the wrong plan steps: %+v", spy.steps)
	}
}

// TestExecutor_ResumeFrom_SkipsCompletedSteps is the core resume behavior at the
// executor level: with resumeFrom=2, the first two steps are treated as done
// (no Runner call, no checkpoint) and only step 2 executes and records.
func TestExecutor_ResumeFrom_SkipsCompletedSteps(t *testing.T) {
	fake := &exec.FakeRunner{}
	spy := &spyRecorder{}

	plan := Plan{Name: "three", Steps: []PlanStep{
		{Uses: "run", Cmd: "a"},
		{Uses: "run", Cmd: "b"},
		{Uses: "run", Cmd: "c"},
	}}

	exe := NewExecutor(fake, testRegistry(t), WithRecorder(spy), WithResumeFrom(2))
	if err := exe.Run(context.Background(), plan, NewContext(nil)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(fake.Calls) != 1 {
		t.Fatalf("resume from 2 must run exactly the last step, got %d calls: %+v", len(fake.Calls), fake.Calls)
	}
	if fake.Calls[0].Name != "c" {
		t.Errorf("expected the resumed step 'c' to run, got %q", fake.Calls[0].Name)
	}
	if len(spy.recorded) != 1 || spy.recorded[0] != 2 {
		t.Errorf("only step 2 should checkpoint on resume, got %v", spy.recorded)
	}
}
