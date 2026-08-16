package exec

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// defaultRetireBound is how long the runner waits for a stream's reader to
// finish on its own before force-closing the read end. It exists because the
// immediate CLI is not the only process that can hold a pipe: anything it
// spawned inherits the write end and can keep it open long after the CLI
// itself has been reaped. Without a bound, "the command finished" and "the
// call returns" are different events separated by a descendant's lifetime.
const defaultRetireBound = 2 * time.Second

// maxZeroProgressReads bounds a reader that keeps returning (0, nil), which
// io.Reader permits and os.File does not do in practice. Purely defensive: it
// converts a theoretical spin into a bounded stop.
const maxZeroProgressReads = 64

// OSSensitiveRunner is the production SensitiveRunner. It captures the
// inherited environment once at construction so a later os.Setenv elsewhere in
// the process cannot change what a sensitive command inherits mid-flight.
type OSSensitiveRunner struct {
	env []string

	// retireBound overrides defaultRetireBound; tests set it directly so the
	// descendant-holds-a-pipe case can be proven without a two-second wait.
	retireBound time.Duration

	// started counts successful fork/execs. It exists because "this command
	// never ran" is not observable from the child: a refusal that kills, or a
	// pre-start check that never forks, both leave no trace in the child's own
	// side effects — the kill wins that race every time. Counting at the one
	// place a process comes into existence is what makes the refusal provable
	// rather than assumed.
	started atomic.Int64
}

// StartedCount reports how many processes this runner has successfully
// started. It is metadata about the runner, never about any command.
func (r *OSSensitiveRunner) StartedCount() int64 { return r.started.Load() }

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
// inspected. Key matching is case-sensitive, which is correct for the POSIX
// platforms forgectl targets.
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
//
// It builds with exec.Command rather than exec.CommandContext deliberately.
// CommandContext kills on context completion but does not own what happens
// next, and this runner does: it must kill, reap, and retire two pipe ends in
// a defined order and within a tested bound, and it must distinguish a
// deadline from a cancellation from an output-limit kill in the returned
// outcome. Handing half of that to CommandContext would leave two killers
// racing for the same process. The context check that CommandContext performs
// before Start is done explicitly in RunSensitive instead.
//
// validate has already required an absolute path, so exec.Command performs no
// PATH lookup here — which matters, because LookPath reads the live process
// PATH rather than this runner's captured environment.
func (r *OSSensitiveRunner) buildCmd(sc SensitiveCommand) *exec.Cmd {
	argv := make([]string, len(sc.Args))
	for i := range sc.Args {
		argv[i] = sc.Args[i].reveal()
	}
	cmd := exec.Command(sc.Path.reveal(), argv...)
	cmd.Env = r.buildEnv(sc.Env)
	return cmd
}

// failedResult is what every never-ran path returns. ExitCode is -1, never 0:
// a command that did not run has no exit status, and 0 is the one value a
// caller reads as success.
func failedResult() SensitiveResult { return SensitiveResult{ExitCode: -1} }

