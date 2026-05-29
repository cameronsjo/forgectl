package exec

import "context"

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
	RunFunc func(name string, args []string) (string, error)
	// InteractiveErr is returned from every RunInteractive call (nil = success).
	InteractiveErr error

	Calls []Call
}

// Run records the call and delegates to RunFunc.
func (f *FakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	f.Calls = append(f.Calls, Call{Name: name, Args: args})
	if f.RunFunc != nil {
		return f.RunFunc(name, args)
	}
	return "", nil
}

// RunInteractive records the call (flagged interactive) and returns InteractiveErr.
func (f *FakeRunner) RunInteractive(_ context.Context, name string, args ...string) error {
	f.Calls = append(f.Calls, Call{Name: name, Args: args, Interactive: true})
	return f.InteractiveErr
}

// Last returns the most recent recorded Call, or the zero Call if none.
func (f *FakeRunner) Last() Call {
	if len(f.Calls) == 0 {
		return Call{}
	}
	return f.Calls[len(f.Calls)-1]
}
