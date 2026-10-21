package merge

import (
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/intake"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// TestWorkerRulesMentionNoMarker pins that the rules forgectl gives a worker
// describe the review marker without mentioning it as this scan reads one. A
// worker posts as the operator, who is the marker author, so a comment that
// echoes a rule spelling the marker's name would refuse the merge outright.
//
// Mutation that turns it red: put "cadence-review" back in worker.WorkerRules
// or in intake's brief template.
func TestWorkerRulesMentionNoMarker(t *testing.T) {
	brief, err := intake.Brief("cameronsjo/forgectl", intake.Issue{Number: 1, Title: "Fix it", Body: "Fix the login bug."},
		func() (string, error) { return "a1b2c3d4e5f6", nil })
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"worker.WorkerRules": worker.WorkerRules, "the intake brief": brief} {
		if n, _ := mentionsOf(text); n != 0 {
			t.Errorf("%s mentions the marker %d time(s) as the merge policy's scan reads it:\n%s", name, n, text)
		}
	}
}
