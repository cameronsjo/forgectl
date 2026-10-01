package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec/internal/sealed"
	"github.com/cameronsjo/forgectl/internal/exec/internal/validated"
)

// helperModeEnv turns this test binary into the child process the runner
// executes. Every process-behavior test below drives the real runner against a
// real child, and the child is this binary re-invoked in a mode — so the suite
// needs no external binary and still exercises fork/exec, pipes, and signals
// for real.
const helperModeEnv = "FORGECTL_SENSITIVE_HELPER_MODE"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperModeEnv); mode != "" {
		os.Exit(helperMain(mode))
	}
	os.Exit(m.Run())
}

func helperMain(mode string) int {
	verb, arg, _ := strings.Cut(mode, ":")
	switch verb {
	case "ok":
		_, _ = fmt.Fprint(os.Stdout, arg)
		return 0
	case "fail":
		_, _ = fmt.Fprint(os.Stderr, arg)
		return 3
	case "flood":
		return floodTo(os.Stdout, arg)
	case "cmuxlist":
		n, err := strconv.Atoi(arg)
		if err != nil || n < 0 {
			return 98
		}
		_, _ = fmt.Fprint(os.Stdout, `{"window_ref":"window:1","workspaces":[{"id":"123E4567-E89B-12D3-A456-426614174000","description":"fc-surface-test","latest_conversation_message":"`)
		_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", n))
		_, _ = fmt.Fprint(os.Stdout, `","remote":{"detail":{"nested":[1,2,3]}}}]}`)
		return 0
	case "floodstderr":
		if code := floodTo(os.Stderr, arg); code != 0 {
			return code
		}
		_, _ = fmt.Fprint(os.Stdout, "stdout-still-flowing")
		return 0
	case "sleep":
		d, err := time.ParseDuration(arg)
		if err != nil {
			return 98
		}
		time.Sleep(d)
		return 0
	case "spawn":
		return spawnHolder(arg)
	case "partial":
		// Write, then stall, then write again. A caller that kills during the
		// stall gets a prefix — and gets it after a clean io.EOF, because the
		// kill closes this process's write ends too.
		_, _ = fmt.Fprint(os.Stdout, "PARTIAL")
		d, err := time.ParseDuration(arg)
		if err != nil {
			return 98
		}
		time.Sleep(d)
		_, _ = fmt.Fprint(os.Stdout, "-REST")
		return 0
	case "partialmark":
		// Write a prefix, then leave durable evidence that it was written,
		// then stall until killed. The marker lets a test kill the child
		// once the bytes are in the pipe, without the parent reading them.
		_, _ = fmt.Fprint(os.Stdout, "PARTIAL")
		f, err := os.Create(filepath.Clean(arg)) //nolint:gosec // G703: a test fixture path the test itself passes
		if err != nil {
			return 95
		}
		_ = f.Close()
		time.Sleep(60 * time.Second)
		return 0
	case "selfkill":
		// Write, then die to a signal this runner did not send — the OOM
		// killer, a supervisor, or a fault in the CLI itself all look like
		// this from the parent's side.
		_, _ = fmt.Fprint(os.Stdout, arg)
		self, err := os.FindProcess(os.Getpid())
		if err != nil {
			return 94
		}
		_ = self.Signal(os.Kill)
		time.Sleep(10 * time.Second) // unreachable; the signal lands first
		return 93
	case "touch":
		// Durable evidence that fork/exec happened. A test asserting "this
		// never ran" cannot rely on captured output: a kill races the child's
		// first write, so an empty stdout is also what a process that started
		// and died promptly looks like.
		f, err := os.Create(arg)
		if err != nil {
			return 95
		}
		_ = f.Close()
		return 0
	case "argv":
		_, _ = fmt.Fprint(os.Stdout, strings.Join(os.Args, "\x00"))
		return 0
	case "env":
		for _, key := range strings.Split(arg, ",") {
			if v, ok := os.LookupEnv(key); ok {
				_, _ = fmt.Fprintf(os.Stdout, "%s=%s\n", key, v)
			} else {
				_, _ = fmt.Fprintf(os.Stdout, "%s=<unset>\n", key)
			}
		}
		return 0
	}
	return 99
}

func floodTo(w *os.File, arg string) int {
	total, err := strconv.Atoi(arg)
	if err != nil {
		return 98
	}
	block := bytes.Repeat([]byte("x"), 4096)
	for written := 0; written < total; written += len(block) {
		if _, err := w.Write(block); err != nil {
			return 1
		}
	}
	return 0
}

