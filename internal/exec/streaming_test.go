package exec

// Test plan for streaming.go (#602)
//
// OSRunner.RunStreaming (Classification: process boundary, concurrency)
//   [x] Unhappy: a cancel returns within the grace even though a grandchild
//       still holds stdout and stderr open, and output written before the
//       cancel was delivered
//   [x] Happy: a slow consumer with no cancel receives every byte of a child
//       that has already exited, and the call succeeds
//   [x] Boundary: a child that closes its stdout early and exits nonzero
//       returns promptly with its exit code and its stderr
//   [x] Invariant: one writer given as both stdout and stderr is written by a
//       single copy (race-free) and receives both streams
//   [x] Leak: no goroutine or file descriptor outlives the call, on the
//       normal path or the cancel path

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// runStreamingBounded runs RunStreaming with no stdin and fails the test
// instead of hanging when it does not return within limit.
func runStreamingBounded(ctx context.Context, t *testing.T, limit time.Duration, stdout, stderr io.Writer, name string, args ...string) (time.Duration, error) {
	t.Helper()
	type result struct {
		elapsed time.Duration
		err     error
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		err := OSRunner{}.RunStreaming(ctx, nil, stdout, stderr, name, args...)
		done <- result{time.Since(start), err}
	}()
	select {
	case r := <-done:
		return r.elapsed, r.err
	case <-time.After(limit):
		t.Fatalf("RunStreaming did not return within %v", limit)
		return 0, nil
	}
}

// grandchildScript starts a sleeping grandchild that inherits stdout and
// stderr, records its pid so the test can kill it afterwards, prints a line,
// and then blocks. Canceling kills sh; the grandchild keeps both pipes open.
func grandchildScript(t *testing.T) string {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	t.Cleanup(func() { killPidFile(pidFile) })
	return fmt.Sprintf(`sleep 30 & echo $! > %q; echo diag >&2; echo before-cancel; wait`, pidFile)
}

// Mutation: in drain, replace the first waitUntil(ctx.Done()) with an
// unbounded wait (waitUntil(nil)) and the call blocks until the grandchild's
// 30s sleep ends, tripping the 10s limit. Mutation: skip the read-end closes
// on the cut path and the output copies never return.
func TestOSRunner_RunStreaming_CancelReturnsDespiteGrandchildHoldingPipes(t *testing.T) {
	script := grandchildScript(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stdout := &notifyWriter{want: "before-cancel\n", seen: make(chan struct{})}
	var stderr syncBuffer
	go func() {
		<-stdout.seen
		cancel()
	}()

	elapsed, err := runStreamingBounded(ctx, t, 10*time.Second, stdout, &stderr, "sh", "-c", script)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) || cmdErr.ExitCode != -1 {
		t.Fatalf("error = %#v, want *CommandError with ExitCode -1", err)
	}
	if elapsed > streamCancelGrace+2*time.Second {
		t.Fatalf("returned after %v, want within the %v grace plus slack", elapsed, streamCancelGrace)
	}
	if elapsed < streamCancelGrace {
		t.Fatalf("returned after %v, before the %v grace; the grandchild was holding the pipes, so a drain cannot have finished on its own", elapsed, streamCancelGrace)
	}
	if got := stdout.String(); got != "before-cancel\n" {
		t.Errorf("stdout = %q, want the line written before the cancel", got)
	}
	if got := stderr.String(); got != "diag\n" {
		t.Errorf("stderr = %q, want %q", got, "diag\n")
	}
}

