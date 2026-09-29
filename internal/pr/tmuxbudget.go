package pr

import (
	"context"
	"time"
)

// lockedTmuxBudget bounds the tmux work a lifecycle-lock holder does, so a hung
// tmux server cannot hold the lock until the operator kills the process
// (forgectl#556, forgectl#656). Every tmux call made under the lock goes
// through a tmuxBudget; the sites are listed in the lock contract on
// withLifecycleLock (lifecycle_unix.go).
//
// 3 s: a healthy tmux answers each call in single-digit milliseconds, so this
// is three orders of magnitude of headroom for a loaded machine, while staying
// well under defaultLockWait (10 s) — a verb queued behind a hung holder still
// acquires the lock once the budget lapses, instead of timing out with it.
//
// The deadline only bounds the tmux CLIENT process's lifetime if the runner
// also stops waiting on pipes a grandchild inherited: exec's pipeWaitDelay adds
// up to 500 ms on top for that. It is a var only so a test can shrink it.
var lockedTmuxBudget = 3 * time.Second

// tmuxBudget hands every bounded unit of tmux work — one teardown's
// resolve-and-kill, one liveness or occupancy read — its OWN full
// lockedTmuxBudget deadline, and remembers whether any of them was cut off.
// It detects an UNRESPONSIVE tmux; it does not cap total time. A slow but
// healthy tmux therefore never runs a sweep down: every unit that answers
// inside the bound leaves the next one a full bound of its own.
//
// A single call site takes a fresh one through boundedTmux. A sweep that makes
// tmux calls in a loop under one lock hold (Cleanup) shares ONE across the
// loop so that, once a call has actually timed out, it stops asking tmux for
// the rest of the sweep. The lock hold is then at most the cost of the
// sessions before the first timeout plus that one timeout, rather than one
// timeout per session.
//
// Not safe for concurrent use; every holder is a single goroutine under the
// lifecycle lock.
type tmuxBudget struct {
	// cutOff is set once any bounded call ended with its context done — the
	// deadline passed, or the caller cancelled. Either way tmux did not answer
	// in time, and a sweep stops asking it.
	cutOff bool
}

func newTmuxBudget() *tmuxBudget { return &tmuxBudget{} }

// bound returns ctx limited to one full lockedTmuxBudget. The returned done
// func must be called once the tmux work is over: it records whether the
// deadline (or the caller) cut the work off, and releases the context.
func (b *tmuxBudget) bound(ctx context.Context) (context.Context, func()) {
	bctx, cancel := context.WithTimeout(ctx, lockedTmuxBudget)
	return bctx, func() {
		if bctx.Err() != nil {
			b.cutOff = true
		}
		cancel()
	}
}

// exhausted reports whether a caller should stop asking tmux at all: an
// earlier call under this budget was already cut off.
func (b *tmuxBudget) exhausted() bool { return b.cutOff }

// boundedTmux bounds one call site's tmux work with a fresh lockedTmuxBudget.
func boundedTmux(ctx context.Context) (context.Context, func()) {
	return newTmuxBudget().bound(ctx)
}
