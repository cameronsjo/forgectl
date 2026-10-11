package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/module"
)

// Inside a drain worker, surface enqueue, surface launch, surface brief,
// surface merge and the drain's start, stop and process refuse before doing
// anything, as intake does: a worker must not start or drive another worker,
// or merge a PR, by accident (security review of cameronsjo/forgectl#1203;
// independent review of cameronsjo/forgectl#1212; cameronsjo/forgectl#1205).
func TestWorkerStartingCommandsRefuseInADrainWorker(t *testing.T) {
	t.Setenv(launch.DrainWorkerEnv, "1")
	// A scratch state dir: if a refusal were ever missing, the command must
	// not reach the operator's real queue or drain (drain stop would signal
	// a live drain).
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cmd := &cobra.Command{}
	for name, run := range map[string]func() error{
		"surface enqueue": func() error {
			return runSurfaceEnqueue(cmd, module.Deps{}, enqueueOptions{Repo: "/nonexistent", Name: "x", Brief: "/nonexistent"})
		},
		"surface launch": func() error {
			return runSurfaceLaunch(cmd, module.Deps{}, surfaceLaunchOptions{Backend: "herdr"})
		},
		// A worker must not type into a sibling worker (cameronsjo/forgectl#1205).
		"surface brief": func() error {
			return runSurfaceBrief(cmd, module.Deps{}, briefOptions{Repo: "/nonexistent", Name: "x", Text: "hi", Readback: time.Second, Start: time.Second})
		},
		// Nor close or dequeue a sibling (cameronsjo/forgectl#1215).
		"surface close": func() error {
			return runSurfaceClose(cmd, module.Deps{}, closeOptions{Repo: "/nonexistent", Name: "x"})
		},
		"surface dequeue": func() error {
			return runSurfaceDequeue(cmd, "x", false)
		},
		"surface merge": func() error {
			return runSurfaceMerge(newSurfaceMergeCmd(module.Deps{}), mergeDeps{}, mergeOptions{Name: "x"})
		},
		// The kill switch: a worker must not restart a drain the operator
		// stopped (independent review of cameronsjo/forgectl#1212).
		"surface drain start": func() error {
			return runSurfaceDrainStart(cmd, module.Deps{}, false)
		},
		"surface drain stop": func() error {
			return runSurfaceDrainStop(cmd, false)
		},
		"surface _drain": func() error {
			c := newSurfaceDrainProcessCmd(module.Deps{})
			return c.RunE(c, nil)
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

// TestTasksDoneRefusesInADrainWorker pins cameronsjo/forgectl#1226: a drain
// worker cannot close a board task. It refuses before the host rule, the
// keychain read or any request, with exit 1 (the tasks verbs give 2 to an
// unreachable instance). State, config and home are scratch directories, and
// the host is unroutable, so a missing refusal cannot reach a real board.
func TestTasksDoneRefusesInADrainWorker(t *testing.T) {
	t.Setenv(launch.DrainWorkerEnv, "1")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	host := "board.example.invalid"
	cmd := newTasksDoneCmd(module.Deps{}, &host)
	cmd.SetArgs([]string{"42", "--evidence", "merged owner/repo#1"})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	if err == nil || ExitCode(err) != exitFailed || !strings.Contains(err.Error(), "tasks done refuses to run inside a drain worker") {
		t.Fatalf("err %v (exit %d), want the drain-worker refusal, exit 1", err, ExitCode(err))
	}
	if out.Len() != 0 {
		t.Fatalf("a refused call wrote %q", out.String())
	}
}
