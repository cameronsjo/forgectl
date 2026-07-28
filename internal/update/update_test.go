package update

import (
	"context"
	"errors"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// fakeStep builds a minimal Step over name, recording nothing itself — tests
// assert via the FakeRunner's Calls or the returned Result.
func fakeStep(name string, destructive bool, checkErr, applyErr error) Step {
	return Step{
		Name:        name,
		Destructive: destructive,
		Check: func(_ context.Context, run exec.Runner) (string, error) {
			out, _ := run.Run(context.Background(), name, "check")
			return out, checkErr
		},
		Apply: func(_ context.Context, run exec.Runner) (string, error) {
			out, _ := run.Run(context.Background(), name, "apply")
			return out, applyErr
		},
	}
}

func TestRun_CheckOnly_NeverMutatesEvenDestructiveSteps(t *testing.T) {
	fr := &exec.FakeRunner{}
	c := New(fr, WithSteps([]Step{fakeStep("a", true, nil, nil)}))

	report := c.Run(context.Background(), Options{CheckOnly: true})

	if len(report.Results) != 1 {
		t.Fatalf("got %d results, want 1", len(report.Results))
	}
	if report.Results[0].Skipped {
		t.Error("check-only result marked Skipped; Check is always safe, never skipped")
	}
	for _, call := range fr.Calls {
		if len(call.Args) > 0 && call.Args[len(call.Args)-1] == "apply" {
			t.Errorf("CheckOnly invoked Apply's command: %+v", call)
		}
	}
}

func TestRun_DestructiveStepSkippedWithoutYes(t *testing.T) {
	fr := &exec.FakeRunner{}
	c := New(fr, WithSteps([]Step{fakeStep("brew", true, nil, nil)}))

	report := c.Run(context.Background(), Options{Yes: false})

	if len(report.Results) != 1 {
		t.Fatalf("got %d results, want 1", len(report.Results))
	}
	res := report.Results[0]
	if !res.Skipped {
		t.Error("destructive step ran without --yes")
	}
	if res.SkipReason == "" {
		t.Error("Skipped result has no SkipReason")
	}
	if len(fr.Calls) != 0 {
		t.Errorf("Apply's command ran despite the step being skipped: %+v", fr.Calls)
	}
}

func TestRun_DestructiveStepAppliesWithYes(t *testing.T) {
	fr := &exec.FakeRunner{}
	c := New(fr, WithSteps([]Step{fakeStep("brew", true, nil, nil)}))

	report := c.Run(context.Background(), Options{Yes: true})

	res := report.Results[0]
	if res.Skipped {
		t.Error("destructive step skipped despite --yes")
	}
	if len(fr.Calls) != 1 || fr.Calls[0].Name != "brew" {
		t.Errorf("expected Apply's command to run once, got %+v", fr.Calls)
	}
}

func TestRun_NonDestructiveStepAlwaysApplies(t *testing.T) {
	fr := &exec.FakeRunner{}
	c := New(fr, WithSteps([]Step{fakeStep("softwareupdate", false, nil, nil)}))

	report := c.Run(context.Background(), Options{Yes: false})

	if report.Results[0].Skipped {
		t.Error("non-destructive step was skipped")
	}
	if len(fr.Calls) != 1 {
		t.Errorf("non-destructive step's Apply did not run: %+v", fr.Calls)
	}
}

// TestRun_PerStepIsolation is the review-critical guarantee: a failing
// step's error lands ONLY on its own Result, and every sibling still runs
// to completion with ITS OWN result intact — a broken step never wedges the
// others, in either direction (a step before OR after the failure).
func TestRun_PerStepIsolation(t *testing.T) {
	fr := &exec.FakeRunner{}
	boom := errors.New("boom")
	c := New(fr, WithSteps([]Step{
		fakeStep("ok-before", false, nil, nil),
		fakeStep("broken", false, nil, boom),
		fakeStep("ok-after", false, nil, nil),
	}))

	report := c.Run(context.Background(), Options{Yes: true})

	if len(report.Results) != 3 {
		t.Fatalf("got %d results, want 3 — a failing step must not abort the loop", len(report.Results))
	}
	if report.Results[0].Err != nil {
		t.Errorf("ok-before carries an error: %v", report.Results[0].Err)
	}
	if !errors.Is(report.Results[1].Err, boom) {
		t.Errorf("broken step's error = %v, want %v", report.Results[1].Err, boom)
	}
	if report.Results[2].Err != nil {
		t.Errorf("ok-after carries an error: %v", report.Results[2].Err)
	}
	if !report.Failed() {
		t.Error("Report.Failed() = false, want true (one step failed)")
	}
	if err := report.Err(); err == nil || !errors.Is(err, boom) {
		t.Errorf("Report.Err() = %v, want it to wrap %v", err, boom)
	}
}

func TestRun_PanicIsolatedToItsOwnResult(t *testing.T) {
	fr := &exec.FakeRunner{}
	c := New(fr, WithSteps([]Step{
		{
			Name:        "panics",
			Destructive: false,
			Check:       func(context.Context, exec.Runner) (string, error) { return "", nil },
			Apply: func(context.Context, exec.Runner) (string, error) {
				panic("kaboom")
			},
		},
		fakeStep("ok-after", false, nil, nil),
	}))

	report := c.Run(context.Background(), Options{Yes: true})

	if len(report.Results) != 2 {
		t.Fatalf("got %d results, want 2 — a panic must not abort the loop", len(report.Results))
	}
	if report.Results[0].Err == nil {
		t.Error("panicking step's Result has no error")
	}
	if report.Results[1].Err != nil {
		t.Errorf("sibling after the panic carries an error: %v", report.Results[1].Err)
	}
}

func TestRun_OnlyFiltersRosterPreservingOrder(t *testing.T) {
	fr := &exec.FakeRunner{}
	c := New(fr, WithSteps([]Step{
		fakeStep("a", false, nil, nil),
		fakeStep("b", false, nil, nil),
		fakeStep("c", false, nil, nil),
	}))

	report := c.Run(context.Background(), Options{Yes: true, Only: []string{"c", "a"}})

	var got []string
	for _, r := range report.Results {
		got = append(got, r.Name)
	}
	want := []string{"a", "c"} // roster order, not Only's order
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRun_OnStepCalledPerStep(t *testing.T) {
	fr := &exec.FakeRunner{}
	c := New(fr, WithSteps([]Step{
		fakeStep("a", false, nil, nil),
		fakeStep("b", false, nil, nil),
	}))

	var seen []string
	c.Run(context.Background(), Options{Yes: true, OnStep: func(r Result) {
		seen = append(seen, r.Name)
	}})

	if len(seen) != 2 || seen[0] != "a" || seen[1] != "b" {
		t.Errorf("OnStep saw %v, want [a b] in roster order", seen)
	}
}

func TestStepNames(t *testing.T) {
	fr := &exec.FakeRunner{}
	c := New(fr, WithSteps([]Step{
		fakeStep("a", false, nil, nil),
		fakeStep("b", false, nil, nil),
	}))
	names := c.StepNames()
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Errorf("StepNames() = %v, want [a b]", names)
	}
}

func TestResult_FailedIsFalseWhenSkipped(t *testing.T) {
	res := Result{Skipped: true, Err: errors.New("should be ignored")}
	if res.Failed() {
		t.Error("a Skipped result must never report Failed()")
	}
}

func TestReport_ErrNilWhenNothingFailed(t *testing.T) {
	report := Report{Results: []Result{{Name: "a"}, {Name: "b", Skipped: true, Err: errors.New("x")}}}
	if err := report.Err(); err != nil {
		t.Errorf("Report.Err() = %v, want nil (no non-skipped failures)", err)
	}
	if report.Failed() {
		t.Error("Report.Failed() = true, want false")
	}
}