// spawnHolder starts a descendant that inherits this process's stdout and
// stderr, then exits immediately. The descendant keeps the pipe write ends
// open long after the immediate child has been reaped — the exact shape that
// makes "the command finished" and "the call returns" different events.
func spawnHolder(arg string) int {
	self, err := os.Executable()
	if err != nil {
		return 97
	}
	child := exec.Command(self)
	child.Env = append(os.Environ(), helperModeEnv+"=sleep:"+arg)
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		return 96
	}
	_, _ = fmt.Fprint(os.Stdout, "parent-done")
	return 0
}

// helperRunner builds a runner whose captured environment puts this test
// binary into the requested mode, plus any extra entries the test needs to see
// survive (or not survive) the mutation policy.
func helperRunner(t *testing.T, mode string, retire time.Duration, extra ...string) (*OSSensitiveRunner, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	env := append(os.Environ(), helperModeEnv+"="+mode)
	env = append(env, extra...)
	return &OSSensitiveRunner{env: env, retireBound: retire}, self
}

func helperCommand(kind CommandKind, path string, caps int64, env ...EnvMutation) SensitiveCommand {
	return SensitiveCommand{
		Kind:      kind,
		Path:      Secret(path),
		Env:       env,
		StdoutCap: caps,
		StderrCap: caps,
	}
}

