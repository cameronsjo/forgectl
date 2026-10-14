// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package runview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Restart pacing for a followed command: the first restart waits
// streamRestartMin, each one after a run that wrote nothing waits twice as
// long, up to streamRestartMax.
// They are variables so a test can run the restart quickly.
var (
	streamRestartMin = time.Second
	streamRestartMax = 30 * time.Second
)

const (
	// streamStopGrace is how long a stopped command has to exit after
	// SIGTERM before it is killed.
	streamStopGrace = 2 * time.Second
	// streamChunk is how much one read of the stream takes.
	streamChunk = 32 << 10
)

// StreamSource reads one run from a byte stream rather than a file: a log
// piped to stdin, or the stdout of a command it runs (kubectl logs -f, ssh
// host tail -F, docker logs -f). The bytes are held in memory, up to the
// same 32 MiB cap a file is read to, and never written to disk, so a lens
// that keeps only event names keeps a sensitive log's text from reaching
// anything else. Its lines read exactly as a file's do, through a lens or as
// JSONL.
//
// A followed command is restarted when it exits, so the run does not end
// because the stream did: a pod replaced mid-run starts a new logs -f. The
// restarted command's first output replaces what the old one wrote, as a
// log file replaced on disk does, because a command like kubectl logs -f
// writes the whole log again from its start.
type StreamSource struct {
	name string
	r    lineReader

	mu      sync.Mutex
	buf     []byte
	gen     uint64 // bumped when a restarted command's output replaces buf
	capped  bool   // the stream passed maxFileBytes; the rest was discarded
	note    error  // what the stream is doing, when it is not just flowing
	updated time.Time

	command bool // the bytes come from a command the source runs
	cancel  context.CancelFunc
	done    chan struct{} // closed once no more bytes will come
}

func newStreamSource(name string, lens *Lens, keys LogKeys) (*StreamSource, error) {
	if lens == nil && keys.Event == "" {
		return nil, errors.New("log: the event key must not be empty")
	}
	return &StreamSource{
		name:    clean(name),
		r:       lineReader{lens: lens, keys: keys},
		updated: time.Now().UTC(),
		cancel:  func() {},
		done:    make(chan struct{}),
	}, nil
}

// NewReaderSource returns a source that reads r, a log's lines, until r
// ends; a nil lens reads them as JSONL with keys. The run is named name.
func NewReaderSource(name string, r io.Reader, lens *Lens, keys LogKeys) (*StreamSource, error) {
	s, err := newStreamSource(name, lens, keys)
	if err != nil {
		return nil, err
	}
	go func() {
		defer close(s.done)
		_, err := s.pump(r, false)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.endLine()
		if err != nil {
			s.note = fmt.Errorf("cannot read %s: %w", s.name, err)
		}
	}()
	return s, nil
}

// NewCommandSource returns a source that runs command with /bin/sh -c and
// reads its stdout as a log; a nil lens reads it as JSONL with keys. The
// command gets no stdin, and its stderr is kept only to say why it exited.
// With follow set it is restarted whenever it exits, until ctx ends or Close;
// without it, the stream ends when the command does. The run is named after
// the command.
func NewCommandSource(ctx context.Context, command string, follow bool, lens *Lens, keys LogKeys) (*StreamSource, error) {
	if strings.TrimSpace(command) == "" {
		return nil, errors.New("follow-cmd: the command must not be empty")
	}
	s, err := newStreamSource(command, lens, keys)
	if err != nil {
		return nil, err
	}
	s.command = true
	ctx, s.cancel = context.WithCancel(ctx)
	go s.follow(ctx, command, follow)
	return s, nil
}

// Close stops a command the source runs and waits for it to exit. A reader
// source is left to end on its own: a pipe's read cannot be interrupted.
func (s *StreamSource) Close() {
	s.cancel()
	if s.command {
		<-s.done
	}
}

