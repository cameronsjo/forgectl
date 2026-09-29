package exec

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// streamCancelGrace is how long RunStreaming keeps delivering output after
// its context is done before it closes its end of the child's pipes.
//
// The bound exists for one case: the child is gone (the context's cancel
// killed it) but a descendant it forked still holds the stdout or stderr pipe
// open, so the copy would otherwise wait for that descendant to exit. The
// grace only has to cover draining what is already in flight: a full pipe
// buffer (64 KiB on Linux, as little as 16 KiB on macOS) reaches a responsive
// writer in well under a millisecond, and a descendant that dies with its
// parent (SIGHUP, SIGPIPE) closes its end at the same moment. Two seconds
// leaves three orders of magnitude of headroom over that, and it is still
// short enough that a user who pressed Ctrl-C sees the command end before
// reaching for a second Ctrl-C or kill. A normal exit never starts this
// timer: without a cancel the copies run until the pipes close, however slow
// the consumer.
const streamCancelGrace = 2 * time.Second

// RunStreaming connects a child to caller-supplied streams without buffering
// stdout or stderr. Unlike the ordinary Runner methods it does not log or
// return argv: kubectl's global flags can carry credentials, and a streaming
// helper must not turn those into a second persistence surface. The child exit
// code is retained on CommandError so an explicitly opted-in CLI can propagate
// it.
//
// RunStreaming owns the pipes and the copies between them and the caller's
// streams, rather than letting os/exec do it (#602). os/exec's only bound on
// its copies is WaitDelay, and that timer also starts when the child exits
// normally, so it truncates a slow consumer (kubectl logs into a paused
// pager) on an ordinary run. Here the wait is unbounded unless the context is
// done; then streamCancelGrace applies, the pipes are closed, and the call
// returns the context's error. An *os.File stream is handed to the child
// directly, as os/exec does, so no copy (and no bound) is involved for it.
//
// After a cancel the call waits for the output copies to leave the caller's
// writers, because returning while one is mid-Write would hand the caller a
// writer still in use. It does not wait for a stdin copy parked in the
// caller's Read, which nothing here can interrupt; that goroutine ends when
// the Read returns, without reading again.
func (OSRunner) RunStreaming(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	slog.Debug("Preparing to run streaming command.", "cmd", name)
	// name and args stay distinct all the way into os/exec; no shell parses
	// them. StreamingRunner is the same process boundary as Runner.Run above.
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // structural argv is the purpose of this execution seam
	var p streamPipes
	if err := p.wire(cmd, stdin, stdout, stderr); err != nil {
		p.closeAll()
		return &CommandError{Name: name, ExitCode: -1, Err: err}
	}
	if err := cmd.Start(); err != nil {
		p.closeAll()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return &CommandError{Name: name, ExitCode: -1, Err: ctxErr}
		}
		return &CommandError{Name: name, ExitCode: -1, Err: err}
	}
	// The child holds its own copies now. Keeping ours open would stop the
	// copies from ever seeing EOF.
	p.closeChildEnds()
	p.startCopies()

	waitErr := cmd.Wait()
	cut, copyErr := p.drain(ctx, streamCancelGrace)
	if cut || (waitErr != nil && ctx.Err() != nil) {
		return &CommandError{Name: name, ExitCode: -1, Err: ctx.Err()}
	}
	err := waitErr
	if err == nil {
		// As os/exec does: a copy failure is reported only when the child
		// itself succeeded, since a failed child's exit is the better reason.
		err = copyErr
	}
	if err == nil {
		slog.Debug("Streaming command exited.", "cmd", name)
		return nil
	}
	return &CommandError{Name: name, ExitCode: exitCodeOf(err), Err: err}
}

// streamPipes holds the pipes RunStreaming creates for non-file streams.
type streamPipes struct {
	childEnds []*os.File    // handed to the child, closed in the parent after Start
	readEnds  []*onceCloser // the parent's ends of the stdout/stderr pipes
	stdinW    *onceCloser   // the parent's end of the stdin pipe, if any

	outputs []copyJob
	stdin   io.Reader

	results chan copyResult
	nOut    int
	nIn     int
}

type copyJob struct {
	dst io.Writer
	src *onceCloser
}

// onceCloser closes its file once. Both a copy goroutine and drain's cut path
// may close the same pipe end, and a second Close of an *os.File is not
// something to lean on from two goroutines at once.
type onceCloser struct {
	*os.File
	once sync.Once
}

func (c *onceCloser) close() { c.once.Do(func() { _ = c.Close() }) }

