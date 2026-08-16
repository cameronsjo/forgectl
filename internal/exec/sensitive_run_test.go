package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
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

func TestRunSensitive_StdoutOverflowKillsAndMarksIncomplete(t *testing.T) {
	const limit = 8192
	runner, self := helperRunner(t, "flood:400000", defaultRetireBound)

	start := time.Now()
	res, err := runner.RunSensitive(context.Background(), helperCommand(KindTmuxSnapshot, self, limit))
	elapsed := time.Since(start)

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
	if elapsed > 3*time.Second {
		t.Errorf("overflow took %v; the runner should kill rather than drain", elapsed)
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

func TestRunSensitive_TimeoutKillsAndClassifies(t *testing.T) {
	runner, self := helperRunner(t, "sleep:60s", defaultRetireBound)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := runner.RunSensitive(ctx, helperCommand(KindCmuxCreate, self, 4096))
	elapsed := time.Since(start)

	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if errors.Is(err, ErrCanceled) {
		t.Error("a deadline was reported as a cancellation; the classes must stay distinct")
	}
	if elapsed > 3*time.Second {
		t.Errorf("timeout returned after %v; the process was not killed and reaped promptly", elapsed)
	}
}

func TestRunSensitive_CancellationKillsAndClassifies(t *testing.T) {
	runner, self := helperRunner(t, "sleep:60s", defaultRetireBound)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := runner.RunSensitive(ctx, helperCommand(KindCmuxCleanup, self, 4096))
	elapsed := time.Since(start)

	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("err = %v, want ErrCanceled", err)
	}
	if errors.Is(err, ErrTimeout) {
		t.Error("a cancellation was reported as a timeout; the classes must stay distinct")
	}
	if elapsed > 3*time.Second {
		t.Errorf("cancellation returned after %v", elapsed)
	}
}

// TestRunSensitive_ReturnsWithinBoundWhenDescendantHoldsThePipe is the
// separately tested wait bound. The immediate CLI exits at once; a descendant
// it spawned keeps both pipe write ends open. Without the runner retiring the
// read ends itself, this call would block for the descendant's whole lifetime.
func TestRunSensitive_ReturnsWithinBoundWhenDescendantHoldsThePipe(t *testing.T) {
	const retire = 300 * time.Millisecond
	runner, self := helperRunner(t, "spawn:30s", retire)

	start := time.Now()
	res, err := runner.RunSensitive(context.Background(), helperCommand(KindTmuxCreate, self, 4096))
	elapsed := time.Since(start)

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
	if elapsed > 3*time.Second {
		t.Fatalf("returned after %v; the descendant's inherited pipe was never retired", elapsed)
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
	cases := map[string]func() (context.Context, context.CancelFunc){
		"deadline": func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 200*time.Millisecond)
		},
		"cancellation": func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				time.Sleep(200 * time.Millisecond)
				cancel()
			}()
			return ctx, func() {}
		},
	}

	for name, mkCtx := range cases {
		runner, self := helperRunner(t, "partial:60s", defaultRetireBound)
		ctx, cancel := mkCtx()

		res, err := runner.RunSensitive(ctx, helperCommand(KindTmuxCreate, self, 4096))
		cancel()

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
// the policy or the process plumbing rather than to both at once.
func TestBuildEnv_RemovesEveryOccurrenceAndAppendsOnce(t *testing.T) {
	runner := &OSSensitiveRunner{env: []string{
		"PATH=/usr/bin",
		"CMUX_SOCKET_PATH=/one",
		"CMUX_AUTH_TOKEN=untouched",
		"CMUX_SOCKET_PATH=/two",
		"TMUX=/tmp/a,1,0",
		"BAREKEY",
	}}

	got := runner.buildEnv([]EnvMutation{ReplaceCmuxSocketPath("/resolved"), UnsetTmux()})

	counts := map[string]int{}
	for _, entry := range got {
		counts[envKeyOf(entry)]++
	}
	if counts["CMUX_SOCKET_PATH"] != 1 {
		t.Errorf("CMUX_SOCKET_PATH appears %d times, want exactly 1: %q", counts["CMUX_SOCKET_PATH"], got)
	}
	if counts["TMUX"] != 0 {
		t.Errorf("TMUX survived the unset: %q", got)
	}
	joined := strings.Join(got, "\n")
	for _, keep := range []string{"PATH=/usr/bin", "CMUX_AUTH_TOKEN=untouched", "BAREKEY", "CMUX_SOCKET_PATH=/resolved"} {
		if !strings.Contains(joined, keep) {
			t.Errorf("missing %q in %q", keep, got)
		}
	}

	// The captured environment must not be mutated in place — a second call
	// with no mutations still sees the original entries.
	if plain := runner.buildEnv(nil); len(plain) != 6 {
		t.Errorf("captured environment was mutated: %q", plain)
	}
}
