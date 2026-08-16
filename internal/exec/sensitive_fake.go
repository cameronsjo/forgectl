package exec

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// FakeSensitiveRunner is the test double for SensitiveRunner. It records every
// SensitiveCommand exactly as constructed — opaque values stay opaque — so an
// adapter test asserts command construction by building the expected
// SensitiveCommand with the same constructors and comparing with Equal, never
// by reading an argument back out as a string. That is deliberate: a reveal
// accessor on the fake would be a reveal accessor in the production API
// surface, reachable from any package that imports exec.
type FakeSensitiveRunner struct {
	// RunFunc produces the result for a call. If nil, every call returns an
	// empty successful result. It receives the command so a test can branch on
	// Kind or argument count; it cannot read an argument's payload.
	RunFunc func(cmd SensitiveCommand) (SensitiveResult, error)

	mu    sync.Mutex
	calls []SensitiveCommand
}

// RunSensitive validates, records, and delegates to RunFunc.
//
// It validates for the same reason the production runner does, and refusing to
// is the more dangerous choice: this fake is the only surface an adapter is
// developed against, so a fake that accepts a relative path, a dash-leading
// dynamic value, or an empty environment pin would let an adapter pass its
// whole suite and fail first in production — against exactly the guards this
// seam exists to add. It refuses an already-done context for the same reason,
// so an adapter's cancellation handling is exercised rather than skipped. A
// refused command is not recorded: production never ran it either, so Calls()
// is a log of attempts that reached a process, not of attempts made.
//
// Two production refusals it cannot mirror, both start failures: an
// unavailable pipe and a fork/exec that fails on a path that is absolute but
// missing or not executable. A fake that never forks cannot reach either, so
// an adapter's start-failure handling stays uncovered here — the gap runs
// toward untested rather than toward accepting something production refuses.
//
// The Args and Env slices are cloned at record time: a caller that reuses a
// backing array across calls would otherwise rewrite its own recorded history.
func (f *FakeSensitiveRunner) RunSensitive(ctx context.Context, cmd SensitiveCommand) (SensitiveResult, error) {
	if err := cmd.validate(); err != nil {
		return failedResult(), newSensitiveError(cmd.Kind, OutcomeInvalid, failedResult(), err.Error())
	}
	if err := ctx.Err(); err != nil {
		outcome := OutcomeCanceled
		if errors.Is(err, context.DeadlineExceeded) {
			outcome = OutcomeTimeout
		}
		return failedResult(), newSensitiveError(cmd.Kind, outcome, failedResult(), "context was already done before start")
	}
	recorded := cmd
	recorded.Args = slices.Clone(cmd.Args)
	recorded.Env = slices.Clone(cmd.Env)

	f.mu.Lock()
	f.calls = append(f.calls, recorded)
	f.mu.Unlock()

	if f.RunFunc != nil {
		return f.RunFunc(cmd)
	}
	return SensitiveResult{}, nil
}

// Calls returns a copy of the recorded commands.
func (f *FakeSensitiveRunner) Calls() []SensitiveCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// Last returns the most recent recorded command and whether there was one.
func (f *FakeSensitiveRunner) Last() (SensitiveCommand, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return SensitiveCommand{}, false
	}
	return f.calls[len(f.calls)-1], true
}

// OutputCause names why a test's BoundedOutput is what it is. It exists so a
// fake cannot say "incomplete" without saying which kind: claiming a cap was
// hit when the stream was merely retired would have an adapter test assert
// against a state production never produces.
type OutputCause uint8

const (
	// OutputComplete means the stream reached EOF within its cap.
	OutputComplete OutputCause = iota
	// OutputOverflowed means the stream exceeded its cap and was killed.
	OutputOverflowed
	// OutputRetired means the read end was closed before EOF — the ordinary
	// outcome when a daemon the command spawned still holds the write end.
	OutputRetired
)

// BoundedOutputForTest builds a BoundedOutput a fake can return, so an adapter
// test can exercise its parsing path without a live backend. It is the only
// constructor for the type outside the runner, and it copies its input.
func BoundedOutputForTest(data []byte, cause OutputCause) BoundedOutput {
	return BoundedOutput{
		buf:      &outputBuf{data: slices.Clone(data)},
		overflow: cause == OutputOverflowed,
		forced:   cause == OutputRetired,
	}
}

var _ SensitiveRunner = (*FakeSensitiveRunner)(nil)
