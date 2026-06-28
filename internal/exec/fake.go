package exec

import (
	"context"
	"sync"
)

// Call records one invocation through a FakeRunner: the binary, its args, and
// whether it went through the interactive path. Tests assert on these to check
// command construction (the argv tmux/sesh actually receive).
type Call struct {
	Name        string
	Args        []string
	Interactive bool
}

// FakeRunner is the test double for Runner. It records every Call and produces
// canned output via RunFunc. Zero value is usable: it records calls and
// returns empty stdout / nil error.
type FakeRunner struct {
	// RunFunc produces stdout (or an error) for Run calls. If nil, Run returns
	// "" and nil. Keyed off (name, args) so a test can branch per command.
	// RunFunc may be invoked concurrently (e.g. Inventory fans out gh + tea), so
	// keep it read-only over shared state.
	RunFunc func(name string, args []string) (string, error)
	// InteractiveErr is returned from every RunInteractive call (nil = success).
	InteractiveErr error

	mu    sync.Mutex
	Calls []Call
}

// Run records the call and delegates to RunFunc. The Calls append is mutex-
// guarded because callers like projects.Inventory invoke Run from concurrent
// goroutines.
func (f *FakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, Call{Name: name, Args: args})
	f.mu.Unlock()
	if f.RunFunc != nil {
		return f.RunFunc(name, args)
	}
	return "", nil
}

// RunInteractive records the call (flagged interactive) and returns InteractiveErr.
func (f *FakeRunner) RunInteractive(_ context.Context, name string, args ...string) error {
	f.mu.Lock()
	f.Calls = append(f.Calls, Call{Name: name, Args: args, Interactive: true})
	f.mu.Unlock()
	return f.InteractiveErr
}

// Last returns the most recent recorded Call, or the zero Call if none.
func (f *FakeRunner) Last() Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Calls) == 0 {
		return Call{}
	}
	return f.Calls[len(f.Calls)-1]
}