// Here the child itself exits 0 at once; only the grandchild keeps the pipes
// open. The call waits (a normal exit never starts the grace), and the
// cancel that finally ends it must be reported: the stream was cut, so a
// nil error would claim a complete output that nobody can vouch for.
//
// Mutation: drop `cut ||` from RunStreaming's cancel check and the call
// returns nil, because the child's own exit was clean.
func TestOSRunner_RunStreaming_CutAfterCleanChildExitIsReported(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	t.Cleanup(func() { killPidFile(pidFile) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stdout := &notifyWriter{want: "done\n", seen: make(chan struct{})}
	go func() {
		<-stdout.seen
		// Let the call sit in its unbounded wait for a while first, so the
		// test also shows that a normal exit never started the grace.
		time.Sleep(streamCancelGrace + 500*time.Millisecond)
		cancel()
	}()
	elapsed, err := runStreamingBounded(ctx, t, 15*time.Second, stdout, nil, "sh", "-c",
		fmt.Sprintf(`sleep 30 & echo $! > %q; echo done; exit 0`, pidFile))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if elapsed < 2*streamCancelGrace {
		t.Fatalf("returned after %v; it should have waited for the cancel and then the grace", elapsed)
	}
}

// The first Write stalls longer than the cancel grace, and the child has
// long exited by the time the consumer wakes. With no cancel, nothing may be
// lost.
//
// Mutation: start the grace on a normal exit too (in drain, replace
// waitUntil(ctx.Done()) with a closed channel so the grace timer runs
// straight away) and the copy is cut, losing the tail: the byte count falls
// short and the call reports a cancel that never happened.
func TestOSRunner_RunStreaming_SlowConsumerWithoutCancelGetsEveryByte(t *testing.T) {
	const size = 60000
	w := &stallingWriter{stall: streamCancelGrace + time.Second}
	_, err := runStreamingBounded(t.Context(), t, 20*time.Second, w, nil, "sh", "-c", fmt.Sprintf(`head -c %d /dev/zero | tr '\0' q`, size))
	if err != nil {
		t.Fatalf("RunStreaming: %v", err)
	}
	if got := w.buf.Len(); got != size {
		t.Fatalf("delivered %d bytes, want %d", got, size)
	}
}

// Mutation: drop p.closeChildEnds() after Start and the parent's own copy of
// the stdout write end keeps the pipe open forever, so the call hangs past
// the 10s limit.
func TestOSRunner_RunStreaming_ClosedStdoutEarlyExitReturnsPromptly(t *testing.T) {
	var stdout, stderr syncBuffer
	elapsed, err := runStreamingBounded(t.Context(), t, 10*time.Second, &stdout, &stderr, "sh", "-c", `echo partial; exec >&-; echo why >&2; exit 3`)
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) || cmdErr.ExitCode != 3 {
		t.Fatalf("error = %v, want *CommandError with ExitCode 3", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("returned after %v; an early-closed stdout must not wait on anything", elapsed)
	}
	if stdout.String() != "partial\n" || stderr.String() != "why\n" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// bytes.Buffer is not safe for concurrent use, so -race flags two copies
// writing into it at once.
//
// Mutation: make sameWriter return false and the race detector reports two
// goroutines writing the shared buffer.
func TestOSRunner_RunStreaming_SharedWriterGetsOneCopy(t *testing.T) {
	var both bytes.Buffer
	_, err := runStreamingBounded(t.Context(), t, 10*time.Second, &both, &both, "sh", "-c",
		`i=0; while [ $i -lt 200 ]; do echo out; echo err >&2; i=$((i+1)); done`)
	if err != nil {
		t.Fatalf("RunStreaming: %v", err)
	}
	if got := strings.Count(both.String(), "out\n") + strings.Count(both.String(), "err\n"); got != 400 {
		t.Fatalf("got %d lines, want 400", got)
	}
}

// Mutation: in the output copy goroutine, drop job.src.close() and the
// normal path leaks one read end per stream (the fd count rises). Mutation:
// on the cut path, skip the read-end closes and the call hangs instead.
func TestOSRunner_RunStreaming_LeavesNoGoroutinesOrDescriptors(t *testing.T) {
	if _, err := os.ReadDir(fdDir); err != nil {
		t.Skipf("cannot list open descriptors: %v", err)
	}
	// Warm up once so lazily created runtime goroutines and descriptors (the
	// signal handler, the poller) are not counted as leaks.
	_ = OSRunner{}.RunStreaming(t.Context(), strings.NewReader("x"), &syncBuffer{}, &syncBuffer{}, "sh", "-c", "cat; echo e >&2")

	baseG, baseFD := runtime.NumGoroutine(), openFDs(t)

	for range 3 {
		err := OSRunner{}.RunStreaming(t.Context(), strings.NewReader("in"), &syncBuffer{}, &syncBuffer{}, "sh", "-c", "cat; echo e >&2")
		if err != nil {
			t.Fatalf("normal path: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	stdout := &notifyWriter{want: "before-cancel\n", seen: make(chan struct{})}
	go func() {
		<-stdout.seen
		cancel()
	}()
	if _, err := runStreamingBounded(ctx, t, 10*time.Second, stdout, &syncBuffer{}, "sh", "-c", grandchildScript(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel path: %v", err)
	}

	// Goroutines exit just after they send their result; give them a moment.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseG && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if g := runtime.NumGoroutine(); g > baseG {
		buf := make([]byte, 1<<16)
		t.Fatalf("goroutines: %d before, %d after\n%s", baseG, g, buf[:runtime.Stack(buf, true)])
	}
	if fd := openFDs(t); fd > baseFD {
		t.Fatalf("open descriptors: %d before, %d after", baseFD, fd)
	}
}

func killPidFile(path string) {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

var fdDir = func() string {
	if runtime.GOOS == "linux" {
		return "/proc/self/fd"
	}
	return "/dev/fd"
}()

func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		t.Fatalf("list descriptors: %v", err)
	}
	return len(entries)
}

// syncBuffer is a bytes.Buffer safe to read while a copy writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// notifyWriter closes seen once the accumulated output contains want.
type notifyWriter struct {
	syncBuffer
	want string
	seen chan struct{}
	once sync.Once
}

func (w *notifyWriter) Write(p []byte) (int, error) {
	n, err := w.syncBuffer.Write(p)
	if strings.Contains(w.String(), w.want) {
		w.once.Do(func() { close(w.seen) })
	}
	return n, err
}

// stallingWriter blocks its first Write for stall, like a paused pager.
type stallingWriter struct {
	stall time.Duration
	once  sync.Once
	buf   bytes.Buffer
}

func (w *stallingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { time.Sleep(w.stall) })
	return w.buf.Write(p)
}
