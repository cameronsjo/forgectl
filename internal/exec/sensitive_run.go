package exec

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"
)

// defaultRetireBound is how long the runner waits for a stream's reader to
// finish on its own before force-closing the read end. It exists because the
// immediate CLI is not the only process that can hold a pipe: anything it
// spawned inherits the write end and can keep it open long after the CLI
// itself has been reaped. Without a bound, "the command finished" and "the
// call returns" are different events separated by a descendant's lifetime.
const defaultRetireBound = 2 * time.Second

// OSSensitiveRunner is the production SensitiveRunner. It captures the
// inherited environment once at construction so a later os.Setenv elsewhere in
// the process cannot change what a sensitive command inherits mid-flight.
type OSSensitiveRunner struct {
	env []string

	// retireBound overrides defaultRetireBound; tests set it directly so the
	// descendant-holds-a-pipe case can be proven without a two-second wait.
	retireBound time.Duration
}

// NewOSSensitiveRunner snapshots the current environment and returns a runner
// ready for production use.
func NewOSSensitiveRunner() *OSSensitiveRunner {
	return &OSSensitiveRunner{env: os.Environ(), retireBound: defaultRetireBound}
}

func (r *OSSensitiveRunner) bound() time.Duration {
	if r.retireBound > 0 {
		return r.retireBound
	}
	return defaultRetireBound
}

// buildEnv clones the captured environment, drops every occurrence of each
// mutated key, and appends one replacement per replace mutation. Removing all
// occurrences matters: a duplicated key in the inherited environment would
// otherwise leave the stale entry in place on some platforms' lookup order.
// Entries the mutation set does not name are copied byte-exact and never
// inspected.
func (r *OSSensitiveRunner) buildEnv(muts []EnvMutation) []string {
	if len(muts) == 0 {
		out := make([]string, len(r.env))
		copy(out, r.env)
		return out
	}
	drop := make(map[string]struct{}, len(muts))
	for _, m := range muts {
		drop[m.key] = struct{}{}
	}
	out := make([]string, 0, len(r.env)+len(muts))
	for _, entry := range r.env {
		if _, mutated := drop[envKeyOf(entry)]; mutated {
			continue
		}
		out = append(out, entry)
	}
	for _, m := range muts {
		if m.op == envOpReplace {
			out = append(out, m.key+"="+m.value.reveal())
		}
	}
	return out
}

// envKeyOf splits an "K=V" environment entry at the first '='. An entry with
// no '=' is its own key, which is what the runtime does with such an entry too.
func envKeyOf(entry string) string {
	for i := 0; i < len(entry); i++ {
		if entry[i] == '=' {
			return entry[:i]
		}
	}
	return entry
}

// buildCmd is the reveal boundary. It is the only place in the package where a
// SecretArg or Arg payload leaves its wrapper, and everything it produces goes
// straight into the *exec.Cmd. It is a separate function so internal/exec's own
// tests can assert that the real values do reach exec.Cmd.Args — the mirror of
// the redaction tests, without a production accessor that reveals.
func (r *OSSensitiveRunner) buildCmd(sc SensitiveCommand) *exec.Cmd {
	argv := make([]string, len(sc.Args))
	for i := range sc.Args {
		argv[i] = sc.Args[i].reveal()
	}
	cmd := exec.Command(sc.Path.reveal(), argv...)
	cmd.Env = r.buildEnv(sc.Env)
	return cmd
}

