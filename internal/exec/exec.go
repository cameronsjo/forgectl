// Package exec is the process-execution seam for the whole tool.
//
// Every non-interactive shell-out goes through one of this package's seams:
// Runner, StreamingRunner, or SensitiveRunner. Production
// uses OSRunner; tests inject a fake (see exec_test helpers / FakeRunner) so
// command construction and branching can be asserted without a live tmux server.
//
// Trust model for child binaries: OSRunner passes bare names (gh, git, tmux, …)
// to os/exec, so they resolve through the inherited PATH, and PATH is trusted
// (a PATH pin would move the trust, not remove it). Two exceptions are
// deliberate: sops is resolved once with LookPath and its absolute path is what
// runs (internal/sops/driver.go, enforced by SensitiveRunner's validate), and
// forgectl re-invoking itself (the sops editor) uses os.Executable(), never
// argv[0]. Go's LookPath also refuses a PATH entry resolving to the current
// directory (exec.ErrDot), which closes the cwd-planted-binary case. See #513.
package exec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Runner abstracts running an external command. Five modes:
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
//   - RunWithEnvFiltered captures stdout like RunWithEnv, but first removes
//     named variables from the inherited environment. Explicit overrides win
//     over removals of the same name. This is the security boundary for a
//     command that must not inherit ambient credentials.
//
// # Where a Runner records what a command prints
//
// The OSRunner methods that capture output (Run, RunWithInput, RunWithEnv,
// RunWithEnvFiltered) keep a failed command's output in these places:
//
//   - stderr is logged by runAndWrap at Error level, so any enabled log_level
//     records it (log_level defaults to off). With log_file unset it goes to
//     the dated log file, or to stderr if that cannot be opened.
//     CommandError.Error() also includes it, so it goes wherever a caller
//     renders or logs the error.
//   - stdout is kept in the exported CommandError.Output field. Error() does
//     not include it, but any code that reaches the *CommandError, directly or
//     through errors.As, can read it. Treat a CommandError from a command that
//     may print a secret as secret-bearing; internal/tasks/token.go drops such
//     an error for this reason.
//   - both streams are bounded. stderr keeps its last 64 KiB (maxStderrTail),
//     drains and discards the rest, and records the dropped count on
//     CommandError.StderrDropped; a chatty stderr never fails a command.
//     stdout has a hard ceiling (maxStdoutBytes) that kills the child and
//     fails with ErrOutputTooLarge and no partial output, so a caller never
//     parses a prefix as the whole stream. RunDiscardingStdout
//     (DiscardingRunner) keeps no stdout and so has no ceiling.
//
// Two seams narrow this, and neither makes a true secret safe, because argv
// stays readable through ps for the life of the process. WithMaskedAssignments
// hides the values of marked KEY=VALUE argv elements and scrubs those values
// from the stderr and failure-path stdout that CommandError keeps, and from
// the stderr it logs. Stdout returned on success is not scrubbed, and a value
// shorter than minScrubLen is scrubbed only where it stands as a whole word.
// SensitiveRunner logs metadata only and caps both streams, but it serves only
// its closed CommandKind set. A command whose output may carry a secret needs
// its own path, not Runner.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
	RunInteractive(ctx context.Context, name string, args ...string) error
	RunWithInput(ctx context.Context, stdin string, name string, args ...string) (string, error)
	RunWithEnv(ctx context.Context, env map[string]string, name string, args ...string) (string, error)
	RunWithEnvFiltered(ctx context.Context, env map[string]string, unset []string, name string, args ...string) (string, error)
}

