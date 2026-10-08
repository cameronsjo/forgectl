//go:build !unix

package cli

import (
	"context"
	"errors"

	"github.com/cameronsjo/forgectl/internal/module"
)

// errDrainUnsupported refuses the drain where its files and process control
// (openat, flock, setsid, signals) do not exist.
var errDrainUnsupported = errors.New("surface drain needs a unix system (darwin or linux)")

var drainSpawn = func([]string, []string) (int, error) { return 0, errDrainUnsupported }

var drainSignal = func(int) error { return errDrainUnsupported }

// drainRunDirCheck passes here so start reports errDrainUnsupported, not a
// run-directory refusal that would send the operator to TMPDIR.
var drainRunDirCheck = func() error { return nil }

func runDrainProcess(context.Context, module.Deps, string, string) error {
	return WithExitCode(errDrainUnsupported, exitUsage)
}
