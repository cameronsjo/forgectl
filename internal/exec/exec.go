// Package exec is the process-execution seam for the whole tool.
//
// Everything that shells out to tmux or sesh goes through a Runner. Production
// uses OSRunner; tests inject a fake (see exec_test helpers / FakeRunner) so
// command construction and branching can be asserted without a live tmux server.
package exec

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

// Runner abstracts running an external command. Two modes:
//
//   - Run captures stdout for parsing (list-sessions, has-session, …).
//   - RunInteractive hands the controlling tty to the child process, required
//     by attach-session and `sesh connect`, which take over the terminal.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
	RunInteractive(ctx context.Context, name string, args ...string) error
}

// OSRunner is the production Runner: it actually spawns processes.
type OSRunner struct{}

// Run executes name+args and returns trimmed stdout. On failure the returned
// error wraps stderr so callers (and fang's styled error output) stay useful.
func (OSRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", &CommandError{Name: name, Args: args, Stderr: msg, Err: err}
		}
		return "", &CommandError{Name: name, Args: args, Err: err}
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// RunInteractive wires the child to the real stdio so it can drive the tty.
func (OSRunner) RunInteractive(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// CommandError carries enough context to debug a failed shell-out without
// leaking the whole environment.
type CommandError struct {
	Name   string
	Args   []string
	Stderr string
	Err    error
}

func (e *CommandError) Error() string {
	cmd := e.Name
	if len(e.Args) > 0 {
		cmd += " " + strings.Join(e.Args, " ")
	}
	if e.Stderr != "" {
		return cmd + ": " + e.Stderr
	}
	return cmd + ": " + e.Err.Error()
}

func (e *CommandError) Unwrap() error { return e.Err }
