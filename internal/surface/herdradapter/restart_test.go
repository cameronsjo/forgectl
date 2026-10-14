//go:build unix

package herdradapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// TestALedgerBoundAdapterSettlesARestartByTheMarker is forgectl#1077: after a
// herdr restart every ledger row read as an identity mismatch forever, and
// close could never clear it. A ledger-bound adapter knows the session is the
// same, so it answers from forgectl's ownership marker on the new server.
func TestALedgerBoundAdapterSettlesARestartByTheMarker(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		listing  func(marker string) []byte
		close    backend.CloseState
		probe    backend.ProbeState
		restored bool
		cleanup  bool
	}{
		{
			name:    "no workspace carries the marker: gone",
			listing: func(string) []byte { return listJSON() },
			close:   backend.CloseAlreadyGone, probe: backend.ProbeGone,
		},
		{
			// herdr's id counter restarts at 1, so the reference's id can name
			// a new workspace. It is not ours, and it is never closed.
			name:    "the id now names another workspace: gone, and it is left alone",
			listing: func(string) []byte { return listJSON([2]string{wsA, "fc-surface-somebodyelse"}) },
			close:   backend.CloseAlreadyGone, probe: backend.ProbeGone,
		},
		{
			// A session restore brings a workspace back under its saved id and
			// label, with fresh shells. It is ours, so close closes it.
			name:    "a restored workspace at the same id: found and closed",
			listing: func(m string) []byte { return listJSON([2]string{wsA, m}) },
			close:   backend.CloseClosed, probe: backend.ProbePresent, restored: true, cleanup: true,
		},
		{
			name:    "the marker at another id: a mismatch",
			listing: func(m string) []byte { return listJSON([2]string{wsB, m}) },
			close:   backend.CloseIdentityMismatch, probe: backend.ProbeIdentityMismatch,
		},
		{
			name:    "the marker on two workspaces: a mismatch",
			listing: func(m string) []byte { return listJSON([2]string{wsA, m}, [2]string{wsB, m}) },
			close:   backend.CloseIdentityMismatch, probe: backend.ProbeIdentityMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, ref := startClean(t)
			run := newRunner().reply1(exec.KindHerdrProbe, tc.listing(ref.OwnershipName()))
			a := newTestAdapter(t, run, nil, WithLstat(liveSocket(liveInode+1))).ForSessionLedger()

			probe := a.Probe(ctx, ref)
			if got := probe.State(); got != tc.probe {
				t.Errorf("Probe state = %v, want %v", got, tc.probe)
			}
			if probe.Restored() != tc.restored {
				t.Errorf("Probe restored = %v, want %v", probe.Restored(), tc.restored)
			}
			if got := a.Prober()(ctx, ref).State(); got != tc.probe {
				t.Errorf("Prober state = %v, want %v", got, tc.probe)
			}
			if got := a.Close(ctx, ref).State(); got != tc.close {
				t.Errorf("Close state = %v, want %v", got, tc.close)
			}
			cleanup, cleaned := commandOfKind(run.calls(), exec.KindHerdrCleanup)
			if cleaned != tc.cleanup {
				t.Fatalf("close issued = %v, want %v", cleaned, tc.cleanup)
			}
			if cleaned && !cleanup.Args[len(cleanup.Args)-1].Equal(exec.Opaque(wsA)) {
				t.Error("the close operand is not the reference's workspace id")
			}
		})
	}

	t.Run("an unreadable listing is unreadable, never gone", func(t *testing.T) {
		_, _, ref := startClean(t)
		run := newRunner().on(exec.KindHerdrProbe, func() (exec.SensitiveResult, error) {
			return exec.SensitiveResult{}, exec.SensitiveErrorForTest(exec.KindHerdrProbe, exec.OutcomeExit)
		})
		a := newTestAdapter(t, run, nil, WithLstat(liveSocket(liveInode+1))).ForSessionLedger()

		if got := a.Close(ctx, ref).State(); got.SatisfiesRollback() {
			t.Errorf("Close state = %v, want an unsatisfied rollback", got)
		}
		if got := a.Probe(ctx, ref).State(); got == backend.ProbeGone || got == backend.ProbePresent {
			t.Errorf("Probe state = %v, want an inconclusive answer", got)
		}
	})

	// The review's case: one session name, two herdr servers. herdr derives
	// the socket from its config directory, so a ledger row written from
	// another environment reaches a different, still-running server. That is
	// not a restart, and the worker may be alive on the other server.
	t.Run("a different endpoint is a mismatch, never gone", func(t *testing.T) {
		_, _, ref := startClean(t)
		run := newRunner().reply1(exec.KindHerdrProbe, listJSON())
		run.sessions = func() (exec.SensitiveResult, error) {
			return stdout(sessionsJSON(map[string]any{
				"default": true, "name": defaultSession, "running": true,
				"session_dir": "/tmp/herdrtest", "socket_path": otherSocket,
			})), nil
		}
		a := newTestAdapter(t, run, nil).ForSessionLedger()

		if got := a.Close(ctx, ref).State(); got != backend.CloseIdentityMismatch {
			t.Errorf("Close state = %v, want identity-mismatch", got)
		}
		if got := a.Probe(ctx, ref).State(); got != backend.ProbeIdentityMismatch {
			t.Errorf("Probe state = %v, want identity-mismatch", got)
		}
		if _, cleaned := commandOfKind(run.calls(), exec.KindHerdrCleanup); cleaned {
			t.Error("a close ran against another endpoint")
		}
	})

	t.Run("a reference that records no endpoint keeps the mismatch", func(t *testing.T) {
		_, _, ref := startClean(t)
		raw, err := json.Marshal(ref)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if _, ok := fields["endpoint"]; !ok {
			t.Fatal("a launch did not record its endpoint")
		}
		delete(fields, "endpoint")
		raw, err = json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		old, err := backend.DecodeRef(raw)
		if err != nil {
			t.Fatalf("DecodeRef: %v", err)
		}
		run := newRunner().reply1(exec.KindHerdrProbe, listJSON())
		a := newTestAdapter(t, run, nil, WithLstat(liveSocket(liveInode+1))).ForSessionLedger()

		if got := a.Close(ctx, old).State(); got != backend.CloseIdentityMismatch {
			t.Errorf("Close state = %v, want identity-mismatch", got)
		}
		if got := a.Probe(ctx, old).State(); got != backend.ProbeIdentityMismatch {
			t.Errorf("Probe state = %v, want identity-mismatch", got)
		}
	})

	t.Run("the original adapter still refuses a changed incarnation", func(t *testing.T) {
		_, _, ref := startClean(t)
		run := newRunner().reply1(exec.KindHerdrProbe, listJSON())
		plain := newTestAdapter(t, run, nil, WithLstat(liveSocket(liveInode+1)))
		_ = plain.ForSessionLedger()

		if got := plain.Close(ctx, ref).State(); got != backend.CloseIdentityMismatch {
			t.Errorf("Close state = %v, want identity-mismatch: ForSessionLedger must not change its receiver", got)
		}
	})
}

