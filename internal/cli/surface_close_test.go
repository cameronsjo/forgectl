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

// fakeClose records what close's steps were asked to do.
type fakeClose struct {
	result    backend.CloseResult
	facts     worker.WorktreeFacts
	inspErr   error
	removeErr error
	// markErr and forgetErr are what the ledger writes return.
	markErr   error
	forgetErr error
	calls     []string
}

func (f *fakeClose) steps() closeSteps {
	return closeSteps{
		close: func(context.Context, backend.Ref) backend.CloseResult {
			f.calls = append(f.calls, "close")
			return f.result
		},
		inspect: func(context.Context) (worker.WorktreeFacts, error) {
			f.calls = append(f.calls, "inspect")
			return f.facts, f.inspErr
		},
		remove: func(context.Context, string) error {
			f.calls = append(f.calls, "remove")
			return f.removeErr
		},
		forget:     func() error { f.calls = append(f.calls, "forget"); return f.forgetErr },
		markClosed: func(time.Time) error { f.calls = append(f.calls, "mark-closed"); return f.markErr },
	}
}

func launchedRow(t *testing.T) worker.Row {
	t.Helper()
	encoded, err := testHerdrRef(t).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return worker.Row{Name: "w", Branch: "feat", Stage: worker.StageLaunched, Ref: encoded}
}

var cleanFacts = worker.WorktreeFacts{Path: "/r/.claude/worktrees/w", Listed: true, OnDisk: true, Branch: "feat"}

func TestCloseWorker(t *testing.T) {
	ctx := context.Background()
	unreadable := backend.NewCloseUnreadable(backend.NewStartCause(backend.FailureUnavailable, errors.New("protocol_mismatch")))
	mismatch := backend.NewCloseIdentityMismatch(backend.NewStartCause(backend.FailureIdentityMismatch, errors.New("restarted")))

	cases := map[string]struct {
		row   func(*testing.T) worker.Row
		fake  fakeClose
		keep  bool
		want  closeResult
		calls string
	}{
		"closes, removes a clean worktree, forgets the row": {
			row: launchedRow, fake: fakeClose{result: backend.NewCloseClosed(), facts: cleanFacts},
			want:  closeResult{Closed: true, Workspace: closeWorkspaceClosed, Worktree: closeWorktreeRemoved, Forgotten: true},
			calls: "close inspect remove forget",
		},
		"refuses on unreadable herdr and touches nothing else": {
			row: launchedRow, fake: fakeClose{result: unreadable, facts: cleanFacts},
			want:  closeResult{Workspace: closeWorkspaceRefused, Worktree: closeWorktreeUntouched},
			calls: "close",
		},
		"refuses on an identity mismatch": {
			row: launchedRow, fake: fakeClose{result: mismatch, facts: cleanFacts},
			want:  closeResult{Workspace: closeWorkspaceRefused, Worktree: closeWorktreeUntouched},
			calls: "close",
		},
		"keeps a worktree a check blocks, and marks the row closed": {
			row: launchedRow, fake: fakeClose{result: backend.NewCloseAlreadyGone(), facts: func() worker.WorktreeFacts {
				f := cleanFacts
				f.Stashes = 1
				return f
			}()},
			want:  closeResult{Closed: true, Workspace: closeWorkspaceGone, Worktree: closeWorktreeKept},
			calls: "close inspect mark-closed",
		},
		"--keep-worktree never inspects": {
			row: launchedRow, fake: fakeClose{result: backend.NewCloseClosed(), facts: cleanFacts}, keep: true,
			want:  closeResult{Closed: true, Workspace: closeWorkspaceClosed, Worktree: closeWorktreeKept},
			calls: "close mark-closed",
		},
		"an unreadable git keeps the worktree": {
			row: launchedRow, fake: fakeClose{result: backend.NewCloseClosed(), inspErr: errors.New("git broke")},
			want:  closeResult{Closed: true, Workspace: closeWorkspaceClosed, Worktree: closeWorktreeKept},
			calls: "close inspect mark-closed",
		},
		"a failed remove keeps the row": {
			row: launchedRow, fake: fakeClose{result: backend.NewCloseClosed(), facts: cleanFacts, removeErr: errors.New("contains modified files")},
			want:  closeResult{Closed: true, Workspace: closeWorkspaceClosed, Worktree: closeWorktreeKept},
			calls: "close inspect remove mark-closed",
		},
		"a launch that died before its workspace: no herdr call": {
			row: func(*testing.T) worker.Row {
				return worker.Row{Name: "w", Branch: "feat", Stage: worker.StageFailed, Worktree: "/r/.claude/worktrees/w"}
			},
			fake:  fakeClose{facts: cleanFacts},
			want:  closeResult{Closed: true, Workspace: closeWorkspaceNone, Worktree: closeWorktreeRemoved, Forgotten: true},
			calls: "inspect remove forget",
		},
		"no worktree left: the row is forgotten": {
			row: launchedRow, fake: fakeClose{result: backend.NewCloseClosed()},
			want:  closeResult{Closed: true, Workspace: closeWorkspaceClosed, Worktree: closeWorktreeGone, Forgotten: true},
			calls: "close inspect forget",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := c.fake
			got := closeWorker(ctx, c.row(t), c.keep, time.Unix(1e9, 0), f.steps())
			if got.Closed != c.want.Closed || got.Workspace != c.want.Workspace || got.Worktree != c.want.Worktree || got.Forgotten != c.want.Forgotten {
				t.Fatalf("result %+v, want %+v", got, c.want)
			}
			if calls := strings.Join(f.calls, " "); calls != c.calls {
				t.Fatalf("calls %q, want %q", calls, c.calls)
			}
			if got.Worktree == closeWorktreeKept && len(got.KeptBecause) == 0 {
				t.Fatal("a kept worktree names no reason")
			}
		})
	}

	t.Run("a recovery tag is named in a note", func(t *testing.T) {
		f := fakeClose{facts: cleanFacts}
		row := worker.Row{Name: "w", Stage: worker.StageFailed, Recovery: "forgectl-abc"}
		if got := closeWorker(ctx, row, false, time.Unix(1e9, 0), f.steps()); !strings.Contains(got.Note, "forgectl-abc") {
			t.Fatalf("note %q does not name the recovery label", got.Note)
		}
	})
}

