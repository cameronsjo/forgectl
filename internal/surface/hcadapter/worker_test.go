package hcadapter

import (
	"testing"

	"github.com/cameronsjo/forgectl/internal/meta"
)

// TestForgectlStatusAppIsTheSharedAppName: progstatus writes the same constant
// into every report, so the adapter's check for forgectl's own status keeps
// matching when the tool is renamed.
func TestForgectlStatusAppIsTheSharedAppName(t *testing.T) {
	if forgectlStatusApp != meta.AppName {
		t.Errorf("forgectlStatusApp = %q, want meta.AppName %q", forgectlStatusApp, meta.AppName)
	}
}
