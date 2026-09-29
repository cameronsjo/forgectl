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

// tmuxBudget is a pool of tmux time that one or more calls draw from. Each
// bound() hands out a context limited to what is LEFT, and its done func
// charges the time actually spent. Only tmux time is charged, never the work a
// caller does between calls, so a long sweep of healthy calls does not run
// dry on wall-clock time it spent elsewhere.
//
// A single call site takes a fresh budget through boundedTmux. A sweep that
// makes tmux calls in a loop under one lock hold (Cleanup) shares ONE budget
// across the whole loop, so a hung server costs the sweep one budget rather
// than one per iteration.
//
// Not safe for concurrent use; every holder is a single goroutine under the
// lifecycle lock.
type tmuxBudget struct {
	remaining time.Duration
	// cutOff is set once any bounded call ended with its context done — the
	// budget ran out, or the caller cancelled. Either way tmux did not answer
	// in time, and a sweep stops asking it.
	cutOff bool
}

func newTmuxBudget() *tmuxBudget { return &tmuxBudget{remaining: lockedTmuxBudget} }

// bound returns ctx limited to the remaining budget. The returned done func
// must be called once the tmux work is over: it charges the elapsed time and
// releases the context.
func (b *tmuxBudget) bound(ctx context.Context) (context.Context, func()) {
	start := time.Now()
	bctx, cancel := context.WithTimeout(ctx, b.remaining)
	return bctx, func() {
		b.remaining -= time.Since(start)
		if b.remaining < 0 {
			b.remaining = 0
		}
		if bctx.Err() != nil {
			b.cutOff = true
		}
		cancel()
	}
}

// exhausted reports whether a caller should stop asking tmux at all: a call
// was already cut off, or no time is left to give the next one.
func (b *tmuxBudget) exhausted() bool { return b.cutOff || b.remaining <= 0 }

// boundedTmux bounds one call site's tmux work with a fresh lockedTmuxBudget.
func boundedTmux(ctx context.Context) (context.Context, func()) {
	return newTmuxBudget().bound(ctx)
}
