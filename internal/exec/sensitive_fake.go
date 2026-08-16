package exec

import (
	"context"
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

// RunSensitive records the command and delegates to RunFunc. The Args and Env
// slices are cloned at record time: a caller that reuses a backing array
// across calls would otherwise rewrite its own recorded history.
func (f *FakeSensitiveRunner) RunSensitive(_ context.Context, cmd SensitiveCommand) (SensitiveResult, error) {
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

// BoundedOutputForTest builds a BoundedOutput a fake can return, so an adapter
// test can exercise its parsing path without a live backend. It is the only
// constructor for the type outside the runner, and it copies its input.
func BoundedOutputForTest(data []byte, complete bool) BoundedOutput {
	return BoundedOutput{buf: &outputBuf{data: slices.Clone(data)}, overflow: !complete}
}

var _ SensitiveRunner = (*FakeSensitiveRunner)(nil)