func TestRunSensitive_CapturesBothStreamsOnSuccess(t *testing.T) {
	runner, self := helperRunner(t, "ok:hello-from-the-child", defaultRetireBound)
	res, err := runner.RunSensitive(context.Background(), helperCommand(KindTmuxProbe, self, 4096))
	if err != nil {
		t.Fatalf("RunSensitive: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	data, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		t.Error("a short stdout reported itself truncated")
	}
	if string(data) != "hello-from-the-child" {
		t.Errorf("stdout = %q", data)
	}
	if res.Stderr.Len() != 0 {
		t.Errorf("stderr = %d bytes, want 0", res.Stderr.Len())
	}
}

func TestRunSensitive_ClassifiesNonzeroExitAndKeepsStderr(t *testing.T) {
	runner, self := helperRunner(t, "fail:backend-said-no", defaultRetireBound)
	res, err := runner.RunSensitive(context.Background(), helperCommand(KindCmuxProbe, self, 4096))
	if !errors.Is(err, ErrNonzeroExit) {
		t.Fatalf("err = %v, want ErrNonzeroExit", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	data, complete := res.Stderr.CopyBytesForParse()
	if !complete || string(data) != "backend-said-no" {
		t.Errorf("stderr = %q complete=%v; the result must ride along with the typed error", data, complete)
	}
	var se *SensitiveError
	if !errors.As(err, &se) || se.Outcome != OutcomeExit || se.StderrBytes != len("backend-said-no") {
		t.Errorf("error metadata did not describe the failure: %v", err)
	}
}

// TestRunSensitive_StdoutOverflowKillsAndMarksIncomplete: the overflow kills
// the producer rather than draining it. The kill shows in the exit status
// (-1, ended by a signal), not in a wall-clock bound, which host load alone
// could fail (forgectl#919); the call only has to return inside a generous
// hang bound.
//
// Mutation: drop kill() from both overflow arms (RunSensitive's and
// awaitOutcome's), and the flood blocks on a pipe nobody reads, so the call
// does not return within sensitiveHangBound.
func TestRunSensitive_StdoutOverflowKillsAndMarksIncomplete(t *testing.T) {
	const limit = 8192
	runner, self := helperRunner(t, "flood:400000", defaultRetireBound)

	res, _, err := runSensitiveWithin(context.Background(), t, runner, helperCommand(KindTmuxSnapshot, self, limit))

	if !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("err = %v, want ErrOutputLimit", err)
	}
	if res.Stdout.Len() != limit {
		t.Errorf("retained %d bytes, want exactly the cap %d", res.Stdout.Len(), limit)
	}
	if res.Stdout.Complete() {
		t.Error("overflowed stdout reported itself complete")
	}
	if _, complete := res.Stdout.CopyBytesForParse(); complete {
		t.Error("overflow bytes were offered to a parser as a complete schema")
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1: the runner should kill the producer rather than drain it", res.ExitCode)
	}
}

// sensitiveHangBound is how long a test waits for RunSensitive before calling
// it hung. It is a hang bound, not a promptness check (forgectl#919): it sits
// far above any run that works, so host load cannot fail it, and below the
// 30 s or more that a helper nobody killed or retired runs for, so a missing
// kill or retirement still fails it. Promptness is shown by what the result
// says happened instead: an exit status of -1 for a kill, an incomplete
// stream for a retirement.
const sensitiveHangBound = 20 * time.Second

// runSensitiveWithin runs sc and fails t if the call has not returned within
// sensitiveHangBound. It also returns how long the call took, for a test
// whose subject is the order of two bounds rather than promptness.
func runSensitiveWithin(ctx context.Context, t *testing.T, runner *OSSensitiveRunner, sc SensitiveCommand) (SensitiveResult, time.Duration, error) {
	t.Helper()
	type outcome struct {
		res     SensitiveResult
		err     error
		elapsed time.Duration
	}
	done := make(chan outcome, 1)
	go func() {
		start := time.Now()
		res, err := runner.RunSensitive(ctx, sc)
		done <- outcome{res, err, time.Since(start)}
	}()
	select {
	case o := <-done:
		return o.res, o.elapsed, o.err
	case <-time.After(sensitiveHangBound):
		t.Fatalf("RunSensitive did not return within %v", sensitiveHangBound)
		return SensitiveResult{}, 0, nil
	}
}

func TestRunSensitive_CmuxProjectionIgnoresLargeUnusedFields(t *testing.T) {
	runner, self := helperRunner(t, "cmuxlist:200000", defaultRetireBound)
	cmd := helperCommand(KindCmuxSnapshot, self, 4096)
	cmd.StdoutMode = CaptureCmuxWorkspaceList
	res, err := runner.RunSensitive(context.Background(), cmd)
	if err != nil {
		t.Fatalf("RunSensitive: %v", err)
	}
	raw, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		t.Fatal("projected workspace listing reported itself incomplete")
	}
	if len(raw) >= 4096 {
		t.Fatalf("projected listing retained %d bytes, want less than cap", len(raw))
	}
	if strings.Contains(string(raw), "latest_conversation_message") || strings.Contains(string(raw), "remote") {
		t.Fatalf("projected listing retained an unused field: %s", raw)
	}
	var got struct {
		Workspaces []projectedCmuxWorkspace `json:"workspaces"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("projected JSON: %v", err)
	}
	if len(got.Workspaces) != 1 || got.Workspaces[0].ID != "123E4567-E89B-12D3-A456-426614174000" || got.Workspaces[0].Description != "fc-surface-test" {
		t.Fatalf("projected workspaces = %+v", got.Workspaces)
	}
}

func TestRunSensitive_CmuxProjectionStillEnforcesOutputCap(t *testing.T) {
	runner, self := helperRunner(t, "cmuxlist:0", defaultRetireBound)
	cmd := helperCommand(KindCmuxSnapshot, self, 32)
	cmd.StdoutMode = CaptureCmuxWorkspaceList
	res, err := runner.RunSensitive(context.Background(), cmd)
	if !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("err = %v, want ErrOutputLimit", err)
	}
	if res.Stdout.Len() != 32 || res.Stdout.Complete() {
		t.Fatalf("projected stdout = %d bytes complete=%v, want capped incomplete output", res.Stdout.Len(), res.Stdout.Complete())
	}
}

func TestRunSensitive_CmuxProjectionMalformedReplyStaysUnreadable(t *testing.T) {
	runner, self := helperRunner(t, `ok:{"workspaces":[`, defaultRetireBound)
	cmd := helperCommand(KindCmuxSnapshot, self, 4096)
	cmd.StdoutMode = CaptureCmuxWorkspaceList
	res, err := runner.RunSensitive(context.Background(), cmd)
	if err != nil {
		t.Fatalf("RunSensitive: %v", err)
	}
	raw, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		t.Fatal("the whole malformed reply was read; it should be unreadable, not reported truncated")
	}
	var value any
	if json.Unmarshal(raw, &value) == nil {
		t.Fatalf("malformed input became readable projected JSON: %s", raw)
	}
}

func TestProjectCmuxWorkspaceListRejectsMalformedAndTrailingJSON(t *testing.T) {
	for _, input := range []string{
		`{"workspaces":[`,
		`{"workspaces":[]} true`,
		`{"workspaces":{}}`,
		`{"workspaces":[7]}`,
	} {
		if got, err := projectCmuxWorkspaceList(strings.NewReader(input)); err == nil {
			t.Errorf("projectCmuxWorkspaceList(%q) = %q, want error", input, got)
		}
	}
}

// TestRunSensitive_ReadsStreamsConcurrently would hang under a sequential
// reader: the child fills stderr past a pipe buffer before writing a single
// stdout byte, so a stdout-first reader blocks on a child that is itself
// blocked on stderr. Returning at all is the assertion.
func TestRunSensitive_ReadsStreamsConcurrently(t *testing.T) {
	const limit = 65536
	runner, self := helperRunner(t, "floodstderr:400000", defaultRetireBound)

	done := make(chan SensitiveResult, 1)
	errs := make(chan error, 1)
	go func() {
		res, err := runner.RunSensitive(context.Background(), helperCommand(KindHerdrSnapshot, self, limit))
		done <- res
		errs <- err
	}()

	select {
	case res := <-done:
		err := <-errs
		if !errors.Is(err, ErrOutputLimit) {
			t.Fatalf("err = %v, want ErrOutputLimit", err)
		}
		if res.Stderr.Len() != limit || res.Stderr.Complete() {
			t.Errorf("stderr = %d bytes complete=%v, want %d and incomplete", res.Stderr.Len(), res.Stderr.Complete(), limit)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("RunSensitive never returned; streams are not being read concurrently")
	}
}

// TestRunSensitive_TimeoutKillsAndClassifies: the deadline kills the child.
// The kill shows in the exit status (-1), and the call returns inside
// sensitiveHangBound, where the unkilled helper would sleep 60 s.
//
// Mutation: drop kill() from awaitOutcome's ctx.Done arm, and the call waits
// on the 60 s sleep past sensitiveHangBound.
func TestRunSensitive_TimeoutKillsAndClassifies(t *testing.T) {
	runner, self := helperRunner(t, "sleep:60s", defaultRetireBound)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	res, _, err := runSensitiveWithin(ctx, t, runner, helperCommand(KindCmuxCreate, self, 4096))

	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if errors.Is(err, ErrCanceled) {
		t.Error("a deadline was reported as a cancellation; the classes must stay distinct")
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1: the deadline did not kill the process", res.ExitCode)
	}
}

// TestRunSensitive_CancellationKillsAndClassifies: the cancellation kills
// the child, shown as TestRunSensitive_TimeoutKillsAndClassifies shows it.
//
// Mutation: drop kill() from awaitOutcome's ctx.Done arm, and the call waits
// on the 60 s sleep past sensitiveHangBound.
func TestRunSensitive_CancellationKillsAndClassifies(t *testing.T) {
	runner, self := helperRunner(t, "sleep:60s", defaultRetireBound)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	res, _, err := runSensitiveWithin(ctx, t, runner, helperCommand(KindCmuxCleanup, self, 4096))

	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("err = %v, want ErrCanceled", err)
	}
	if errors.Is(err, ErrTimeout) {
		t.Error("a cancellation was reported as a timeout; the classes must stay distinct")
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1: the cancellation did not kill the process", res.ExitCode)
	}
}

// TestRunSensitive_ReturnsWithinBoundWhenDescendantHoldsThePipe is the
// separately tested wait bound. The immediate CLI exits at once; a descendant
// it spawned keeps both pipe write ends open. Without the runner retiring the
// read ends itself, this call would block for the descendant's whole lifetime.
// That the runner retired the pipe shows in the incomplete stream, not in a
// wall-clock bound (forgectl#919): a stream the descendant let go of on its
// own would end at io.EOF, complete. The call only has to return inside
// sensitiveHangBound, below the descendant's 30 s.
//
// Mutation: in retire, never fire the timer (wait on the readers alone), and
// the call does not return within sensitiveHangBound.
func TestRunSensitive_ReturnsWithinBoundWhenDescendantHoldsThePipe(t *testing.T) {
	const retire = 300 * time.Millisecond
	runner, self := helperRunner(t, "spawn:30s", retire)

	res, elapsed, err := runSensitiveWithin(context.Background(), t, runner, helperCommand(KindTmuxCreate, self, 4096))

	// The immediate CLI exited 0, but a descendant held the pipe past the
	// bound, so the stream is a prefix. That is a successful command, not a
	// failed one — every backend this seam drives spawns a daemon that
	// inherits the write end, so an error here would make the ordinary create
	// path look like a failure and invite a duplicate-creating retry. The
	// prefix has to be visible all the same: a force-closed Read returns
	// os.ErrClosed rather than io.EOF, so without the explicit flag a parser
	// would receive truncated bytes marked whole.
	if err != nil {
		t.Fatalf("err = %v; a cut-off stream on a zero-exit run is not an error", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d; the immediate CLI exited cleanly", res.ExitCode)
	}
	if res.Stdout.Complete() {
		t.Error("a force-retired stream reported itself complete")
	}
	if elapsed < retire {
		t.Errorf("returned after %v, before the %v retirement bound; the bound is not being applied", elapsed, retire)
	}
	data, complete := res.Stdout.CopyBytesForParse()
	if complete {
		t.Error("CopyBytesForParse offered a force-retired prefix as a complete schema")
	}
	if !strings.Contains(string(data), "parent-done") {
		t.Errorf("stdout = %q; what the immediate child wrote before exiting must still be captured", data)
	}
}

// TestRunSensitive_KilledProducerLeavesAnIncompletePrefix covers the one cause
// of truncation a reader structurally cannot see. Killing the child closes its
// write ends, so the reader gets a genuine io.EOF and correctly reports that
// the stream ended — while the reason it ended is that the producer was
// stopped mid-write. Only the layer that killed it knows, and a caller parsing
// the bytes has to be told, because the seam's contract sends them to the
// completeness flag rather than to the error.
func TestRunSensitive_KilledProducerLeavesAnIncompletePrefix(t *testing.T) {
	// Each context fires once the PARENT has read the child's prefix, not
	// after a fixed delay and not when the child reports writing it. A fixed
	// 200 ms raced the child's startup (#661). A marker file the child wrote
	// after its prefix still raced, one step later: the kill force-closes the
	// read ends at once, so a prefix still sitting in the pipe, not yet taken
	// by a reader the scheduler had not run, was dropped and stdout came back
	// empty under load (#787). The tap on the runner's stdout reader closes
	// fired only after those bytes are in the reader's hands. A context whose
	// deadline is set when it is made cannot wait for an event, so the
	// deadline case uses a context that reports DeadlineExceeded once fired,
	// which is all the runner reads of it.
	cases := map[string]func(fired <-chan struct{}) context.Context{
		"deadline": func(fired <-chan struct{}) context.Context {
			return firedContext{Context: context.Background(), done: fired, err: context.DeadlineExceeded}
		},
		"cancellation": func(fired <-chan struct{}) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				<-fired
				cancel()
			}()
			return ctx
		},
	}

	for name, mkCtx := range cases {
		runner, self := helperRunner(t, "partial:60s", defaultRetireBound)
		fired := make(chan struct{})
		runner.stdoutTap = func(r io.Reader) io.Reader {
			return &firstBytesTap{r: r, want: len("PARTIAL"), fired: fired}
		}

		res, err := runner.RunSensitive(mkCtx(fired), helperCommand(KindTmuxCreate, self, 4096))

		if err == nil {
			t.Errorf("%s: expected the kill to be reported", name)
		}
		data, complete := res.Stdout.CopyBytesForParse()
		if string(data) != "PARTIAL" {
			t.Errorf("%s: stdout = %q, want the prefix the child managed to write", name, data)
		}
		if complete {
			t.Errorf("%s: a killed producer's prefix reported itself complete", name)
		}
		if res.Stdout.Complete() {
			t.Errorf("%s: Complete() disagreed with CopyBytesForParse", name)
		}
		if res.Stderr.Complete() {
			t.Errorf("%s: the other stream of a killed process reported itself complete", name)
		}
	}

	// The same helper run to completion reports complete, so "incomplete" above
	// is the kill and not something the fixture always produces.
	runner, self := helperRunner(t, "partial:1ms", defaultRetireBound)
	res, err := runner.RunSensitive(context.Background(), helperCommand(KindTmuxCreate, self, 4096))
	if err != nil {
		t.Fatalf("control run failed: %v", err)
	}
	data, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		t.Errorf("an uninterrupted run reported itself incomplete; the assertions above prove nothing")
	}
	if string(data) != "PARTIAL-REST" {
		t.Errorf("control stdout = %q, want the whole stream", data)
	}
}

// firedContext is done once fired closes, and from then on reports err. It
// lets a test end a run with context.DeadlineExceeded at a moment it chooses.
type firedContext struct {
	context.Context
	done <-chan struct{}
	err  error
}

func (c firedContext) Done() <-chan struct{} { return c.done }

func (c firedContext) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

// firstBytesTap passes reads through and closes fired once want bytes have
// been returned to the reader, so a test can kill the child at the moment its
// prefix is in the parent's hands rather than merely in the pipe. Only the one
// reader goroutine calls Read, so it needs no lock.
type firstBytesTap struct {
	r     io.Reader
	want  int
	got   int
	fired chan<- struct{}
}

func (t *firstBytesTap) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if t.got < t.want {
		if t.got += n; t.got >= t.want {
			close(t.fired)
		}
	}
	return n, err
}

// diedUnderSignal must answer from the process state, not from the exit code.
// exitCodeOf returns -1 for a signal death AND for any error that does not wrap
// an *exec.ExitError — a Wait that failed for its own reasons, a process that
// never started. Reading -1 as "signalled" collapses "the child was killed"
// into "we could not find out what happened to it", and marks streams as
// prefixes on evidence that says nothing about the child at all.
func TestDiedUnderSignal_AnswersFromProcessStateNotExitCode(t *testing.T) {
	signalled := func() error {
		self, err := os.Executable()
		if err != nil {
			t.Fatalf("os.Executable: %v", err)
		}
		cmd := exec.Command(self)
		cmd.Env = append(os.Environ(), helperModeEnv+"=sleep:30s")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		_ = cmd.Process.Kill()
		return cmd.Wait()
	}()

	cases := map[string]struct {
		err  error
		want bool
	}{
		"signal death":           {err: signalled, want: true},
		"nil":                    {err: nil, want: false},
		"plain error":            {err: errors.New("wait failed for its own reasons"), want: false},
		"wrapped plain error":    {err: fmt.Errorf("context: %w", errors.New("boom")), want: false},
		"exec.ErrNotFound":       {err: exec.ErrNotFound, want: false},
		"nil-state ExitError":    {err: &exec.ExitError{}, want: false},
		"wrapped signal ExitErr": {err: fmt.Errorf("wrapped: %w", signalled), want: true},
	}

	for name, tc := range cases {
		if got := diedUnderSignal(tc.err); got != tc.want {
			t.Errorf("%s: diedUnderSignal = %v, want %v (err=%v)", name, got, tc.want, tc.err)
		}
	}

	// Anti-vacuity: the fixture must really be a signal death, or "signal
	// death" above proves nothing about signals.
	if exitCodeOf(signalled) != -1 {
		t.Fatalf("fixture did not produce a signal death: exit=%d", exitCodeOf(signalled))
	}
}

// A command that never ran has no stream to be complete. The zero-value
// BoundedOutput reports Complete() == true over zero bytes, and this contract
// sends a parser to the flag rather than to the error — so that reads as "the
// backend returned an empty response". Empty-and-complete is a legitimate
// answer from a command that ran and printed nothing; it must not also be the
// answer from one that never started. Every refusal shares failedResult, so a
// future refusal path inherits the guarantee rather than reintroducing the bug.
func TestRunSensitive_NeverRanReportsNoCompleteStream(t *testing.T) {
	_, self := helperRunner(t, "ok:unreachable", defaultRetireBound)

	doneCtx, cancel := context.WithCancel(context.Background())
	cancel()

	cases := map[string]func() (SensitiveResult, error){
		"already-done context": func() (SensitiveResult, error) {
			r, s := helperRunner(t, "ok:unreachable", defaultRetireBound)
			return r.RunSensitive(doneCtx, helperCommand(KindTmuxCreate, s, 4096))
		},
		"refused before start": func() (SensitiveResult, error) {
			r, _ := helperRunner(t, "ok:unreachable", defaultRetireBound)
			return r.RunSensitive(context.Background(), helperCommand(KindTmuxCreate, "relative/path", 4096))
		},
	}

	for name, run := range cases {
		res, err := run()
		if err == nil {
			t.Fatalf("%s: expected a refusal", name)
		}
		if res.ExitCode != -1 {
			t.Errorf("%s: ExitCode = %d, want -1", name, res.ExitCode)
		}
		if res.Stdout.Complete() {
			t.Errorf("%s: stdout of a command that never ran reported itself complete", name)
		}
		if res.Stderr.Complete() {
			t.Errorf("%s: stderr of a command that never ran reported itself complete", name)
		}
		if _, complete := res.Stdout.CopyBytesForParse(); complete {
			t.Errorf("%s: a parser was offered zero bytes as a complete stream", name)
		}
	}

	// A command that ran and printed nothing is still complete: "empty" and
	// "never ran" have to stay distinguishable in the direction that matters.
	empty, _ := helperRunner(t, "ok:", defaultRetireBound)
	res, err := empty.RunSensitive(context.Background(), helperCommand(KindTmuxCreate, self, 4096))
	if err != nil {
		t.Fatalf("control run: %v", err)
	}
	if !res.Stdout.Complete() {
		t.Error("a command that ran and printed nothing reported itself truncated")
	}
}

// TestRunSensitive_ForeignSignalAlsoLeavesAnIncompletePrefix covers the same
// truncation as the kill test above, arriving from outside. The runner never
// pulls the trigger here, so a predicate keyed on "did I kill it" misses this
// entirely — and the OOM killer, a supervisor, an operator, and a fault in the
// CLI all produce it. The child's wait status is what says the producer did
// not choose its moment.
func TestRunSensitive_ForeignSignalAlsoLeavesAnIncompletePrefix(t *testing.T) {
	runner, self := helperRunner(t, "selfkill:PARTIAL", defaultRetireBound)

	res, err := runner.RunSensitive(context.Background(), helperCommand(KindTmuxCreate, self, 4096))
	if err == nil {
		t.Fatal("a child that died to a signal reported success")
	}
	if errors.Is(err, ErrTimeout) || errors.Is(err, ErrCanceled) {
		t.Errorf("err = %v; this runner did not end the process", err)
	}

	data, complete := res.Stdout.CopyBytesForParse()
	if string(data) != "PARTIAL" {
		t.Errorf("stdout = %q, want the prefix the child managed to write", data)
	}
	if complete {
		t.Error("a signalled producer's prefix reported itself complete")
	}

	// A child that exits nonzero under its own control is a different thing:
	// it chose its moment, so its output is whole and must stay parsable.
	// Without this, marking every failed run incomplete would pass the check
	// above while telling every caller its diagnostics are untrustworthy.
	failRunner, failSelf := helperRunner(t, "fail:backend-said-no", defaultRetireBound)
	failRes, failErr := failRunner.RunSensitive(context.Background(), helperCommand(KindTmuxProbe, failSelf, 4096))
	if !errors.Is(failErr, ErrNonzeroExit) {
		t.Fatalf("control run: err = %v, want ErrNonzeroExit", failErr)
	}
	if !failRes.Stderr.Complete() {
		t.Error("a process that chose to exit nonzero had its output marked incomplete")
	}
}

// TestRunSensitive_EnvironmentPolicyIsAppliedToTheRealChild proves the policy
// end to end: a replacement wins over every stale inherited occurrence, an
// unset really removes the key, and an unrelated inherited entry survives
// byte-exact.
func TestRunSensitive_EnvironmentPolicyIsAppliedToTheRealChild(t *testing.T) {
	runner, self := helperRunner(t,
		"env:CMUX_SOCKET_PATH,CMUX_QUIET,HERDR_CONFIG_PATH,TMUX,FORGECTL_UNRELATED",
		defaultRetireBound,
		"CMUX_SOCKET_PATH=/stale/first",
		"CMUX_SOCKET_PATH=/stale/second",
		"TMUX=/tmp/tmux-501/default,900,0",
		"FORGECTL_UNRELATED=keep me exactly = as is",
	)

	res, err := runner.RunSensitive(context.Background(), helperCommand(KindCmuxCreate, self, 8192,
		ReplaceCmuxSocketPath("/run/cmux/resolved.sock"),
		SetCmuxQuiet(),
		ReplaceHerdrConfigPath("/etc/herdr/pinned.toml"),
		UnsetTmux(),
	))
	if err != nil {
		t.Fatalf("RunSensitive: %v", err)
	}
	data, complete := res.Stdout.CopyBytesForParse()
	if !complete {
		t.Fatal("environment probe output was truncated")
	}

	want := []string{
		"CMUX_SOCKET_PATH=/run/cmux/resolved.sock",
		"CMUX_QUIET=1",
		"HERDR_CONFIG_PATH=/etc/herdr/pinned.toml",
		"TMUX=<unset>",
		"FORGECTL_UNRELATED=keep me exactly = as is",
	}
	got := string(data)
	for _, line := range want {
		if !strings.Contains(got, line) {
			t.Errorf("child environment missing %q; saw:\n%s", line, got)
		}
	}
	if strings.Contains(got, "/stale/") {
		t.Errorf("a stale inherited occurrence survived the replacement:\n%s", got)
	}
}

// errAfterReader delivers some bytes and then fails, standing in for a pipe
// whose peer died abnormally (EIO) — the one way a stream can stop short
// without either hitting its cap or being force-closed by this package.
type errAfterReader struct {
	data []byte
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// TestReadCapped_MarksANonEOFStopAsIncomplete covers the one path where a
// cut-off stream would otherwise be emitted complete without retire ever
// seeing it: readCapped is the only place that can tell io.EOF from a real
// read error, and downstream receives a delivered BoundedOutput either way.
func TestReadCapped_MarksANonEOFStopAsIncomplete(t *testing.T) {
	cases := map[string]struct {
		reader   io.Reader
		complete bool
	}{
		"clean EOF":       {&errAfterReader{data: []byte("whole"), err: io.EOF}, true},
		"read error":      {&errAfterReader{data: []byte("part"), err: errors.New("input/output error")}, false},
		"closed mid-read": {&errAfterReader{data: []byte("part"), err: os.ErrClosed}, false},
		"no progress":     {&errAfterReader{data: nil, err: nil}, false},
	}

	for name, tc := range cases {
		out := make(chan BoundedOutput, 1)
		overflow := make(chan struct{}, 2)
		readCapped(tc.reader, 1024, out, overflow)
		got := <-out

		if got.Complete() != tc.complete {
			t.Errorf("%s: Complete() = %v, want %v", name, got.Complete(), tc.complete)
		}
		if _, complete := got.CopyBytesForParse(); complete != tc.complete {
			t.Errorf("%s: CopyBytesForParse complete = %v, want %v", name, complete, tc.complete)
		}
		if got.overflow {
			t.Errorf("%s: claimed its cap was hit; none of these reach it", name)
		}
	}
}

// TestBuildEnv_RemovesEveryOccurrenceAndAppendsOnce pins the pure mutation
// logic, so a failure in the end-to-end test above is attributable to either
// the policy or the process plumbing rather than to both at once. It pins
// both halves buildEnv hands sealed.Start in their exact order: the
// surviving inherited entries byte-exact, and each replacement in mutation
// order, its value compared sealed. sealed's own command test pins that the
// replacements land after the inherited entries.
//
// Mutations that turn it red: drop only the first occurrence of a mutated
// key; collect replacements in reverse.
func TestBuildEnv_RemovesEveryOccurrenceAndAppendsOnce(t *testing.T) {
	runner := &OSSensitiveRunner{env: []string{
		"PATH=/usr/bin",
		"CMUX_SOCKET_PATH=/one",
		"CMUX_AUTH_TOKEN=untouched",
		"CMUX_SOCKET_PATH=/two",
		"TMUX=/tmp/a,1,0",
		"BAREKEY",
	}}

	var muts []validated.Env
	for _, m := range []EnvMutation{ReplaceCmuxSocketPath("/resolved"), UnsetTmux(), SetCmuxQuiet()} {
		muts = append(muts, m.toValidated())
	}
	env, set := runner.buildEnv(muts)
	if want := []string{"PATH=/usr/bin", "CMUX_AUTH_TOKEN=untouched", "BAREKEY"}; !slices.Equal(env, want) {
		t.Errorf("inherited env = %q, want %q", env, want)
	}
	wantSet := []sealed.EnvVar{
		{Key: "CMUX_SOCKET_PATH", Value: sealed.New("/resolved")},
		{Key: "CMUX_QUIET", Value: sealed.New("1")},
	}
	if len(set) != len(wantSet) {
		t.Fatalf("buildEnv returned %d replacements, want %d", len(set), len(wantSet))
	}
	for i := range wantSet {
		if set[i].Key != wantSet[i].Key || !set[i].Value.Equal(wantSet[i].Value) {
			t.Errorf("replacement %d has key %q, want %q (or its sealed value differs)", i, set[i].Key, wantSet[i].Key)
		}
	}

	// The captured environment must not be mutated in place — a second call
	// with no mutations still sees the original entries.
	if plain, none := runner.buildEnv(nil); len(plain) != 6 || none != nil {
		t.Errorf("captured environment was mutated: %q", plain)
	}
	// An empty captured environment still yields a non-nil env, or the child
	// would inherit the live process environment.
	if empty, _ := (&OSSensitiveRunner{}).buildEnv(nil); empty == nil {
		t.Error("an empty captured environment built a nil env; the child would inherit the live environment")
	}
}

// TestRunSensitive_KillDrainsPrefixStillInThePipe pins forgectl#794: a kill
// must not drop bytes the child had already written but the parent's reader
// had not yet taken. The child writes its prefix and a marker file, then
// stalls; the test cancels once the marker exists, and the tap holds the
// reader back until well after the kill and reap, so the prefix is still in
// the pipe when retirement begins. The drain window is widened to a minute,
// past sensitiveHangBound, so the reader's delay sits far inside it.
//
// The drain must end at the reader's io.EOF, not at its bound. With the
// bound a minute long, a run that waited it out does not return within
// sensitiveHangBound, so that needs no tight wall-clock bound (forgectl#919);
// nor may retire log that the window expired.
//
// Mutations that turn it red: make retire force-close at once when stopped
// (closeAll before collecting; stdout comes back empty), or set drainBound's
// result to a nanosecond; make retire sleep out the bound when stopped, or
// stop collecting stdout when stopped until the bound (neither returns
// within sensitiveHangBound).
func TestRunSensitive_KillDrainsPrefixStillInThePipe(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "written")
	runner, self := helperRunner(t, "partialmark:"+marker, 2*time.Minute)
	runner.killDrainBound = time.Minute
	logs := captureLogs(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	killed := make(chan struct{})
	runner.stdoutTap = func(r io.Reader) io.Reader {
		return &gatedReader{r: r, gate: killed}
	}
	go func() {
		for {
			if _, err := os.Stat(marker); err == nil {
				cancel()
				// Past the kill and reap, which is when the old code closed
				// the read end.
				time.Sleep(200 * time.Millisecond)
				close(killed)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	res, _, err := runSensitiveWithin(ctx, t, runner, helperCommand(KindTmuxCreate, self, 4096))
	if err == nil {
		t.Fatal("expected the cancellation to be reported")
	}
	data, complete := res.Stdout.CopyBytesForParse()
	if string(data) != "PARTIAL" {
		t.Errorf("stdout = %q, want the prefix the child wrote before the kill", data)
	}
	if complete {
		t.Error("a killed producer's prefix reported itself complete")
	}
	if strings.Contains(logs.String(), "drain window expired") {
		t.Errorf("the post-kill drain ran out its bound; it should end at the reader's EOF:\n%s", logs)
	}
}

// gatedReader blocks its first Read until gate closes, then passes reads
// through. Only the one reader goroutine calls Read.
type gatedReader struct {
	r      io.Reader
	gate   <-chan struct{}
	opened bool
}

func (g *gatedReader) Read(p []byte) (int, error) {
	if !g.opened {
		<-g.gate
		g.opened = true
	}
	return g.r.Read(p)
}
