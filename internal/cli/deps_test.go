package cli

import (
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
)

// TestProductionDeps_FillsEverySeam pins that the dependency set Execute hands
// to every module constructor is complete. A seam production forgets to fill
// is a nil interface whichever module first needs it discovers at its own call
// site, at which point the failure is far from the wiring that caused it.
func TestProductionDeps_FillsEverySeam(t *testing.T) {
	boundary := &config.LegacyMigrationBoundary{}
	deps := productionDeps(config.Config{}, boundary)

	if deps.Runner == nil {
		t.Error("Runner is nil")
	}
	if deps.SensitiveRunner == nil {
		t.Error("SensitiveRunner is nil; a bootstrap-bearing command would have no bounded seam to run through")
	}
	if deps.LegacyBoundary != boundary {
		t.Error("LegacyBoundary was not passed through")
	}

	// The two runners are deliberately different types: the sensitive seam is a
	// separate interface precisely so a caller cannot reach the argv-logging
	// runner by accident when it meant the bounded one.
	if _, ok := deps.SensitiveRunner.(exec.Runner); ok {
		t.Error("the sensitive runner also satisfies Runner; the seams must stay distinct")
	}
}
