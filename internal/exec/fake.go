package exec

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
)

// Call records one invocation through a FakeRunner: the binary, its args,
// whether it went through the interactive path, — for RunWithInput — what
// was piped into stdin, and — for the environment modes — the overrides and
// removals passed. Tests assert on these to check command construction (the
// argv tmux/sesh actually receive).
//
// For a tmux call, Args is the argv with the client's leading `-u` removed and
// TmuxUTF8 records whether it was there (see tmuxView).
type Call struct {
	Name        string
	Args        []string
	TmuxUTF8    bool
	Interactive bool
	Input       string
	Env         map[string]string
	UnsetEnv    []string
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
	return f.answer(Call{Name: name}, args)
}

// RunWithInput records the call (with Input set to stdin) and delegates to
// RunFunc, same as Run — RunFunc doesn't see stdin, only name/args, so a
// test that needs to branch on the piped input reads it back off Calls.
func (f *FakeRunner) RunWithInput(_ context.Context, stdin string, name string, args ...string) (string, error) {
	return f.answer(Call{Name: name, Input: stdin}, args)
}

// RunWithEnv records the call (with Env set) and delegates to RunFunc, same
// as Run — RunFunc doesn't see env, only name/args, so a test that needs to
// branch on the env reads it back off Calls.
func (f *FakeRunner) RunWithEnv(_ context.Context, env map[string]string, name string, args ...string) (string, error) {
	return f.answer(Call{Name: name, Env: env}, args)
}

// RunWithEnvFiltered records both environment overrides and exact removals,
// then delegates to RunFunc like the other captured-output modes.
func (f *FakeRunner) RunWithEnvFiltered(_ context.Context, env map[string]string, unset []string, name string, args ...string) (string, error) {
	return f.answer(Call{Name: name, Env: env, UnsetEnv: unset}, args)
}

// RunInteractive records the call (flagged interactive) and returns InteractiveErr.
func (f *FakeRunner) RunInteractive(_ context.Context, name string, args ...string) error {
	call := Call{Name: name, Interactive: true}
	call.Args, call.TmuxUTF8 = tmuxView(name, args)
	f.mu.Lock()
	f.Calls = append(f.Calls, call)
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

// answer records call with the argv tmuxView presents, then produces RunFunc's
// reply for that same view.
func (f *FakeRunner) answer(call Call, args []string) (string, error) {
	call.Args, call.TmuxUTF8 = tmuxView(call.Name, args)
	f.mu.Lock()
	f.Calls = append(f.Calls, call)
	f.mu.Unlock()
	if f.RunFunc == nil {
		return "", nil
	}
	out, err := f.RunFunc(call.Name, call.Args)
	if call.TmuxUTF8 {
		err = issuedArgvError(err, call, args)
	}
	return out, err
}

// issuedArgvError makes a CommandError a RunFunc built from the argv it was
// shown name the argv the caller issued, because a real runner's does and
// internal/tmux compares the two to classify a failure.
//
// Only a *CommandError returned directly, naming exactly this call's view, is
// replaced, and it is replaced by a COPY: a RunFunc may return one error value
// from every call, and rewriting it in place would change what that value says
// to the test holding it and to every later call. The copy keeps Err, so
// errors.Is and errors.As through it reach what the original reached; it is
// not the same pointer, so errors.Is(err, original) no longer holds.
//
// A CommandError naming this call's view but WRAPPED inside another error is
// refused with a panic, not returned: rebuilding an arbitrary wrapper chain
// around a copy is not possible, and returning it untouched would hand
// internal/tmux an argv it never issued, so its failure classification
// (sessions.go, server_state.go) would silently miss the error and the test
// would assert against a state production never reaches (forgectl#851). A
// wrapped CommandError naming some other argv is returned untouched, like a
// direct one.
func issuedArgvError(err error, call Call, issued []string) error {
	cmdErr, ok := err.(*CommandError) //nolint:errorlint // only a direct CommandError is replaced; see above
	if !ok {
		var wrapped *CommandError
		if errors.As(err, &wrapped) && wrapped.Name == call.Name && slices.Equal(wrapped.Args, call.Args) {
			panic(fmt.Sprintf("exec: FakeRunner.RunFunc returned a tmux CommandError naming the argv it was shown (%q), "+
				"wrapped inside another error, for a call that issued %q; return the *CommandError directly so the "+
				"fake can restore the issued argv, or build it with a different argv", call.Args, issued))
		}
		return err
	}
	if cmdErr.Name != call.Name || !slices.Equal(cmdErr.Args, call.Args) {
		return err
	}
	cp := *cmdErr
	cp.Args = slices.Clone(issued)
	return &cp
}

// tmuxView is the argv a tmux call presents to RunFunc and to Calls: the
// caller's argv with internal/tmux's `-u` removed from the leading global
// options, and whether it was there (forgectl#840).
//
// internal/tmux passes `-u` on every non-interactive command, after the
// `-S <socket>` pin when there is one. Nearly every tmux fake in the tree
// branches on args[0] and asserts exact argv, all written before that flag
// existed, so presenting them the argv without it keeps each one reading the
// command it was written for; a test that cares about the flag reads
// Call.TmuxUTF8. Only the flag is removed. The other leading global options
// (`-S <path>`, `-L <name>`, `-f <file>`) are skipped over while looking for
// it and stay in the view, because the socket-pin tests assert them. Any
// other binary's argv passes through untouched.
//
// TmuxSubcommand below walks the same leading global options and must stay in
// step with it: if tmux gains a value-taking global option that internal/tmux
// passes, add it to both. They differ on purpose in what they keep — this
// view drops only `-u` and keeps the pin, because fakes assert the pin;
// TmuxSubcommand drops every global option, because its callers key on the
// command.
func tmuxView(name string, args []string) ([]string, bool) {
	if filepath.Base(name) != "tmux" {
		return args, false
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-u":
			return append(slices.Clone(args[:i]), args[i+1:]...), true
		case "-S", "-L", "-f":
			i++ // skip the option's value
		default:
			return args, false
		}
	}
	return args, false
}

// TmuxSubcommand is a tmux argv past its leading global options: `-u` (which
// internal/tmux passes on every non-interactive call, forgectl#840) and the
// value-taking `-S <path>`, `-L <name>` and `-f <file>`. It is the one
// definition every hand-written test runner and verb helper uses to find the
// command it keys on, rather than reading args[0], which is `-u` or `-S` on a
// real argv. A global option missing its value leaves nothing.
//
// It walks the same option set as tmuxView above; keep the two in step (see
// tmuxView for why they differ in what they keep).
func TmuxSubcommand(args []string) []string {
	for len(args) > 0 {
		switch args[0] {
		case "-u":
			args = args[1:]
		case "-S", "-L", "-f":
			if len(args) < 2 {
				return nil
			}
			args = args[2:]
		default:
			return args
		}
	}
	return args
}
