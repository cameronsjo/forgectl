package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/module"
)

// Inside a drain worker, surface enqueue, surface launch and surface merge
// refuse before doing anything, as intake does: a worker must not start
// another worker, or merge a PR, by accident (security review of
// cameronsjo/forgectl#1203; independent review of cameronsjo/forgectl#1212).
func TestWorkerStartingCommandsRefuseInADrainWorker(t *testing.T) {
	t.Setenv(launch.DrainWorkerEnv, "1")
	cmd := &cobra.Command{}
	for name, run := range map[string]func() error{
		"surface enqueue": func() error {
			return runSurfaceEnqueue(cmd, module.Deps{}, enqueueOptions{Repo: "/nonexistent", Name: "x", Brief: "/nonexistent"})
		},
		"surface launch": func() error {
			return runSurfaceLaunch(cmd, module.Deps{}, surfaceLaunchOptions{Backend: "herdr"})
		},
		"surface merge": func() error {
			return runSurfaceMerge(newSurfaceMergeCmd(module.Deps{}), mergeDeps{}, mergeOptions{Name: "x"})
		},
	} {
		err := run()
		if err == nil || ExitCode(err) != exitUsage || !strings.Contains(err.Error(), name+" refuses to run inside a drain worker") {
			t.Errorf("%s: err %v (exit %d), want the drain-worker refusal, exit 2", name, err, ExitCode(err))
		}
	}
}

func TestRefuseInDrainWorkerIgnoresAnUnsetMarker(t *testing.T) {
	if err := refuseInDrainWorker(func(string) string { return "" }, "surface enqueue"); err != nil {
		t.Fatalf("unset marker refused: %v", err)
	}
}