func TestCloseWorkerGuards(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1e9, 0)

	t.Run("a launch inside its settle window is refused untouched", func(t *testing.T) {
		for _, stage := range []worker.Stage{worker.StagePending, worker.StageWorktree} {
			f := fakeClose{facts: cleanFacts}
			row := worker.Row{Name: "w", Stage: stage, StartedAt: now.Add(-time.Minute)}
			got := closeWorker(ctx, row, false, now, f.steps())
			if got.Closed || len(f.calls) != 0 || !strings.Contains(got.Reason, "may still be running") {
				t.Fatalf("stage %s: result %+v, calls %q", stage, got, f.calls)
			}
		}
	})

	t.Run("a launch past its settle window is closed", func(t *testing.T) {
		f := fakeClose{facts: cleanFacts}
		row := worker.Row{Name: "w", Stage: worker.StageWorktree, StartedAt: now.Add(-launchSettleAfter - time.Second)}
		if got := closeWorker(ctx, row, false, now, f.steps()); !got.Closed || !got.Forgotten {
			t.Fatalf("result %+v", got)
		}
	})

	t.Run("a row an earlier close kept is retried without herdr", func(t *testing.T) {
		row := launchedRow(t)
		row.Stage = worker.StageClosed
		f := fakeClose{result: backend.NewCloseIdentityMismatch(backend.NewStartCause(backend.FailureIdentityMismatch, errors.New("restarted"))), facts: cleanFacts}
		got := closeWorker(ctx, row, false, now, f.steps())
		if !got.Closed || got.Workspace != closeWorkspaceEarlier || strings.Join(f.calls, " ") != "inspect remove forget" {
			t.Fatalf("result %+v, calls %q", got, f.calls)
		}
	})

	t.Run("each refusal names its own cause", func(t *testing.T) {
		for want, result := range map[string]backend.CloseResult{
			"herdr restarted":   backend.NewCloseIdentityMismatch(backend.NewStartCause(backend.FailureIdentityMismatch, errors.New("x"))),
			"could not be read": backend.NewCloseUnreadable(backend.NewStartCause(backend.FailureUnavailable, errors.New("x"))),
			"did not close":     backend.NewCloseFailed(backend.NewStartCause(backend.FailureUnavailable, errors.New("x"))),
		} {
			f := fakeClose{result: result}
			if got := closeWorker(ctx, launchedRow(t), false, now, f.steps()); got.Closed || !strings.Contains(got.Reason, want) {
				t.Fatalf("reason %q, want it to contain %q", got.Reason, want)
			}
		}
		row := launchedRow(t)
		row.Ref = []byte(`{"kind":"nope"}`)
		f := fakeClose{}
		if got := closeWorker(ctx, row, false, now, f.steps()); got.Closed || !strings.Contains(got.Reason, "does not decode") || len(f.calls) != 0 {
			t.Fatalf("result %+v, calls %q", got, f.calls)
		}
	})

	t.Run("a row already removed by another close counts as forgotten", func(t *testing.T) {
		f := fakeClose{result: backend.NewCloseClosed(), facts: cleanFacts}
		steps := f.steps()
		steps.forget = func() error { return worker.ErrNoRow }
		if got := closeWorker(ctx, launchedRow(t), false, now, steps); !got.Forgotten || got.Note != "" {
			t.Fatalf("result %+v", got)
		}
	})
}

