package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

type fakeProber struct{ result backend.ProbeResult }

func (p fakeProber) Probe(context.Context, backend.Ref) backend.ProbeResult { return p.result }

func TestReconcileRow(t *testing.T) {
	ctx := context.Background()
	const top = "/r"
	listed := map[string]bool{worker.WorktreePath(top, "w"): true}
	unreadable := backend.NewProbeUnreadable(backend.NewStartCause(backend.FailureUnavailable, errors.New("protocol_mismatch")))
	mismatch := backend.NewProbeIdentityMismatch(backend.NewStartCause(backend.FailureIdentityMismatch, errors.New("restarted")))

	cases := map[string]struct {
		row    func(*testing.T) worker.Row
		probe  backend.ProbeResult
		state  string
		orphan bool
	}{
		"present":                            {launchedRow, backend.NewProbePresent(), workerPresent, false},
		"gone is an orphan":                  {launchedRow, backend.NewProbeGone(), workerGone, true},
		"a herdr error is unreadable":        {launchedRow, unreadable, workerUnreadable, false},
		"an identity mismatch is unreadable": {launchedRow, mismatch, workerUnreadable, false},
		"a launch that never finished is an orphan": {
			func(*testing.T) worker.Row { return worker.Row{Name: "w", Stage: worker.StageFailed} },
			backend.NewProbePresent(), workerGone, true,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := reconcileRow(ctx, fakeProber{c.probe}, c.row(t), top, listed)
			if got.State != c.state || got.Orphan != c.orphan {
				t.Fatalf("state %q orphan %v, want %q %v (reason %q)", got.State, got.Orphan, c.state, c.orphan, got.Reason)
			}
			if !got.WorktreePresent {
				t.Fatal("the listed worktree reads as absent")
			}
		})
	}

	t.Run("the reference's ids and the session ride along", func(t *testing.T) {
		row := launchedRow(t)
		row.SessionID, row.Transcript = "0f8e2c1a-3b4d-4e5f-8a6b-7c8d9e0f1a2b", "/h/.claude/projects/-r/x.jsonl"
		got := reconcileRow(ctx, fakeProber{backend.NewProbePresent()}, row, top, listed)
		if got.WorkspaceID == "" || got.PaneID == "" || got.SessionID != row.SessionID || got.Transcript != row.Transcript {
			t.Fatalf("row %+v", got)
		}
	})
}