// RunSensitive runs one bounded command. Nothing it logs or returns can render
// the path, the argv, or the environment, and stdout and stderr are captured
// concurrently into fixed buffers that cannot exceed the caps.
//
// An overflow, a timeout, and a cancellation kill and reap the immediate CLI
// and retire both pipe ends. A descendant still holding a pipe after the CLI
// exited on its own is different: nothing is killed there — the read ends are
// closed and the call returns, leaving the descendant to its own lifetime.
// Either way a stream that stopped short is reported incomplete.
func (r *OSSensitiveRunner) RunSensitive(ctx context.Context, sc SensitiveCommand) (SensitiveResult, error) {
	if err := sc.validate(); err != nil {
		slog.Debug("Refusing sensitive command before start.", "cmd", sc, "reason", err.Error())
		return failedResult(), newSensitiveError(sc.Kind, OutcomeInvalid, failedResult(), err.Error())
	}

	// An already-done context must not buy a fork/exec. exec.CommandContext
	// performs this check internally; since this runner deliberately does not
	// use it (see buildCmd), the check is explicit here.
	if err := ctx.Err(); err != nil {
		outcome := OutcomeCanceled
		if errors.Is(err, context.DeadlineExceeded) {
			outcome = OutcomeTimeout
		}
		slog.Debug("Refusing sensitive command; context already done.", "cmd", sc, "outcome", outcome.String())
		return failedResult(), newSensitiveError(sc.Kind, outcome, failedResult(), "context was already done before start")
	}

	outR, outW, err := os.Pipe()
	if err != nil {
		return failedResult(), newSensitiveError(sc.Kind, OutcomeStartFailed, failedResult(), "stdout pipe unavailable")
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		closeAll(outR, outW)
		return failedResult(), newSensitiveError(sc.Kind, OutcomeStartFailed, failedResult(), "stderr pipe unavailable")
	}

	cmd := r.buildCmd(sc)
	cmd.Stdout = outW
	cmd.Stderr = errW

	slog.Debug("Preparing to run sensitive command.", "cmd", sc)
	start := time.Now()

	if err := cmd.Start(); err != nil {
		closeAll(outR, outW, errR, errW)
		slog.Error("Sensitive command failed to start.", "kind", sc.Kind.String())
		return failedResult(), newSensitiveError(sc.Kind, OutcomeStartFailed, failedResult(), "fork/exec did not succeed")
	}

	r.started.Add(1)

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

	// A completed process wins over a simultaneously-ready cancellation. Go's
	// select picks uniformly among ready cases, so without this the outcome of
	// a command that finished just as its deadline expired would be a coin
	// flip between success and timeout.
	select {
	case waitErr = <-waitCh:
	default:
		// An overflow already signalled outranks a simultaneously-ready
		// cancellation for the same reason completion does: the classification
		// is what a caller retries on, and a coin flip between "the backend said
		// too much" and "we ran out of time" is a misleading answer to that.
		select {
		case <-overflow:
			trigger = OutcomeOutputLimit
			kill()
			waitErr = <-waitCh
		default:
			trigger, waitErr = r.awaitOutcome(ctx, waitCh, overflow, kill)
		}
	}

	// Retire both pipes. On an abnormal ending there is nothing left worth
	// waiting for, so the read ends close immediately; on a clean exit the
	// readers get the bound to drain what the CLI already wrote before the
	// same force-close applies to whatever descendant inherited the pipe.
	// A force-closed read returns os.ErrClosed, not io.EOF, so a reader cannot
	// tell a retired stream from a finished one on its own. readCapped makes
	// that call for each stream as it ends, so a stdout that reached EOF stays
	// parsable even when stderr's reader was the one still held.
	stopped := trigger != OutcomeUnspecified
	stdout, stderr := r.retire(stopped, sc.Kind, outR, errR, outCh, errCh)
	if stopped || diedUnderSignal(waitErr) {
		// The one cause readCapped cannot see. Ending the child closes every
		// write end, so its reader gets a genuine io.EOF and correctly reports
		// that the stream ended — while the reason it ended is that the
		// producer was stopped mid-write. From the reader's side a stopped
		// producer and a finished one are identical.
		//
		// The predicate is "the child did not finish under its own control",
		// not "this runner killed it". A timeout, a cancellation, and an
		// overflow are the likeliest causes and this layer knows them
		// first-hand, but the OOM killer, a supervisor, an operator's kill,
		// and a fault inside the CLI leave exactly the same prefix behind
		// exactly the same clean EOF — and the wait status says so.
		//
		// This over-marks a stream that had genuinely finished before the
		// child stopped: a strict adapter loses a diagnostic it could have
		// rendered. That is the safe direction, and the cost is paid only on
		// a run that already failed.
		stdout.forced, stderr.forced = true, true
	}

	// Overflow can also surface after the fact: a reader that filled its
	// buffer while the process was already exiting signals on a channel nobody
	// selected. Read the real signal rather than inferring one from
	// incompleteness, which by now also covers forced retirement.
	if trigger == OutcomeUnspecified && (stdout.overflow || stderr.overflow) {
		trigger = OutcomeOutputLimit
		kill()
	}
	// A clean exit whose output was cut off is deliberately NOT an error. Every
	// backend this seam drives spawns a daemon that inherits the write end and
	// outlives the command, so the retirement bound expiring is the normal shape
	// of a successful create — returning an error there would make the obvious
	// handling (retry) produce a duplicate session. BoundedOutput.Complete
	// carries it instead, and CopyBytesForParse hands the caller that flag
	// alongside the bytes.

	res := SensitiveResult{Stdout: stdout, Stderr: stderr, ExitCode: 0}
	if waitErr != nil {
		res.ExitCode = exitCodeOf(waitErr)
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

// awaitOutcome blocks until the process finishes, the context ends, or a
// stream overflows, killing and reaping on the latter two. It is the arm the
// caller reaches only after the non-blocking checks found nothing already
// ready, so its own uniform select is between genuinely concurrent events.
func (r *OSSensitiveRunner) awaitOutcome(ctx context.Context, waitCh chan error, overflow chan struct{}, kill func()) (Outcome, error) {
	select {
	case waitErr := <-waitCh:
		// The immediate CLI is done. A descendant may still hold a pipe; the
		// retirement below is what bounds that.
		return OutcomeUnspecified, waitErr
	case <-ctx.Done():
		trigger := OutcomeCanceled
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			trigger = OutcomeTimeout
		}
		kill()
		return trigger, <-waitCh
	case <-overflow:
		kill()
		return OutcomeOutputLimit, <-waitCh
	}
}

// diedUnderSignal reports whether the process was terminated rather than
// exiting. os.ProcessState.ExitCode returns -1 exactly when a process was
// ended by a signal, which is the portable way to ask — syscall.WaitStatus
// is not the same type on every platform this builds for.
//
// A process that was signalled did not choose its moment, so whatever it had
// written is a prefix. That is true whether the signal came from this runner
// or from the OOM killer, a supervisor, an operator, or a fault in the CLI.
func diedUnderSignal(waitErr error) bool {
	return waitErr != nil && exitCodeOf(waitErr) == -1
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
//
// It does not mark the streams it cuts off, deliberately. The interrupted Read
// returns os.ErrClosed, which readCapped already classifies as a stop short of
// the end, and a reader that finished before the close carries its own correct
// answer — so marking here would duplicate a decision already made.
//
// That covers every cause a reader can observe. It does not cover a producer
// that was stopped: once the child is reaped its write ends are closed too, so
// the reader sees a genuine io.EOF and cannot tell a producer that finished
// from one that was killed. RunSensitive marks that case — both when it did
// the killing and when something outside it did, which the wait status
// reports.
func (r *OSSensitiveRunner) retire(immediate bool, kind CommandKind, outR, errR *os.File, outCh, errCh <-chan BoundedOutput) (BoundedOutput, BoundedOutput) {
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
			// Log it: the bound expiring is the one source of latency in this
			// call that is not the backend's own, and an unattributed
			// multi-second pause is exactly what a future debugging session
			// would otherwise have to rediscover.
			slog.Warn("Retirement bound expired with a pipe still held; closing it.",
				"kind", kind.String(), "bound", r.bound())
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
	idle := 0
	// cut records that the stream stopped for a reason other than reaching its
	// end. Only io.EOF means the stream is whole; a force-close reports
	// os.ErrClosed, and a pipe whose peer died abnormally can report EIO. Both
	// leave a prefix that must not be parsed as a complete response, and this is
	// the only place that distinction is visible — downstream sees a delivered
	// BoundedOutput either way.
	cut := false
	for int64(n) < limit+1 {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			cut = !errors.Is(err, io.EOF)
			break
		}
		if m == 0 {
			// io.Reader permits (0, nil); os.File does not produce it, but a
			// reader that only ever did would spin here forever. Giving up is
			// not reaching the end, so it counts as a cut.
			if idle++; idle >= maxZeroProgressReads {
				cut = true
				break
			}
			continue
		}
		idle = 0
	}
	if int64(n) > limit {
		select {
		case overflow <- struct{}{}:
		default:
		}
		out <- BoundedOutput{buf: &outputBuf{data: buf[:limit]}, overflow: true}
		return
	}
	out <- BoundedOutput{buf: &outputBuf{data: buf[:n]}, forced: cut}
}

// closeAll retires pipe ends. Close errors are dropped deliberately: these are
// read/write ends of the runner's own pipes, a second Close is expected on the
// paths where both the timeout and the caller retire the same descriptor, and
// no caller decision depends on the result.
func closeAll(files ...*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}

// compile-time proof the production runner satisfies the seam.
var _ SensitiveRunner = (*OSSensitiveRunner)(nil)
