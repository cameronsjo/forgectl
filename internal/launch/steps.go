package launch

import (
	"context"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/step"
)

// Steps is launch's workflow step contribution (ADR-0005): the `launch` verb,
// still the spike-scoped stub (full clean-room execution is the follow-on per
// the design doc). It exists here — rather than as a workflow builtin — to
// prove the module→data-plane seam with a second contributor alongside
// quarantine's strip.
func Steps() step.Registry {
	return step.Registry{
		"launch": {Runner: launchStepStub, Exports: []string{"review"}},
	}
}

// launchStepStub backs the registered-but-unimplemented launch verb.
func launchStepStub(context.Context, exec.Runner, *step.Context, step.PlanStep) error {
	return step.ErrNotYetWired
}