// noCloser is a backend.Closer markClosed never calls.
type noCloser struct{}

func (noCloser) Close(context.Context, backend.Ref) backend.CloseResult { return backend.CloseResult{} }

// TestRealCloseStepsMarkClosedRecordsClosedAt pins that the close that
// keeps a worktree records closed_at once: a later close that keeps it
// again leaves the first time, so prune ages the row from its first close.
func TestRealCloseStepsMarkClosedRecordsClosedAt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const repo = "/repo/closed-at"
	led, err := worker.Open(repo, "s")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if err := led.Begin(worker.Row{Name: "w", Branch: "worker/w", StartedAt: started}); err != nil {
		t.Fatal(err)
	}
	if err := led.Update("w", func(r *worker.Row) { r.Stage = worker.StageLaunched }); err != nil {
		t.Fatal(err)
	}
	read := func() worker.Row {
		rows, err := led.Rows()
		if err != nil || len(rows) != 1 {
			t.Fatalf("rows %+v, %v", rows, err)
		}
		return rows[0]
	}
	first := time.Date(2026, 10, 9, 12, 30, 15, 500, time.UTC)
	if err := realCloseSteps(nil, noCloser{}, led, repo, read()).markClosed(first); err != nil {
		t.Fatal(err)
	}
	r := read()
	if r.Stage != worker.StageClosed || r.ClosedAt == nil || !r.ClosedAt.Equal(first.Truncate(time.Second)) {
		t.Fatalf("after the first close: stage %s closed_at %v", r.Stage, r.ClosedAt)
	}
	if err := realCloseSteps(nil, noCloser{}, led, repo, r).markClosed(first.Add(48 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if again := read(); again.ClosedAt == nil || !again.ClosedAt.Equal(first.Truncate(time.Second)) {
		t.Fatalf("a second close moved closed_at to %v", again.ClosedAt)
	}
}

// TestCloseWorkerLedgerWriteFailure pins that a close whose ledger write
// fails says so in its note and marks the result, which the drain's closers
// read to leave the queue row reported.
func TestCloseWorkerLedgerWriteFailure(t *testing.T) {
	stashed := cleanFacts
	stashed.Stashes = 1
	for name, fake := range map[string]fakeClose{
		"mark closed fails": {result: backend.NewCloseClosed(), facts: stashed, markErr: errors.New("disk full")},
		"forget fails":      {result: backend.NewCloseClosed(), facts: cleanFacts, forgetErr: errors.New("disk full")},
	} {
		t.Run(name, func(t *testing.T) {
			res := closeWorker(context.Background(), launchedRow(t), false, time.Now(), fake.steps())
			if !res.Closed || !res.ledgerFailed || !strings.Contains(res.Note, "disk full") || res.Forgotten {
				t.Fatalf("result %+v; want closed, ledgerFailed, the error in the note, not forgotten", res)
			}
		})
	}
	ok := fakeClose{result: backend.NewCloseClosed(), facts: cleanFacts}
	if res := closeWorker(context.Background(), launchedRow(t), false, time.Now(), ok.steps()); res.ledgerFailed {
		t.Fatalf("a clean close reported a ledger failure: %+v", res)
	}
}

// A relaunch under the same name after close read the row (here, between
// steps) refuses inspect and remove before either touches the worktree,
// which is found by name and would be the new launch's.
func TestRealCloseStepsRefuseARelaunchedRow(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	const repo = "/repo/relaunch"
	led, err := worker.Open(repo, "s")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if err := led.Begin(worker.Row{Name: "w", Branch: "worker/w", StartedAt: started}); err != nil {
		t.Fatal(err)
	}
	rows, err := led.Rows()
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows %+v, %v", rows, err)
	}
	steps := realCloseSteps(nil, noCloser{}, led, repo, rows[0])
	// The old row is closed and forgotten, and the name launched again.
	if err := led.RemoveIf("w", worker.SameRow(rows[0])); err != nil {
		t.Fatal(err)
	}
	if err := led.Begin(worker.Row{Name: "w", Branch: "worker/w", StartedAt: started.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := steps.inspect(t.Context()); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("inspect after a relaunch: %v, want a refusal", err)
	}
	if err := steps.remove(t.Context(), "/repo/relaunch/.claude/worktrees/w"); err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("remove after a relaunch: %v, want a refusal", err)
	}
}
