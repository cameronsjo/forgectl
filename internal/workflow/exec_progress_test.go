// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package workflow

import (
	"context"
	"fmt"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// TestExecutor_Progress_ReportsPositionOverFullPlan pins that a resumed run
// reports 3/3, not 1/1: i is 1-based and n is the plan's full length, and a
// step skipped by resume does not call the callback.
func TestExecutor_Progress_ReportsPositionOverFullPlan(t *testing.T) {
	plan := Plan{Name: "three", Steps: []PlanStep{
		{Uses: "run", Cmd: "a"}, {Uses: "run", Cmd: "b"}, {Uses: "run", Cmd: "c"},
	}}
	cases := []struct {
		name   string
		resume int
		want   []string
	}{
		{"fresh", 0, []string{"1/3 run", "2/3 run", "3/3 run"}},
		{"resumed from 2", 2, []string{"3/3 run"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			exe := NewExecutor(&exec.FakeRunner{}, testRegistry(t),
				WithResumeFrom(tc.resume),
				WithProgress(func(i, n int, uses string) { got = append(got, fmt.Sprintf("%d/%d %s", i, n, uses)) }))
			if err := exe.Run(context.Background(), plan, NewContext(nil)); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("progress = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestExecutor_Progress_DryRunReportsNothing: dry-run executes no step, so no
// progress is reported.
func TestExecutor_Progress_DryRunReportsNothing(t *testing.T) {
	called := false
	exe := NewExecutor(&exec.FakeRunner{}, testRegistry(t), WithDryRun(true),
		WithProgress(func(int, int, string) { called = true }))
	if err := exe.Run(context.Background(), Plan{Steps: []PlanStep{{Uses: "run", Cmd: "a"}}}, NewContext(nil)); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("dry-run reported progress")
	}
}
