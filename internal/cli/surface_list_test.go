//go:build unix

package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
			got := reconcileRow(ctx, fakeProber{c.probe}, c.row(t), top, listed, time.Unix(1e9, 0))
			if got.State != c.state || got.Orphan != c.orphan {
				t.Fatalf("state %q orphan %v, want %q %v (reason %q)", got.State, got.Orphan, c.state, c.orphan, got.Reason)
			}
			if !got.WorktreePresent {
				t.Fatal("the listed worktree reads as absent")
			}
		})
	}

	// A workspace herdr restored after a restart is present, but the worker
	// in it did not survive, and list says so (forgectl#1077).
	t.Run("a restored workspace is present and says the worker is gone", func(t *testing.T) {
		got := reconcileRow(ctx, fakeProber{backend.NewProbePresentRestored()}, launchedRow(t), top, listed, time.Unix(1e9, 0))
		if got.State != workerPresent || !strings.Contains(got.Reason, "did not survive") {
			t.Fatalf("state %q reason %q", got.State, got.Reason)
		}
		plain := reconcileRow(ctx, fakeProber{backend.NewProbePresent()}, launchedRow(t), top, listed, time.Unix(1e9, 0))
		if plain.Reason != "" {
			t.Fatalf("a plain present row carries a reason: %q", plain.Reason)
		}
	})

	t.Run("the reference's ids and the session ride along", func(t *testing.T) {
		row := launchedRow(t)
		row.SessionID, row.Transcript = "0f8e2c1a-3b4d-4e5f-8a6b-7c8d9e0f1a2b", "/h/.claude/projects/-r/x.jsonl"
		got := reconcileRow(ctx, fakeProber{backend.NewProbePresent()}, row, top, listed, time.Unix(1e9, 0))
		if got.WorkspaceID == "" || got.PaneID == "" || got.SessionID != row.SessionID || got.Transcript != row.Transcript {
			t.Fatalf("row %+v", got)
		}
	})
}

func TestReconcileRowOrphans(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1e9, 0)
	listed := map[string]bool{}
	closed := launchedRow(t)
	closed.Stage = worker.StageClosed
	cases := map[string]struct {
		row    worker.Row
		probe  backend.ProbeResult
		orphan bool
	}{
		"a closed row is an orphan even while herdr reads it present": {closed, backend.NewProbePresent(), true},
		"a young pending launch is not an orphan":                     {worker.Row{Name: "w", Stage: worker.StagePending, StartedAt: now.Add(-time.Minute)}, backend.NewProbePresent(), false},
		"a stale pending launch is an orphan":                         {worker.Row{Name: "w", Stage: worker.StagePending, StartedAt: now.Add(-time.Hour)}, backend.NewProbePresent(), true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := reconcileRow(ctx, fakeProber{c.probe}, c.row, "/r", listed, now); got.Orphan != c.orphan {
				t.Fatalf("orphan %v, want %v (state %q)", got.Orphan, c.orphan, got.State)
			}
		})
	}
}
