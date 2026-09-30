package exec

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec/internal/sealed"
	"github.com/cameronsjo/forgectl/internal/exec/internal/validated"
)

// defaultRetireBound is how long the runner waits for a stream's reader to
// finish on its own before force-closing the read end. It exists because the
// immediate CLI is not the only process that can hold a pipe: anything it
// spawned inherits the write end and can keep it open long after the CLI
// itself has been reaped. Without a bound, "the command finished" and "the
// call returns" are different events separated by a descendant's lifetime.
const defaultRetireBound = 2 * time.Second

// defaultKillDrainBound is how long, after a kill, the runner lets each
// stream's reader take what the child had already written before the read
// ends are force-closed (forgectl#794). Once the child is reaped its write
// ends are closed, so a reader with bytes still in the pipe reaches io.EOF
// within microseconds; the bound only matters when a descendant still holds
// a write end, and then it caps the extra wait. It is short, because a kill
// means the caller is already done waiting, and it never exceeds the
// retirement bound.
const defaultKillDrainBound = 250 * time.Millisecond

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

	// killDrainBound overrides defaultKillDrainBound; tests set it directly
	// so the drain can be proven without racing a short window.
	killDrainBound time.Duration

	// started counts successful fork/execs. It exists because "this command
	// never ran" is not observable from the child: a refusal that kills, or a
	// pre-start check that never forks, both leave no trace in the child's own
	// side effects — the kill wins that race every time. Counting at the one
	// place a process comes into existence is what makes the refusal provable
	// rather than assumed.
	started atomic.Int64

	// stdoutTap, when set, wraps the stdout read end before its reader sees
	// it. It is a test seam and nil in production. It lets a test act on the
	// event "the parent has read the child's bytes", which is the event a kill
	// has to follow for those bytes to be captured. An abnormal ending used
	// to force-close the read ends at once, dropping bytes the child wrote
	// but the reader had not yet taken (forgectl#787); it now drains for
	// drainBound first (forgectl#794), and a test delays the reader through
	// this seam to prove it.
	stdoutTap func(io.Reader) io.Reader
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

// drainBound is the post-kill drain window: killDrainBound or its default,
// never longer than the retirement bound.
func (r *OSSensitiveRunner) drainBound() time.Duration {
	d := defaultKillDrainBound
	if r.killDrainBound > 0 {
		d = r.killDrainBound
	}
	return min(d, r.bound())
}

