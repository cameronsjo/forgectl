package cli

import (
	"testing"

	"github.com/cameronsjo/forgectl/internal/doctor"
)

// TestDoctorProblemsNamesTheFailedChecks pins doctor's closing verdict to the
// checks that failed, so it no longer says only "found problems"
// (forgectl#1148). Warn and skip rows are not problems.
func TestDoctorProblemsNamesTheFailedChecks(t *testing.T) {
	report := doctor.Report{Checks: []doctor.Check{
		{Name: "claude", State: doctor.StateOK},
		{Name: "gh", State: doctor.StateFail},
		{Name: "ghostty", State: doctor.StateWarn},
		{Name: "config", State: doctor.StateFail},
	}}
	if got, want := doctorProblems(report).Error(), "doctor found 2 problems: gh, config (each row's hint names the fix)"; got != want {
		t.Errorf("doctorProblems = %q, want %q", got, want)
	}
	one := doctor.Report{Checks: []doctor.Check{{Name: "gh", State: doctor.StateFail}}}
	if got, want := doctorProblems(one).Error(), "doctor found 1 problem: gh (each row's hint names the fix)"; got != want {
		t.Errorf("doctorProblems = %q, want %q", got, want)
	}
}