// RunSensitive runs one bounded command. Nothing it logs or returns can render
// the path, the argv, or the environment; stdout and stderr are captured
// concurrently into fixed buffers that cannot exceed the caps; and every
// abnormal ending — overflow, timeout, cancellation — kills and reaps the
// immediate CLI and retires both pipe ends within a bounded wait.
func (r *OSSensitiveRunner) RunSensitive(ctx context.Context, sc SensitiveCommand) (SensitiveResult, error) {
	if err := sc.validate(); err != nil {
		slog.Debug("Refusing sensitive command before start.", "cmd", sc, "reason", err.Error())
		return SensitiveResult{}, newSensitiveError(sc.Kind, OutcomeInvalid, SensitiveResult{}, err.Error())
	}

	outR, outW, err := os.Pipe()
	if err != nil {
		return SensitiveResult{}, newSensitiveError(sc.Kind, OutcomeStartFailed, SensitiveResult{}, "stdout pipe unavailable")
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		closeAll(outR, outW)
		return SensitiveResult{}, newSensitiveError(sc.Kind, OutcomeStartFailed, SensitiveResult{}, "stderr pipe unavailable")
	}

	cmd := r.buildCmd(sc)
	cmd.Stdout = outW
	cmd.Stderr = errW

	slog.Debug("Preparing to run sensitive command.", "cmd", sc)
	start := time.Now()

	if err := cmd.Start(); err != nil {
		closeAll(outR, outW, errR, errW)
		res := SensitiveResult{ExitCode: -1}
		slog.Error("Sensitive command failed to start.", "kind", sc.Kind.String())
		return res, newSensitiveError(sc.Kind, OutcomeStartFailed, res, "fork/exec did not succeed")
	}

	// The parent's write ends must go now, or the readers never see EOF even
	// after every child process has exited.
	closeAll(outW, errW)

	overflow := make(chan struct{}, 2)
	outCh := make(chan BoundedOutput, 1)
	errCh := make(chan BoundedOutput, 1)
	go readCapped(outR, sc.StdoutCap, outCh, overflow)
	go readCapped(errR, sc.StderrCap, errCh, overflow)

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var (
		killOnce sync.Once
		trigger  = OutcomeUnspecified
		waitErr  error
	)
	// The Kill error is dropped deliberately: the only failure it reports is
	// os.ErrProcessDone, which is the state kill was trying to reach. Every
	// caller of kill follows it with a Wait, which carries the real outcome.
	kill := func() { killOnce.Do(func() { _ = cmd.Process.Kill() }) }

	select {
	case waitErr = <-waitCh:
		// The immediate CLI is done. A descendant may still hold a pipe; the
		// retirement below is what bounds that.
	case <-ctx.Done():
		trigger = OutcomeCanceled
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			trigger = OutcomeTimeout
		}
		kill()
		waitErr = <-waitCh
	case <-overflow:
		trigger = OutcomeOutputLimit
		kill()
		waitErr = <-waitCh
	}

	// Retire both pipes. On an abnormal ending there is nothing left worth
	// waiting for, so the read ends close immediately; on a clean exit the
	// readers get the bound to drain what the CLI already wrote before the
	// same force-close applies to whatever descendant inherited the pipe.
	stdout, stderr := r.retire(trigger != OutcomeUnspecified, outR, errR, outCh, errCh)
	if !stdout.Complete() || !stderr.Complete() {
		if trigger == OutcomeUnspecified {
			trigger = OutcomeOutputLimit
		}
		kill()
	}

	res := SensitiveResult{Stdout: stdout, Stderr: stderr, ExitCode: exitCodeOf(waitErr)}
	if waitErr == nil {
		res.ExitCode = 0
	}

	switch {
	case trigger != OutcomeUnspecified:
		slog.Error("Sensitive command retired early.",
			"kind", sc.Kind.String(), "outcome", trigger.String(),
			"duration", time.Since(start).Round(time.Millisecond), "result", res)
		return res, newSensitiveError(sc.Kind, trigger, res, retireReason(trigger))
	case waitErr != nil:
		slog.Error("Sensitive command exited nonzero.",
			"kind", sc.Kind.String(), "exit", res.ExitCode,
			"duration", time.Since(start).Round(time.Millisecond), "result", res)
		return res, newSensitiveError(sc.Kind, OutcomeExit, res, "process reported a nonzero status")
	}

	slog.Debug("Successfully ran sensitive command.",
		"kind", sc.Kind.String(), "duration", time.Since(start).Round(time.Millisecond), "result", res)
	return res, nil
}

func retireReason(o Outcome) string {
	switch o {
	case OutcomeTimeout:
		return "context deadline expired; process killed"
	case OutcomeCanceled:
		return "context canceled; process killed"
	case OutcomeOutputLimit:
		return "stream exceeded its cap; process killed"
	default:
		return ""
	}
}

// retire collects both readers and guarantees the call returns even when a
// descendant still holds a write end. Closing an *os.File interrupts a Read
// blocked on it, so the force-close is what unblocks a reader no EOF is ever
// coming for.
func (r *OSSensitiveRunner) retire(immediate bool, outR, errR *os.File, outCh, errCh <-chan BoundedOutput) (BoundedOutput, BoundedOutput) {
	if immediate {
		closeAll(outR, errR)
		return <-outCh, <-errCh
	}

	timer := time.NewTimer(r.bound())
	defer timer.Stop()

	var (
		stdout, stderr BoundedOutput
		gotOut, gotErr bool
	)
	for !gotOut || !gotErr {
		select {
		case stdout = <-outCh:
			gotOut = true
		case stderr = <-errCh:
			gotErr = true
		case <-timer.C:
			closeAll(outR, errR)
			if !gotOut {
				stdout = <-outCh
			}
			if !gotErr {
				stderr = <-errCh
			}
			return stdout, stderr
		}
	}
	closeAll(outR, errR)
	return stdout, stderr
}

// readCapped fills a single fixed buffer of cap+1 bytes and stops. It never
// grows an allocation to match what the process produced, and on overflow it
// stops reading rather than draining — the caller kills the process, which is
// what unblocks a writer now stuck on a full pipe.
func readCapped(r io.Reader, limit int64, out chan<- BoundedOutput, overflow chan<- struct{}) {
	buf := make([]byte, limit+1)
	n := 0
	for int64(n) < limit+1 {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			break
		}
	}
	if int64(n) > limit {
		select {
		case overflow <- struct{}{}:
		default:
		}
		out <- BoundedOutput{data: buf[:limit], overflow: true}
		return
	}
	out <- BoundedOutput{data: buf[:n]}
}

// closeAll retires pipe ends. Close errors are dropped deliberately: these
// are read/write ends of the runner's own pipes, a second Close is expected
// on the paths where both the timeout and Wait retire the same descriptor,
// and no caller decision depends on the result.
func closeAll(files ...*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}

// compile-time proof the production runner satisfies the seam.
var _ SensitiveRunner = (*OSSensitiveRunner)(nil)