// TestThePaneReadersStayStrictAfterARestart: a workspace herdr restored after
// a restart runs a fresh shell, not the worker, so a ledger-bound adapter
// must still refuse to read or type into it.
func TestThePaneReadersStayStrictAfterARestart(t *testing.T) {
	ctx := context.Background()
	_, _, ref := startClean(t)
	run := newRunner().reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
	a := newTestAdapter(t, run, nil, WithLstat(liveSocket(liveInode+1))).ForSessionLedger()

	if _, err := a.WorkerScreen(ctx, ref); !errors.Is(err, ErrScreenUnreadable) {
		t.Errorf("WorkerScreen err = %v, want ErrScreenUnreadable", err)
	}
	if err := a.TypeText(ctx, ref, "hi"); !errors.Is(err, ErrScreenUnreadable) {
		t.Errorf("TypeText err = %v, want ErrScreenUnreadable", err)
	}
	for _, c := range run.calls() {
		if c.Kind == exec.KindHerdrScreenRead || c.Kind == exec.KindHerdrCleanup {
			t.Errorf("a %v command ran against a restarted server", c.Kind)
		}
	}
}

// TestCloseReprovesTheIncarnationAroundTheClose is forgectl#1045. herdr
// reuses workspace ids across a restart and closes by id alone, so a restart
// between the lookup and the close could close another workspace that now
// holds the id. The socket is stat'ed again on both sides of the close.
func TestCloseReprovesTheIncarnationAroundTheClose(t *testing.T) {
	ctx := context.Background()
	// flip returns a stat seam that answers before() until flipped, then after().
	flip := func(after func(string) (os.FileInfo, error)) (func(string) (os.FileInfo, error), func()) {
		flipped := false
		before := liveSocket(liveInode)
		return func(p string) (os.FileInfo, error) {
				if flipped {
					return after(p)
				}
				return before(p)
			}, func() {
				flipped = true
			}
	}

	t.Run("a restart after the lookup closes nothing", func(t *testing.T) {
		_, _, ref := startClean(t)
		seam, restart := flip(liveSocket(liveInode + 1))
		run := newRunner().on(exec.KindHerdrProbe, func() (exec.SensitiveResult, error) {
			restart() // the server restarts right after answering the listing
			return stdout(listJSON([2]string{wsA, ref.OwnershipName()})), nil
		})
		a := newTestAdapter(t, run, nil, WithLstat(seam)).ForSessionLedger()

		if got := a.Close(ctx, ref).State(); got != backend.CloseIdentityMismatch {
			t.Errorf("Close state = %v, want identity-mismatch", got)
		}
		if _, cleaned := commandOfKind(run.calls(), exec.KindHerdrCleanup); cleaned {
			t.Error("a close ran after the server restarted under it")
		}
	})

	// Absence needs a failed herdr call AND the stat; a stat alone keeps the
	// obligation.
	t.Run("a socket that vanished after the lookup closes nothing and keeps the obligation", func(t *testing.T) {
		_, _, ref := startClean(t)
		seam, quit := flip(absentSocket)
		run := newRunner().on(exec.KindHerdrProbe, func() (exec.SensitiveResult, error) {
			quit()
			return stdout(listJSON([2]string{wsA, ref.OwnershipName()})), nil
		})
		a := newTestAdapter(t, run, nil, WithLstat(seam))

		if got := a.Close(ctx, ref).State(); got != backend.CloseUnreadable {
			t.Errorf("Close state = %v, want unreadable", got)
		}
		if _, cleaned := commandOfKind(run.calls(), exec.KindHerdrCleanup); cleaned {
			t.Error("a close ran against a server that had quit")
		}
	})

	t.Run("a restart during the close is not a clean close", func(t *testing.T) {
		_, _, ref := startClean(t)
		seam, restart := flip(liveSocket(liveInode + 1))
		run := newRunner().reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
		run.on(exec.KindHerdrCleanup, func() (exec.SensitiveResult, error) {
			restart()
			return exec.SensitiveResult{}, nil
		})
		a := newTestAdapter(t, run, nil, WithLstat(seam))

		if got := a.Close(ctx, ref).State(); got.SatisfiesRollback() {
			t.Errorf("Close state = %v, want an unsatisfied rollback", got)
		}
	})

	t.Run("an unchanged server closes", func(t *testing.T) {
		_, _, ref := startClean(t)
		run := newRunner().reply1(exec.KindHerdrProbe, listJSON([2]string{wsA, ref.OwnershipName()}))
		a := newTestAdapter(t, run, nil)

		if got := a.Close(ctx, ref).State(); got != backend.CloseClosed {
			t.Errorf("Close state = %v, want closed", got)
		}
	})
}
