// Package exec is the process-execution seam for the whole tool.
//
// Everything that shells out to tmux or sesh goes through a Runner. Production
// uses OSRunner; tests inject a fake (see exec_test helpers / FakeRunner) so
// command construction and branching can be asserted without a live tmux server.
package exec

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner abstracts running an external command. Three modes:
//
//   - Run captures stdout for parsing (list-sessions, has-session, …).
//   - RunInteractive hands the controlling tty to the child process, required
//     by attach-session and `sesh connect`, which take over the terminal.
//   - RunWithInput pipes a string into the child's stdin and captures stdout,
//     for commands that read from stdin rather than argv (pbcopy).
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
	RunInteractive(ctx context.Context, name string, args ...string) error
	RunWithInput(ctx context.Context, stdin string, name string, args ...string) (string, error)
}

// OSRunner is the production Runner: it actually spawns processes.
type OSRunner struct{}

// Run executes name+args and returns trimmed stdout. On failure the returned
// error wraps stderr so callers (and fang's styled error output) stay useful;
// the child's captured stdout (if any) is never discarded — it rides along on
// the returned *CommandError's Output field, since a nonzero exit doesn't
// always mean the command produced nothing worth seeing (e.g. `npm outdated`
// exits 1 precisely when its output has something to report).
func (OSRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	slog.Debug("Preparing to run command.", "cmd", name, "args", args)
	start := time.Now()

	cmd := exec.CommandContext(ctx, name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		trimmed := strings.TrimRight(string(out), "\n")
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			slog.Error("Failed to run command.", "cmd", name, "stderr", msg, "error", err)
			return "", &CommandError{Name: name, Args: args, Stderr: msg, Output: trimmed, ExitCode: exitCodeOf(err), Err: err}
		}
		slog.Error("Failed to run command.", "cmd", name, "error", err)
		return "", &CommandError{Name: name, Args: args, Output: trimmed, ExitCode: exitCodeOf(err), Err: err}
	}
	slog.Debug("Successfully ran command.", "cmd", name, "duration", time.Since(start).Round(time.Millisecond))
	return strings.TrimRight(string(out), "\n"), nil
}

// RunWithInput executes name+args with stdin piped in and returns trimmed
// stdout. Same error-wrapping behavior as Run; the only difference is the
// child reads from stdin instead of relying purely on argv (e.g. pbcopy).
func (OSRunner) RunWithInput(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	slog.Debug("Preparing to run command with stdin.", "cmd", name, "args", args)
	start := time.Now()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		trimmed := strings.TrimRight(string(out), "\n")
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			slog.Error("Failed to run command with stdin.", "cmd", name, "stderr", msg, "error", err)
			return "", &CommandError{Name: name, Args: args, Stderr: msg, Output: trimmed, ExitCode: exitCodeOf(err), Err: err}
		}
		slog.Error("Failed to run command with stdin.", "cmd", name, "error", err)
		return "", &CommandError{Name: name, Args: args, Output: trimmed, ExitCode: exitCodeOf(err), Err: err}
	}
	slog.Debug("Successfully ran command with stdin.", "cmd", name, "duration", time.Since(start).Round(time.Millisecond))
	return strings.TrimRight(string(out), "\n"), nil
}

// RunInteractive wires the child to the real stdio so it can drive the tty.
func (OSRunner) RunInteractive(ctx context.Context, name string, args ...string) error {
	slog.Debug("Preparing to run interactive command.", "cmd", name, "args", args)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	slog.Debug("Interactive command exited.", "cmd", name, "error", err)
	return err
}

// CommandError carries enough context to debug a failed shell-out without
// leaking the whole environment.
type CommandError struct {
	Name   string
	Args   []string
	Stderr string

	// Output is the child's captured stdout, even though the command
	// failed — a nonzero exit doesn't mean stdout was empty (npm's
	// `outdated` subcommand, for one, exits 1 precisely when it has a
	// table to report). Empty when the process produced no stdout, or
	// never ran at all (e.g. the binary wasn't found).
	Output string

	// ExitCode is the child process's exit status when Err wraps a real
	// *os/exec.ExitError, or -1 when the command never got that far (bad
	// binary path, context cancellation, …) — callers that special-case a
	// specific exit code (npmStep's Check) use this instead of unwrapping
	// Err themselves.
	ExitCode int

	Err error
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

// exitCodeOf extracts the process exit code from err via *os/exec.ExitError,
// or -1 when err doesn't wrap one (the command never started, the context
// was canceled, …) — -1 is never a real exit code, so it's a safe "unknown"
// sentinel for callers comparing against a specific status.
func exitCodeOf(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