// Wait blocks until no more bytes will come: the reader ended, or the
// command exited and is not followed. Show reads a stream this way when it
// prints one view rather than following it.
func (s *StreamSource) Wait(ctx context.Context) error {
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// follow runs command until it exits, then again while restart is set.
func (s *StreamSource) follow(ctx context.Context, command string, restart bool) {
	defer close(s.done)
	wait := streamRestartMin
	for run := 0; ; run++ {
		wrote, err := s.runOnce(ctx, command, run > 0)
		if ctx.Err() != nil {
			return
		}
		if !restart {
			s.mu.Lock()
			s.endLine()
			if err != nil {
				s.note = err
			}
			s.mu.Unlock()
			return
		}
		if wrote {
			wait = streamRestartMin
		}
		s.setNote(fmt.Errorf("%w; restarting in %s", errOrExit(err), wait))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		s.setNote(fmt.Errorf("%w; restarted, waiting for output", errOrExit(err)))
		wait = min(wait*2, streamRestartMax)
	}
}

// errOrExit is err, or "the command exited" for a clean exit.
func errOrExit(err error) error {
	if err == nil {
		return errors.New("the command exited")
	}
	return err
}

func (s *StreamSource) setNote(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.note = err
}

// runOnce runs command once and reads its stdout into the source. On a
// restart, the command's first output replaces what the last run wrote.
// wrote reports any output; err says how the command failed, with the last
// line it wrote to stderr.
func (s *StreamSource) runOnce(ctx context.Context, command string, restart bool) (wrote bool, err error) {
	// The command is the person's own --follow-cmd, run as they wrote it.
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command) //nolint:gosec // G204: running it is the point

	// Its own process group, so stopping it stops a whole pipeline, and the
	// terminal's job-control signals reach forgectl alone.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return unix.Kill(-cmd.Process.Pid, unix.SIGTERM) }
	cmd.WaitDelay = streamStopGrace
	var stderr lastLine
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return false, fmt.Errorf("cannot run the command: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("cannot run the command: %w", err)
	}
	wrote, readErr := s.pump(out, restart)
	err = cmd.Wait()
	if err == nil {
		err = readErr
	}
	if err != nil {
		if line := stderr.String(); line != "" {
			err = fmt.Errorf("%w: %s", err, line)
		}
		err = fmt.Errorf("the command failed: %w", err)
	}
	return wrote, err
}

// pump reads r into the buffer until r ends. With replace set, the first
// bytes read replace the buffer as a new generation. Past maxFileBytes the
// stream is still drained, so a command never blocks on a full pipe, but
// what it writes is discarded.
func (s *StreamSource) pump(r io.Reader, replace bool) (wrote bool, err error) {
	chunk := make([]byte, streamChunk)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			s.mu.Lock()
			if replace && !wrote {
				s.buf, s.capped, s.note = nil, false, nil
				s.gen++
			}
			wrote = true
			room := max(maxFileBytes-len(s.buf), 0)
			s.buf = append(s.buf, chunk[:min(n, room)]...)
			if n > room {
				s.capped = true
			}
			s.updated = time.Now().UTC()
			s.mu.Unlock()
		}
		if errors.Is(err, io.EOF) || errors.Is(err, unix.EIO) || errors.Is(err, io.ErrClosedPipe) {
			return wrote, nil
		}
		if err != nil {
			return wrote, err
		}
	}
}

// endLine ends a stream whose last line has no newline: it never will get
// one, so it is read rather than held. s.mu is held.
func (s *StreamSource) endLine() {
	if len(s.buf) > 0 && s.buf[len(s.buf)-1] != '\n' && !s.capped {
		s.buf = append(s.buf, '\n')
	}
}

// Name is "log": a stream is read as a log is.
func (s *StreamSource) Name() string { return "log" }

// Spec is the fold a run of this source takes: the lens's, or none.
func (s *StreamSource) Spec(RunRef) *Spec {
	if s.r.lens != nil {
		return s.r.lens.Spec()
	}
	return &Spec{}
}

// List returns the one run, named after the stream.
func (s *StreamSource) List() ([]RunRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []RunRef{{Source: s.Name(), Name: s.name, Kind: KindLog, Updated: s.updated}}, nil
}

// Load reads what the stream gained since cur. When a restarted command's
// output replaced the stream, it is read again from its first line and Reset
// is set. Err says what the stream is doing when it is not flowing: a
// command that exited, and when it restarts.
func (s *StreamSource) Load(_ RunRef, cur *Cursor) (Delta, error) {
	if cur == nil {
		cur = &Cursor{}
	}
	d := s.r.begin()
	s.mu.Lock()
	if cur.opened && (cur.Ino != s.gen || cur.Offset > int64(len(s.buf))) {
		*cur = Cursor{}
		d.Reset = true
	}
	cur.Ino, cur.opened = s.gen, true
	data := slices.Clone(s.buf[min(cur.Offset, int64(len(s.buf))):])
	capped, note := s.capped, s.note
	s.mu.Unlock()

	scanLines(cur, data, func(_ int, b []byte) { s.r.line(cur, &d, b) })
	s.r.finish(cur, &d, capped, note)
	return d, nil
}

// lastLine keeps the last non-blank line written to it, for a command's
// stderr: enough to say why it exited without holding all it wrote.
type lastLine struct {
	mu        sync.Mutex
	cur, last []byte
}

func (l *lastLine) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			if len(bytes.TrimSpace(l.cur)) > 0 {
				l.last = append(l.last[:0], l.cur...)
			}
			l.cur = l.cur[:0]
			continue
		}
		if len(l.cur) < maxLineBytes {
			l.cur = append(l.cur, b)
		}
	}
	return len(p), nil
}

// String is the last non-blank line, made inert and capped.
func (l *lastLine) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	line := l.last
	if len(bytes.TrimSpace(l.cur)) > 0 {
		line = l.cur
	}
	return clean(strings.TrimSpace(string(line)))
}
