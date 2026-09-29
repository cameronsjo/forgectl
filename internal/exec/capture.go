package exec

import (
	"errors"
	"os"
)

// maxStderrTail is how much of a child's stderr runAndWrap keeps: the last
// 64 KiB. A failing command puts its reason at the end (git's "fatal: …"
// follows whatever progress it printed), so the tail is the part worth
// keeping, and 64 KiB is far more than any diagnostic needs. Past it the
// runner drains and discards and never kills the child over stderr: a noisy
// stderr is not a reason to fail a command that would otherwise succeed.
const maxStderrTail = 64 << 10

// maxStdoutBytes is the stdout ceiling for runAndWrap. Stdout is the result a
// caller parses, so it is never cut to a prefix: a child that writes past the
// ceiling is killed and the call fails with ErrOutputTooLarge and no output.
// The largest callers today (`gh repo list --limit 1000 --json …`, `tea repo
// ls --limit 1000`, `git for-each-ref`, `kubectl describe`) write well under
// 1 MiB, and a workflow `run` step's stdout is discarded, so the ceiling sits
// orders of magnitude above them. It exists to bound the heap against a
// runaway child, not to shape any real output.
const maxStdoutBytes = 64 << 20

// ErrOutputTooLarge reports that a child's stdout passed maxStdoutBytes. The
// child was killed and none of its stdout is returned, so a parser can never
// mistake a prefix for the whole stream.
var ErrOutputTooLarge = errors.New("command stdout exceeded the capture ceiling")

// tailBuffer is an io.Writer that keeps only the last limit bytes written to
// it and counts the rest. It never returns an error, so the child's stderr is
// drained to EOF however much it writes.
type tailBuffer struct {
	limit   int
	buf     []byte
	dropped int64
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n >= t.limit {
		t.dropped += int64(len(t.buf) + n - t.limit)
		t.buf = append(t.buf[:0], p[n-t.limit:]...)
		return n, nil
	}
	t.buf = append(t.buf, p...)
	// Compact only once the buffer holds twice the limit, so the copy is
	// amortized over at least limit bytes written.
	if len(t.buf) > 2*t.limit {
		excess := len(t.buf) - t.limit
		t.dropped += int64(excess)
		t.buf = t.buf[:copy(t.buf, t.buf[excess:])]
	}
	return n, nil
}

// tail returns the retained bytes and how many bytes were dropped before them.
func (t *tailBuffer) tail() ([]byte, int64) {
	if excess := len(t.buf) - t.limit; excess > 0 {
		return t.buf[excess:], t.dropped + int64(excess)
	}
	return t.buf, t.dropped
}

// maskedTail renders the retained stderr as the failure path may keep it.
// When the cap dropped bytes, the cut can split a masked value, leaving only
// its end at the start of the tail, where text cannot match it. straddleLen
// drops any such fragment from the raw bytes first; only then does the mask
// run, over a tail in which every masked value is either whole or absent.
//
// The order matters. Masking first and then trimming a fixed count (the
// longest value's length) looks equivalent but is not: a replacement can be
// longer than what it replaced ([redacted] is 10 bytes, a short value can be
// 1), so the masked form of a fragment can outgrow any fixed trim. Trimming a
// fixed count of raw bytes first is wrong the other way: it moves the cut and
// can split a second value at the new start. The returned count covers both
// the bytes the cap dropped and the bytes straddleLen removed.
func maskedTail(t *tailBuffer, mask argMask) (string, int64) {
	raw, dropped := t.tail()
	s := string(raw)
	if dropped > 0 {
		n := mask.straddleLen(s)
		s = s[n:]
		dropped += int64(n)
	}
	return mask.text(s), dropped
}

// ceilingWriter captures stdout up to limit bytes. The write that would cross
// the limit kills the child and fails, and so does every later one; os/exec
// then closes its read end of the pipe, so the child cannot block on it
// either.
type ceilingWriter struct {
	limit int
	buf   []byte
	over  bool
	proc  func() *os.Process
}

func (w *ceilingWriter) Write(p []byte) (int, error) {
	if w.over {
		return 0, ErrOutputTooLarge
	}
	if len(w.buf)+len(p) > w.limit {
		w.over = true
		w.buf = nil
		// Writes arrive only after Start has set cmd.Process, and os/exec
		// tolerates a Kill of a process that has already exited.
		if p := w.proc(); p != nil {
			_ = p.Kill()
		}
		return 0, ErrOutputTooLarge
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}