type copyResult struct {
	stdin bool
	err   error
}

func (p *streamPipes) wire(cmd *exec.Cmd, stdin io.Reader, stdout, stderr io.Writer) error {
	var err error
	if cmd.Stdout, err = p.output(stdout); err != nil {
		return err
	}
	if sameWriter(stdout, stderr) {
		// One pipe for both, as os/exec does, so a writer that is not safe
		// for concurrent use is only ever written by one copy.
		cmd.Stderr = cmd.Stdout
	} else if cmd.Stderr, err = p.output(stderr); err != nil {
		return err
	}
	switch in := stdin.(type) {
	case nil:
	case *os.File:
		cmd.Stdin = in
	default:
		pr, pw, err := os.Pipe()
		if err != nil {
			return err
		}
		p.childEnds = append(p.childEnds, pr)
		p.stdinW = &onceCloser{File: pw}
		p.stdin = in
		p.nIn = 1
		cmd.Stdin = pr
	}
	return nil
}

// output returns what the child should write to for w: nothing (os/exec's
// null device), the file itself, or the write end of a new pipe that a copy
// will drain into w.
func (p *streamPipes) output(w io.Writer) (io.Writer, error) {
	switch f := w.(type) {
	case nil:
		return nil, nil
	case *os.File:
		return f, nil
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	rc := &onceCloser{File: pr}
	p.childEnds = append(p.childEnds, pw)
	p.readEnds = append(p.readEnds, rc)
	p.outputs = append(p.outputs, copyJob{dst: w, src: rc})
	p.nOut++
	return pw, nil
}

func (p *streamPipes) startCopies() {
	p.results = make(chan copyResult, p.nOut+p.nIn)
	for _, job := range p.outputs {
		go func() {
			_, err := io.Copy(job.dst, job.src.File)
			// On a write error the child must not block on a pipe nobody
			// reads any more; closing our end hands it EPIPE instead.
			job.src.close()
			p.results <- copyResult{err: err}
		}()
	}
	if p.nIn > 0 {
		go func() {
			_, err := io.Copy(p.stdinW.File, p.stdin)
			p.stdinW.close()
			p.results <- copyResult{stdin: true, err: err}
		}()
	}
}

// drain waits for the copies. It has no deadline until ctx is done; from then
// it allows grace, and past that it closes the parent's pipe ends, waits for
// the output copies to return, and reports cut. When the copies finish on
// their own it returns the first copy error.
func (p *streamPipes) drain(ctx context.Context, grace time.Duration) (cut bool, err error) {
	pendingOut, pendingIn := p.nOut, p.nIn
	collect := func(r copyResult) {
		if r.stdin {
			pendingIn--
			if ignorableStdinErr(r.err) {
				return
			}
		} else {
			pendingOut--
		}
		if err == nil {
			err = r.err
		}
	}
	// waitUntil collects results until every copy is done (true) or stop
	// fires first (false).
	waitUntil := func(stop <-chan struct{}) bool {
		for pendingOut+pendingIn > 0 {
			select {
			case r := <-p.results:
				collect(r)
			case <-stop:
				return false
			}
		}
		return true
	}

	if waitUntil(ctx.Done()) {
		return false, err
	}
	graceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()
	if waitUntil(graceCtx.Done()) {
		return false, err
	}

	// Past the grace: whatever still holds the pipes is not going to finish
	// on its own. Closing our read ends unblocks a copy parked in Read, and
	// closing the stdin write end unblocks one parked in Write.
	for _, f := range p.readEnds {
		f.close()
	}
	if p.stdinW != nil {
		p.stdinW.close()
	}
	for pendingOut > 0 {
		collect(<-p.results)
	}
	return true, nil
}

func (p *streamPipes) closeChildEnds() {
	for _, f := range p.childEnds {
		_ = f.Close()
	}
}

func (p *streamPipes) closeAll() {
	p.closeChildEnds()
	for _, f := range p.readEnds {
		f.close()
	}
	if p.stdinW != nil {
		p.stdinW.close()
	}
}

// ignorableStdinErr mirrors os/exec: a child that exits without reading all
// of its stdin is not a failure.
func ignorableStdinErr(err error) bool {
	return err == nil || errors.Is(err, syscall.EPIPE) || errors.Is(err, os.ErrClosed)
}

// sameWriter reports whether a and b are the same writer. Comparing
// interfaces panics when the dynamic type is not comparable, and such writers
// are never treated as shared.
func sameWriter(a, b io.Writer) (same bool) {
	if a == nil || b == nil {
		return false
	}
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}
