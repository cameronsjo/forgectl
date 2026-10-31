package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/hcadapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

func launchedHcRow(t *testing.T) worker.Row {
	t.Helper()
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		t.Fatal(err)
	}
	server, err := backend.FingerprintHcCore("default", "core1")
	if err != nil {
		t.Fatal(err)
	}
	id, err := backend.NewHcIdentity("t7", 1793409108596)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := backend.NewHcRef(backend.HcDefaultSessionServer(), server, tag, id)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := ref.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return worker.Row{Name: "w", Branch: "feat", Stage: worker.StageLaunched, Ref: encoded, StartedAt: time.Unix(1e9, 0),
		HcSelection: "00112233445566778899aabbccddeeff"}
}

// TestForgetWorker is forgectl#1386: forget drops a row only when hc proves
// its terminal gone, and every other answer, or a row close settles itself,
// leaves the row in place. hc is never asked for a row the row alone refuses.
func TestForgetWorker(t *testing.T) {
	now := time.Unix(2e9, 0)
	hcRow := launchedHcRow(t)
	for name, tc := range map[string]struct {
		row       func() worker.Row
		abandoned func() (string, error)
		removeErr error
		forgotten bool
		asked     bool
		reason    string
	}{
		"core wiped": {row: func() worker.Row { return hcRow }, abandoned: func() (string, error) { return "the hc core is not the one", nil },
			forgotten: true, asked: true, reason: "hc core is not"},
		"still there": {row: func() worker.Row { return hcRow }, abandoned: func() (string, error) { return "", hcadapter.ErrStillThere },
			asked: true, reason: "surface close"},
		"selection unproven": {row: func() worker.Row { return hcRow }, abandoned: func() (string, error) { return "", hcadapter.ErrSelectionUnproven },
			asked: true, reason: "run surface close under the environment"},
		"unreadable": {row: func() worker.Row { return hcRow }, abandoned: func() (string, error) { return "", hcadapter.ErrUnreadable },
			asked: true, reason: "could not prove"},
		"row changed": {row: func() worker.Row { return hcRow }, abandoned: func() (string, error) { return "gone", nil }, removeErr: worker.ErrRowChanged,
			asked: true, reason: "changed or went away"},
		"no ref": {row: func() worker.Row { r := hcRow; r.Ref = nil; r.Stage = worker.StageFailed; return r }, reason: "records no terminal"},
		"closed": {row: func() worker.Row { r := hcRow; r.Stage = worker.StageClosed; return r }, reason: "earlier close"},
		"in flight": {row: func() worker.Row {
			r := hcRow
			r.Ref, r.Stage, r.StartedAt = nil, worker.StageWorktree, now.Add(-time.Minute)
			return r
		}, reason: "may still be running"},
		"herdr ref": {row: func() worker.Row { return launchedRow(t) }, reason: "not an hc terminal"},
	} {
		t.Run(name, func(t *testing.T) {
			asked, removed := false, false
			res := forgetWorker(context.Background(), tc.row(), now, forgetSteps{
				abandoned: func(_ context.Context, _ backend.Ref, launched string) (string, error) {
					asked = true
					if launched != hcRow.HcSelection {
						t.Errorf("abandoned was handed selection %q, want the row's %q", launched, hcRow.HcSelection)
					}
					if tc.abandoned == nil {
						return "", errors.New("unscripted")
					}
					return tc.abandoned()
				},
				remove: func() error { removed = true; return tc.removeErr },
			})
			if res.Forgotten != tc.forgotten || asked != tc.asked || !strings.Contains(res.Reason, tc.reason) {
				t.Fatalf("forgotten %v asked %v reason %q; want forgotten %v asked %v reason containing %q", res.Forgotten, asked, res.Reason, tc.forgotten, tc.asked, tc.reason)
			}
			if !tc.asked && removed {
				t.Fatal("a row refused before hc was asked was removed")
			}
			if tc.abandoned != nil {
				if _, err := tc.abandoned(); err != nil && removed {
					t.Fatal("a row hc did not prove gone was removed")
				}
			}
		})
	}
}

// TestForgetIsHcOnly: forget refuses herdr with a usage error before it
// opens a ledger.
func TestForgetIsHcOnly(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := runSurfaceForget(cmd, module.Deps{}, forgetOptions{Surface: workerSurfaceHerdr, Name: "w"})
	if code := ExitCode(err); code != exitUsage || !errors.Is(err, errForgetHcOnly) {
		t.Fatalf("exit %d (err %v), want 2 and errForgetHcOnly", code, err)
	}
}

// TestHerdrSpaceNeedsAnHcWorker: --herdr-space labels an hc terminal for the
// herdr-app window, so a herdr worker or a launch that is not a worker
// refuses it as a usage error before anything is created.
func TestHerdrSpaceNeedsAnHcWorker(t *testing.T) {
	for name, opts := range map[string]surfaceLaunchOptions{
		"herdr worker": {Backend: "herdr", Worktree: "feat/x", DisplayName: "x", HerdrSpace: "sp1"},
		"not a worker": {Backend: "hc", HerdrSpace: "sp1"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			err := runSurfaceLaunch(cmd, module.Deps{}, opts)
			if code := ExitCode(err); code != exitUsage || err == nil || !strings.Contains(err.Error(), "--herdr-space") {
				t.Fatalf("exit code = %d (err %v), want 2 naming --herdr-space", code, err)
			}
		})
	}
}
