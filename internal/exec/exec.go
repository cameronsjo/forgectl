// Package exec is the process-execution seam for the whole tool.
//
// Everything that shells out to tmux or sesh goes through a Runner. Production
// uses OSRunner; tests inject a fake (see exec_test helpers / FakeRunner) so
// command construction and branching can be asserted without a live tmux server.
package exec

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner abstracts running an external command. Four modes:
//
//   - Run captures stdout for parsing (list-sessions, has-session, …).
//   - RunInteractive hands the controlling tty to the child process, required
//     by attach-session and `sesh connect`, which take over the terminal.
//   - RunWithInput pipes a string into the child's stdin and captures stdout,
//     for commands that read from stdin rather than argv (pbcopy).
//   - RunWithEnv captures stdout like Run, with extra environment variables
//     merged on top of the inherited environment — for a command whose
//     behavior needs pinning regardless of the caller's ambient env (e.g.
//     `HOMEBREW_NO_AUTO_UPDATE=1`, so `brew outdated` can't silently trigger
//     Homebrew's own implicit update-and-tap-refresh as a side effect).
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
	RunInteractive(ctx context.Context, name string, args ...string) error
	RunWithInput(ctx context.Context, stdin string, name string, args ...string) (string, error)
	RunWithEnv(ctx context.Context, env map[string]string, name string, args ...string) (string, error)
}

// StreamingRunner is the narrow execution seam for commands whose output must
// be transformed as it arrives. It is deliberately separate from Runner so
// adding a streaming consumer does not force every existing test double to
// grow a method it cannot meaningfully exercise.
type StreamingRunner interface {
	RunStreaming(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error
}

// HomebrewNoAutoUpdate disables Homebrew's own implicit "auto-update and
// refresh taps" behavior: several brew subcommands beyond `update` itself —
// `outdated`, `upgrade`, `install`, `list --versions`, … — silently trigger
// it on their own once the last auto-update is more than 24h stale. Every
// brew-shelling caller in this repo (internal/update, internal/selfupdate)
// merges this onto its RunWithEnv calls so brew's behavior stays
// deterministic regardless of how stale the ambient Homebrew auto-update
// timestamp happens to be — a single definition so the two callers can
// never drift apart on the exact env shape.
var HomebrewNoAutoUpdate = map[string]string{"HOMEBREW_NO_AUTO_UPDATE": "1"}

// OSRunner is the production Runner: it actually spawns processes.
type OSRunner struct{}

// Run executes name+args and returns trimmed stdout. On failure the returned
// error wraps stderr so callers (and fang's styled error output) stay useful;
// the child's captured stdout (if any) is never discarded — it rides along on
// the returned *CommandError's Output field, since a nonzero exit doesn't
// always mean the command produced nothing worth seeing (e.g. `npm outdated`
// exits 1 precisely when its output has something to report).
func (OSRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	return runAndWrap(exec.CommandContext(ctx, name, args...), "Preparing to run command.", "Successfully ran command.", "Failed to run command.", name, args)
}

// RunWithInput executes name+args with stdin piped in and returns trimmed
// stdout. Same error-wrapping behavior as Run; the only difference is the
// child reads from stdin instead of relying purely on argv (e.g. pbcopy).
func (OSRunner) RunWithInput(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	return runAndWrap(cmd, "Preparing to run command with stdin.", "Successfully ran command with stdin.", "Failed to run command with stdin.", name, args)
}

// RunWithEnv executes name+args with env merged on top of the inherited
// environment (os.Environ()) and returns trimmed stdout. Same error-wrapping
// behavior as Run; the only difference is the child's environment.
func (OSRunner) RunWithEnv(ctx context.Context, env map[string]string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return runAndWrap(cmd, "Preparing to run command with environment overrides.", "Successfully ran command with environment overrides.", "Failed to run command with environment overrides.", name, args)
}

// runAndWrap runs an already-configured *exec.Cmd (Stdin/Env set by the
// caller, Stderr not yet wired) and converts its outcome into the Runner
// contract: trimmed stdout on success, or a *CommandError — carrying stderr,
// the captured stdout, and the exit code — on failure. Shared body behind
// Run, RunWithInput, and RunWithEnv, which differ only in how they configure
// cmd beforehand and which log messages they use.
func runAndWrap(cmd *exec.Cmd, preparingMsg, successMsg, failureMsg, name string, args []string) (string, error) {
	slog.Debug(preparingMsg, "cmd", name, "args", args)
	start := time.Now()

	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		trimmed := strings.TrimRight(string(out), "\n")
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			slog.Error(failureMsg, "cmd", name, "stderr", msg, "error", err)
			return "", &CommandError{Name: name, Args: args, Stderr: msg, Output: trimmed, ExitCode: exitCodeOf(err), Err: err}
		}
		slog.Error(failureMsg, "cmd", name, "error", err)
		return "", &CommandError{Name: name, Args: args, Output: trimmed, ExitCode: exitCodeOf(err), Err: err}
	}
	slog.Debug(successMsg, "cmd", name, "duration", time.Since(start).Round(time.Millisecond))
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

// RunStreaming connects a child to caller-supplied streams without buffering
// stdout or stderr. Unlike the ordinary Runner methods it does not log or
// return argv: kubectl's global flags can carry credentials, and a streaming
// helper must not turn those into a second persistence surface. The child exit
// code is retained on CommandError so an explicitly opted-in CLI can propagate
// it.
func (OSRunner) RunStreaming(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	slog.Debug("Preparing to run streaming command.", "cmd", name)
	// name and args stay distinct all the way into os/exec; no shell parses
	// them. StreamingRunner is the same process boundary as Runner.Run above.
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // structural argv is the purpose of this execution seam
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		slog.Debug("Streaming command exited.", "cmd", name)
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return &CommandError{Name: name, ExitCode: -1, Err: ctxErr}
	}
	return &CommandError{Name: name, ExitCode: exitCodeOf(err), Err: err}
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