// buildEnv splits muts into what startSealed hands sealed.Start: a fresh copy
// of the captured environment with every occurrence of each mutated key
// dropped, and one replacement per replace mutation, in mutation order.
// Removing all occurrences matters: a duplicated key in the inherited
// environment would otherwise leave the stale entry in place on some
// platforms' lookup order. Entries the mutation set does not name are copied
// byte-exact and never inspected. Key matching is case-sensitive, which is
// correct for the POSIX platforms forgectl targets.
//
// It reads keys only. A replacement's value stays sealed until sealed.Start
// appends it as KEY=value, after every inherited entry, so the order of the
// final environment is exactly what it was when this function built it whole.
func (r *OSSensitiveRunner) buildEnv(muts []validated.Env) ([]string, []sealed.EnvVar) {
	if len(muts) == 0 {
		out := make([]string, len(r.env))
		copy(out, r.env)
		return out, nil
	}
	drop := make(map[string]struct{}, len(muts))
	for _, m := range muts {
		drop[m.Key] = struct{}{}
	}
	out := make([]string, 0, len(r.env)+len(muts))
	for _, entry := range r.env {
		if _, mutated := drop[envKeyOf(entry)]; mutated {
			continue
		}
		out = append(out, entry)
	}
	var set []sealed.EnvVar
	for _, m := range muts {
		if m.Op == validated.EnvOpReplace {
			set = append(set, sealed.EnvVar{Key: m.Key, Value: m.Value})
		}
	}
	return out, set
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

// startSealed starts vc's process through sealed.Start, which is the reveal
// boundary: the only function that takes a SecretArg, Arg or EnvMutation
// payload out of its wrapper, and it puts every payload into the child
// process and nowhere else (forgectl#854). What comes back is a *sealed.Proc,
// which can only wait for and kill the process. This package never holds an
// *exec.Cmd, or anything else carrying a plaintext payload. (The inherited
// environment r.env is plaintext by design; it holds no payload.)
//
// It accepts only a validated.Command, which only validated.New builds, from
// a copy it checked, so what it starts passed validation, and a later write
// to the caller's SensitiveCommand cannot reach it. The validated package's
// boundary makes that the compiler's property (forgectl#888). Only RunSensitive
// may call it, once, as a direct call and never as a func value, so every
// launch goes through the runner's pipes and bounds
// (TestSealedStartHasOneCaller, TestValidateDominatesStartSealed).
// sealed.Start refuses a non-absolute path on its own as defense in depth. It
// is a plain func, not a method, on purpose: a package-level func is
// reachable only by naming it, so the test's Uses walk sees every route,
// where a method could be reached through an interface value the walk would
// not attribute to it.
//
// What the compiler enforces, against ordinary Go (calls, method values,
// interfaces, generic constraints, conversions):
//
//   - a payload sits in a sealed.Value whose reveal is unexported inside
//     internal/exec/internal/sealed, so no code outside sealed can call it,
//     this package included;
//   - the *exec.Cmd that sealed.Start builds never leaves sealed through an
//     exported name, and Proc's fields are closures over it;
//   - Go's internal-package rule lets nothing outside internal/exec import
//     sealed at all.
//
// What the compiler does NOT enforce: reflect reads an unexported field, and
// a closure's captured variables, without importing unsafe. Value.Addr on the
// field, then Value.UnsafePointer, then a conversion of the pointer to *T
// reads a sealed.Value's payload or the Cmd behind a Proc, from any package
// holding one. The guard tests below refuse it; they do not make it
// impossible.
//
// Residual risk, which no guard here closes: code in the module can read
// its own process memory through the operating system with neither reflect
// nor unsafe, for example /proc/self/mem, or a raw syscall handed a uintptr,
// at an address taken from fmt's %p or text/template's printing of a
// pointer. %p and a template only give an address; the read is the part that
// matters, and it is a review property.
//
// What the guard tests enforce, as the backstop for what the compiler cannot
// see:
//
//   - a //go:linkname directive or an "unsafe" import reaches an unexported
//     symbol in any package, internal or not, so a linkname to sealed's
//     unexported command would still read a payload
//     (TestNoFileReachesPastTheTypeSystem refuses both module-wide);
//   - the exported surface of this package, of sealed (Proc among it, with
//     no accessor that reads a payload), and of tmuxesc, with every file and
//     its build constraint, is pinned in testdata/exported_api.golden on
//     every platform in guardPlatforms (TestExportedAPI), so a new export
//     that hands a payload out, here or in sealed, fails until reviewed;
//   - outside this package, only the files in transformCallers name
//     MapOpaque, Transform or TmuxDirOperand
//     (TestNoCallerCodeReceivesAnOpaquePayload);
//   - tmuxesc, which sealed hands a payload to, stays a leaf of pure string
//     funcs (TestTmuxescIsALeaf);
//   - a package outside internal/exec that imports sealed fails to build
//     (TestSealedIsUnimportableOutsideExec), which proves the rule the rest
//     relies on;
//   - no package but this one (and sealed's own test binary) imports sealed,
//     a subpackage of internal/exec included, which the internal-package
//     rule would admit (TestOnlyExecImportsSealed);
//   - the reflect route above: no production file in the module holds an
//     unsafe.Pointer-typed value without importing "unsafe", or uses
//     reflect.Value's Addr, UnsafeAddr, UnsafePointer or Pointer (or a
//     method that reaches them by name), on every guard platform
//     (TestNoFileReadsMemoryThroughReflect, forgectl#888);
//   - in this package's production files, sealed.Start is named only here
//     (TestSealedStartHasOneCaller). A package-level func can be reached only
//     by naming it, so this check has no interface or method-value gap.
//
// What neither enforces: the child is the caller's chosen backend, and a
// backend that echoes its argv or environment to stdout hands a payload back
// through the result by design. The seam keeps payloads out of logs and error
// strings; it does not vet the program it runs.
//
// sealed.Start builds with exec.Command rather than exec.CommandContext
// deliberately. CommandContext kills on context completion but does not own
// what happens next, and this runner does: it must kill, reap, and retire two
// pipe ends in a defined order and within a tested bound, and it must
// distinguish a deadline from a cancellation from an output-limit kill in the
// returned outcome. Handing half of that to CommandContext would leave two
// killers racing for the same process. The context check that CommandContext
// performs before Start is done explicitly in RunSensitive instead.
//
// validate has already required an absolute path, so no PATH lookup happens
// — which matters, because LookPath reads the live process PATH rather than
// this runner's captured environment.
func startSealed(r *OSSensitiveRunner, vc validated.Command, stdout, stderr *os.File) (*sealed.Proc, error) {
	env, set := r.buildEnv(vc.Env())
	return sealed.Start(vc.Path(), vc.Args(), env, set, stdout, stderr)
}

// failedResult is what every never-ran path returns. ExitCode is -1, never 0:
// a command that did not run has no exit status, and 0 is the one value a
// caller reads as success.
//
// Both streams are marked cut off for the same reason. A zero-value
// BoundedOutput reports Complete() == true over zero bytes, which says "the
// stream ended and you have all of it" about a process that never existed —
// and this contract sends a parser to the completeness flag rather than to the
// error. Empty-and-complete is a valid answer from a command that ran and
// printed nothing; it must not also be the answer from one that never started.
func failedResult() SensitiveResult {
	return SensitiveResult{
		ExitCode: -1,
		Stdout:   BoundedOutput{forced: true},
		Stderr:   BoundedOutput{forced: true},
	}
}

// RunSensitive runs one bounded command. Nothing it logs or returns can render
// the path, the argv, or the environment, and stdout and stderr are captured
// concurrently into fixed buffers that cannot exceed the caps.
//
// An overflow, a timeout, and a cancellation kill and reap the immediate CLI
// and retire both pipe ends. A descendant still holding a pipe after the CLI
// exited on its own is different: nothing is killed there — the read ends are
// closed and the call returns, leaving the descendant to its own lifetime.
// Either way a stream that stopped short is reported incomplete.
//
// That incompleteness rides BoundedOutput.Complete, never the returned error.
// On a zero-exit run there is no error to check: a backend that daemonises
// leaves a descendant holding the pipe, which is a successful run by every
// measure except that we stopped listening. A caller that parses output must
// read the completeness flag — CopyBytesForParse returns it alongside the bytes
// so it cannot be skipped by accident.
func (r *OSSensitiveRunner) RunSensitive(ctx context.Context, sc SensitiveCommand) (SensitiveResult, error) {
	vc, err := sc.validated()
	if err != nil {
		slog.Debug("Refusing sensitive command before start.", "cmd", sc, "reason", err.Error())
		return failedResult(), newSensitiveError(sc.Kind, OutcomeInvalid, failedResult(), err.Error())
	}

	// An already-done context must not buy a fork/exec. exec.CommandContext
	// performs this check internally; since this runner deliberately does not
	// use it (see startSealed), the check is explicit here.
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

	slog.Debug("Preparing to run sensitive command.", "cmd", sc)
	start := time.Now()

	proc, err := startSealed(r, vc, outW, errW)
	if err != nil {
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
	var stdoutSrc io.Reader = outR
	if r.stdoutTap != nil {
		stdoutSrc = r.stdoutTap(outR)
	}
	go readCappedMode(stdoutSrc, sc.StdoutCap, sc.StdoutMode, outCh, overflow)
	go readCapped(errR, sc.StderrCap, errCh, overflow)

	waitCh := make(chan error, 1)
	go func() { waitCh <- proc.Wait() }()

	var (
		killOnce sync.Once
		trigger  = OutcomeUnspecified
		waitErr  error
	)
	// The Kill error is dropped deliberately: the only failure it reports is
	// os.ErrProcessDone, which is the state kill was trying to reach. Every
	// caller of kill follows it with a Wait, which carries the real outcome.
	kill := func() { killOnce.Do(func() { _ = proc.Kill() }) }

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

	// Retire both pipes. On an abnormal ending the readers get a short drain
	// window (drainBound) to take what the child had already written, so a
	// kill does not drop a prefix still sitting in the pipe (forgectl#794);
	// on a clean exit they get the full retirement bound. Either way the same
	// force-close then applies to whatever descendant inherited the pipe.
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
// exiting. A process that was signalled did not choose its moment, so whatever
// it had written is a prefix — true whether the signal came from this runner
// or from the OOM killer, a supervisor, an operator, or a fault in the CLI.
//
// It asks the ProcessState rather than the exit code. os.ProcessState.ExitCode
// returns -1 exactly when a process was ended by a signal, which is the
// portable way to ask — syscall.WaitStatus is not the same type on every
// platform this builds for. But exitCodeOf also returns -1 when the error is
// not an *exec.ExitError at all: a Wait that failed for its own reasons, a
// process that never started. Routing through it would call those signal
// deaths, so the ExitError has to be unwrapped first and its state asked
// directly. The distinction is not academic — it is the difference between
// "the child was killed" and "we could not find out what happened to it".
func diedUnderSignal(waitErr error) bool {
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) || exitErr.ProcessState == nil {
		return false
	}
	return exitErr.ExitCode() == -1
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
// stopped (the child was killed) shortens the wait from the retirement
// bound to drainBound, and the expiry is logged at Debug rather than Warn:
// the caller already chose to stop, so the wait is only a bounded chance for
// the readers to collect bytes the child wrote before the kill. Before
// forgectl#794 a kill force-closed at once and dropped them.
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
func (r *OSSensitiveRunner) retire(stopped bool, kind CommandKind, outR, errR *os.File, outCh, errCh <-chan BoundedOutput) (BoundedOutput, BoundedOutput) {
	bound := r.bound()
	if stopped {
		bound = r.drainBound()
	}
	timer := time.NewTimer(bound)
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
			if stopped {
				slog.Debug("Post-kill drain window expired with a pipe still held; closing it.",
					"kind", kind.String(), "bound", bound)
			} else {
				slog.Warn("Retirement bound expired with a pipe still held; closing it.",
					"kind", kind.String(), "bound", bound)
			}
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

func readCappedMode(r io.Reader, limit int64, mode CaptureMode, out chan<- BoundedOutput, overflow chan<- struct{}) {
	if mode == CaptureRaw {
		readCapped(r, limit, out, overflow)
		return
	}
	data, err := projectCmuxWorkspaceList(r)
	if err != nil {
		// Keep draining after a malformed document so the child cannot block on
		// a full pipe while RunSensitive waits for it. Empty complete output is
		// deliberately invalid JSON, so the adapter reports an unreadable reply.
		_, drainErr := io.Copy(io.Discard, r)
		out <- BoundedOutput{buf: &outputBuf{}, forced: drainErr != nil}
		return
	}
	if int64(len(data)) > limit {
		select {
		case overflow <- struct{}{}:
		default:
		}
		out <- BoundedOutput{buf: &outputBuf{data: data[:limit]}, overflow: true}
		return
	}
	out <- BoundedOutput{buf: &outputBuf{data: data}}
}

type projectedCmuxWorkspace struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// projectCmuxWorkspaceList walks the JSON token stream instead of decoding the
// envelope wholesale. Unknown values are consumed and discarded one token at
// a time, so large conversation, port, remote, and state fields never enter the
// retained result; the returned projection grows only with the match set. The
// decoder can still hold its current token, so peak working memory is bounded
// by the largest single JSON token plus that projection, not by the projection
// alone. cmux currently emits the heavy data as many ordinary-sized tokens.
func projectCmuxWorkspaceList(r io.Reader) ([]byte, error) {
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("cmux workspace listing is not an object")
	}
	var workspaces []projectedCmuxWorkspace
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, errors.New("cmux workspace listing has a non-string key")
		}
		if name != "workspaces" {
			if err := skipJSONValue(dec); err != nil {
				return nil, err
			}
			continue
		}
		workspaces, err = decodeProjectedCmuxWorkspaces(dec)
		if err != nil {
			return nil, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if tok, err := dec.Token(); !errors.Is(err, io.EOF) || tok != nil {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("cmux workspace listing has trailing JSON")
	}
	// termsafe:allow-raw-json internal cmux protocol projection, never command output
	return json.Marshal(struct {
		Workspaces []projectedCmuxWorkspace `json:"workspaces"`
	}{Workspaces: workspaces})
}

func decodeProjectedCmuxWorkspaces(dec *json.Decoder) ([]projectedCmuxWorkspace, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return nil, nil
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		return nil, errors.New("cmux workspaces field is not an array")
	}
	var rows []projectedCmuxWorkspace
	for dec.More() {
		row, err := decodeProjectedCmuxWorkspace(dec)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return rows, nil
}

func decodeProjectedCmuxWorkspace(dec *json.Decoder) (projectedCmuxWorkspace, error) {
	tok, err := dec.Token()
	if err != nil {
		return projectedCmuxWorkspace{}, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return projectedCmuxWorkspace{}, errors.New("cmux workspace row is not an object")
	}
	var row projectedCmuxWorkspace
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return projectedCmuxWorkspace{}, err
		}
		name, ok := key.(string)
		if !ok {
			return projectedCmuxWorkspace{}, errors.New("cmux workspace row has a non-string key")
		}
		switch name {
		case "id":
			err = dec.Decode(&row.ID)
		case "description":
			err = dec.Decode(&row.Description)
		default:
			err = skipJSONValue(dec)
		}
		if err != nil {
			return projectedCmuxWorkspace{}, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return projectedCmuxWorkspace{}, err
	}
	return row, nil
}

func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	for dec.More() {
		if delim == '{' {
			if _, err := dec.Token(); err != nil {
				return err
			}
		}
		if err := skipJSONValue(dec); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
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