// StreamingRunner is the narrow execution seam for commands whose output must
// be transformed as it arrives. It is deliberately separate from Runner so
// adding a streaming consumer does not force every existing test double to
// grow a method it cannot meaningfully exercise.
type StreamingRunner interface {
	RunStreaming(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error
}

// DiscardingRunner is the optional seam for a caller that runs a command only
// for whether it succeeds and throws its stdout away. RunDiscardingStdout
// behaves like Runner.Run on the failure path (a *CommandError carrying the
// masked stderr tail and the exit code, with an empty Output) but sends stdout
// to a discard sink instead of capturing it, so a command that prints more
// than maxStdoutBytes still succeeds. A caller type-asserts for it and falls
// back to Run, so a Runner that does not implement it (a test fake, a
// policy-enforcing wrapper) keeps working unchanged and is never bypassed.
type DiscardingRunner interface {
	RunDiscardingStdout(ctx context.Context, name string, args ...string) error
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

// OSRunner is the production Runner: it actually spawns processes. Its zero
// value is the production configuration.
type OSRunner struct {
	// stdoutCeiling overrides maxStdoutBytes when positive. It exists so a
	// test can prove the ceiling's kill without writing 64 MiB.
	stdoutCeiling int
}

func (r OSRunner) ceiling() int {
	if r.stdoutCeiling > 0 {
		return r.stdoutCeiling
	}
	return maxStdoutBytes
}

// Run executes name+args and returns trimmed stdout. On failure the returned
// error wraps stderr so callers (and fang's styled error output) stay useful;
// the child's captured stdout (if any) is never discarded — it rides along on
// the returned *CommandError's Output field, since a nonzero exit doesn't
// always mean the command produced nothing worth seeing (e.g. `npm outdated`
// exits 1 precisely when its output has something to report).
func (r OSRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	return runAndWrap(exec.CommandContext(ctx, name, args...), r.ceiling(), "Preparing to run command.", "Successfully ran command.", "Failed to run command.", maskFrom(ctx), name, args) //nolint:gosec // structural argv is the purpose of this execution seam
}

// RunDiscardingStdout executes name+args like Run but discards stdout rather
// than capturing it, so no stdout ceiling applies (DiscardingRunner).
func (r OSRunner) RunDiscardingStdout(ctx context.Context, name string, args ...string) error {
	_, err := runAndWrap(exec.CommandContext(ctx, name, args...), discardStdout, "Preparing to run command, discarding stdout.", "Successfully ran command, discarding stdout.", "Failed to run command, discarding stdout.", maskFrom(ctx), name, args) //nolint:gosec // structural argv is the purpose of this execution seam
	return err
}

// RunWithInput executes name+args with stdin piped in and returns trimmed
// stdout. Same error-wrapping behavior as Run; the only difference is the
// child reads from stdin instead of relying purely on argv (e.g. pbcopy).
func (r OSRunner) RunWithInput(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	return runAndWrap(cmd, r.ceiling(), "Preparing to run command with stdin.", "Successfully ran command with stdin.", "Failed to run command with stdin.", maskFrom(ctx), name, args)
}

// RunWithEnv executes name+args with env merged on top of the inherited
// environment (os.Environ()) and returns trimmed stdout. Same error-wrapping
// behavior as Run; the only difference is the child's environment.
func (r OSRunner) RunWithEnv(ctx context.Context, env map[string]string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return runAndWrap(cmd, r.ceiling(), "Preparing to run command with environment overrides.", "Successfully ran command with environment overrides.", "Failed to run command with environment overrides.", maskFrom(ctx), name, args)
}

// RunWithEnvFiltered executes name+args after removing unset from the inherited
// environment and applying env overrides. An override wins when its key also
// appears in unset, allowing callers to replace an ambient value deliberately.
func (r OSRunner) RunWithEnvFiltered(ctx context.Context, env map[string]string, unset []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // structural argv is the purpose of this execution seam
	cmd.Env = filteredEnvironment(env, unset)
	return runAndWrap(cmd, r.ceiling(), "Preparing to run command with a filtered environment.", "Successfully ran command with a filtered environment.", "Failed to run command with a filtered environment.", maskFrom(ctx), name, args)
}

func filteredEnvironment(overrides map[string]string, unset []string) []string {
	replaced := make(map[string]struct{}, len(overrides)+len(unset))
	for _, key := range unset {
		replaced[key] = struct{}{}
	}
	for key := range overrides {
		replaced[key] = struct{}{}
	}

	inherited := os.Environ()
	filtered := make([]string, 0, len(inherited)+len(overrides))
	for _, entry := range inherited {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, remove := replaced[key]; remove {
				continue
			}
		}
		filtered = append(filtered, entry)
	}
	for key, value := range overrides {
		filtered = append(filtered, key+"="+value)
	}
	return filtered
}

// pipeWaitDelay bounds how long Wait lingers for the output pipes after the
// child has exited or its context was cancelled. Without it, a grandchild that
// inherits stdout/stderr (a script's `sleep 30 &`) keeps the pipe open, so
// CommandContext's SIGKILL of the direct child does not end the call: Wait
// blocks until the grandchild lets go. That defeats every context deadline
// callers rely on, including the lifecycle-lock-held tmux budget in
// internal/pr (#556). 500 ms is ample for a live child to flush what it already
// wrote and short enough not to matter against the seconds-scale deadlines
// above it.
const pipeWaitDelay = 500 * time.Millisecond

// runAndWrap runs an already-configured *exec.Cmd (Stdin/Env set by the
// caller, Stderr not yet wired) and converts its outcome into the Runner
// contract: trimmed stdout on success, or a *CommandError — carrying stderr,
// the captured stdout, and the exit code — on failure. Shared body behind
// Run, RunWithInput, RunWithEnv, and RunWithEnvFiltered, which differ only in
// how they configure cmd beforehand and which log messages they use. mask
// governs every rendering of argv and what the failure path keeps of stderr
// and stdout, plus the stderr it logs (WithMaskedAssignments); cmd itself was
// built from the real args.
func runAndWrap(cmd *exec.Cmd, ceiling int, preparingMsg, successMsg, failureMsg string, mask argMask, name string, args []string) (string, error) {
	shown := mask.args(args)
	slog.Debug(preparingMsg, "cmd", name, "args", shown)
	start := time.Now()

	// Unlike RunSensitive, runAndWrap has no Complete flag: it returns full
	// stdout or an error, and a stderr cut to its tail says so via
	// CommandError.StderrDropped.
	stderr := &tailBuffer{limit: maxStderrTail}
	stdout := &ceilingWriter{limit: ceiling, discard: ceiling == discardStdout, proc: func() *os.Process { return cmd.Process }}
	cmd.Stderr = stderr
	cmd.Stdout = stdout
	cmd.WaitDelay = pipeWaitDelay
	err := cmd.Run()
	// A child that exited 0 while a grandchild still held the pipes (git over
	// ssh ControlPersist) succeeded; WaitDelay only stopped us waiting on them.
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		slog.Debug("Command succeeded but a descendant kept its output pipes open; stopped waiting.", "cmd", name)
		err = nil
	}
	// The ceiling check comes AFTER the WaitDelay forgiveness and overrides it,
	// so an overflow can never be excused as a clean exit: ErrOutputTooLarge
	// always wins.
	if stdout.over {
		// Checked before err: the kill is ours, so err only says "signal:
		// killed" or echoes the write error. No stdout rides along, masked or
		// not, because a prefix is exactly what the ceiling refuses to return.
		err = fmt.Errorf("%w (%d bytes)", ErrOutputTooLarge, ceiling)
	}
	if err != nil {
		// Empty after an overflow: ceilingWriter drops what it held.
		trimmed := mask.text(strings.TrimRight(string(stdout.buf), "\n"))
		tail, dropped := maskedTail(stderr, mask)
		cmdErr := &CommandError{Name: name, Args: shown, Stderr: strings.TrimSpace(tail), StderrDropped: dropped, Output: trimmed, ExitCode: exitCodeOf(err), Err: err}
		switch {
		case cmdErr.Stderr != "" && dropped > 0:
			slog.Error(failureMsg, "cmd", name, "stderr", cmdErr.Stderr, "stderr_dropped", dropped, "error", err)
		case cmdErr.Stderr != "":
			slog.Error(failureMsg, "cmd", name, "stderr", cmdErr.Stderr, "error", err)
		default:
			slog.Error(failureMsg, "cmd", name, "error", err)
		}
		return "", cmdErr
	}
	slog.Debug(successMsg, "cmd", name, "duration", time.Since(start).Round(time.Millisecond))
	return strings.TrimRight(string(stdout.buf), "\n"), nil
}

// RunInteractive wires the child to the real stdio so it can drive the tty.
func (OSRunner) RunInteractive(ctx context.Context, name string, args ...string) error {
	slog.Debug("Preparing to run interactive command.", "cmd", name, "args", maskFrom(ctx).args(args))
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
	Name string
	Args []string

	// Stderr is the child's stderr, masked and trimmed. It is a tail, not
	// the whole stream, whenever StderrDropped is nonzero.
	Stderr string

	// StderrDropped counts the bytes cut from the front of stderr before
	// Stderr: those past the maxStderrTail cap, plus the few trimmed after
	// masking so no fragment of a masked value survives the cut. Zero means
	// Stderr is the whole stream.
	StderrDropped int64

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
		if e.StderrDropped > 0 {
			return cmd + ": [stderr truncated, " + strconv.FormatInt(e.StderrDropped, 10) + " earlier bytes dropped] " + e.Stderr
		}
		return cmd + ": " + e.Stderr
	}
	if e.Err == nil {
		// Production constructors always set Err; a hand-built CommandError
		// (a test fake, another runner) may not, and formatting it must not
		// panic.
		return cmd + ": exit " + strconv.Itoa(e.ExitCode)
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
