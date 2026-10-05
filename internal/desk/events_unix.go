//go:build unix

package desk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EventLog appends event lines to done/<name>.events. Safe for concurrent use.
type EventLog struct {
	mu sync.Mutex
	f  *os.File
}

// openEvents creates done/<name>.events. It must not exist yet: an events
// file planted ahead of the run, already holding RUN-END, would make a
// watcher report the run as ended before it starts.
func (d *Desk) openEvents(name string) (*EventLog, error) {
	f, err := d.root.OpenFile(path.Join(DirDone, name+extEvents), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, fileMode)
	if err != nil {
		return nil, fmt.Errorf("desk: open events for %s: %w", describe(name), err)
	}
	return &EventLog{f: f}, nil
}

// Emit appends one line. A newline inside line is replaced, so one call is
// always one event.
func (e *EventLog) Emit(line string) error {
	line = strings.NewReplacer("\n", " ", "\r", " ").Replace(line)
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err := e.f.WriteString(line + "\n")
	return err
}

// Close closes the log.
func (e *EventLog) Close() error { return e.f.Close() }

// hasRunEnd reports whether done/<name>.events ends with a RUN-END line, and
// whether the file exists at all. RUN-END is written last, so only the last
// 4 KiB are read.
func (d *Desk) hasRunEnd(name string) (ended, exists bool) {
	buf, whole, err := d.readTail(DirDone, name+extEvents)
	if errors.Is(err, fs.ErrNotExist) {
		return false, false
	}
	if err != nil {
		return false, true
	}
	if !whole { // the tail starts mid-line; that fragment is not a line start
		_, buf, _ = bytes.Cut(buf, []byte("\n"))
	}
	for _, line := range strings.Split(string(buf), "\n") {
		if strings.HasPrefix(line, EventRunEnd+" ") {
			return true, true
		}
	}
	return false, true
}

// WatchState is how a watched run stands after a poll.
type WatchState int

const (
	// WatchWaiting: the item is pending, or claimed by a live claimer with no
	// owner recorded yet.
	WatchWaiting WatchState = iota
	// WatchRunning: the owner process is alive.
	WatchRunning
	// WatchEnded: RUN-END was read (or a legacy done item, which has no events).
	WatchEnded
	// WatchLost: the owner (or, before a run began, the claimer) is gone and
	// there is no RUN-END; Poll emitted RUN-LOST.
	WatchLost
	// WatchSkipped: the item was skipped (operator, or changed) and will not run.
	WatchSkipped
)

func (s WatchState) String() string {
	switch s {
	case WatchWaiting:
		return "waiting"
	case WatchRunning:
		return "running"
	case WatchEnded:
		return "ended"
	case WatchLost:
		return "lost"
	case WatchSkipped:
		return "skipped"
	}
	return "unknown(" + strconv.Itoa(int(s)) + ")"
}

// Watcher limits. Only the first maxEventBytes of an events file are read;
// past that the watcher reports Capped. A poll reads at most maxPollBytes and
// returns at most maxPollLines lines; the rest come on later polls. An event
// line longer than maxEventLine is dropped and counted, never held.
const (
	maxEventBytes = 32 << 20
	maxPollBytes  = 4 << 20
	maxPollLines  = 50000
	maxEventLine  = 64 << 10
)

// Watcher follows one item's event lines.
type Watcher struct {
	d    *Desk
	name string
	skip int
	seen int
	// dropped counts lines longer than maxEventLine, which were not returned.
	dropped int
	// off is how far the file has been read; dev and ino identify it, so a
	// replaced or truncated file starts the read over.
	off      int64
	dev, ino uint64
	opened   bool
	partial  []byte
	// dropping: the current line already passed maxEventLine; discard up to
	// its newline.
	dropping bool
	// more: the last read stopped at a poll limit with lines still unread
	// inside the cap. capped: the file is past maxEventBytes.
	more, capped bool
}

// NewWatcher follows name's events, not returning the first skip lines (the
// resume point a previous watcher reported through Seen).
func (d *Desk) NewWatcher(name string, skip int) (*Watcher, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("desk: %q is not an item name (NN-name)", describe(name))
	}
	return &Watcher{d: d, name: name, skip: max(skip, 0)}, nil
}

// Seen is how many complete event lines have been read from the current
// events file, including skipped and dropped ones; pass it as skip to
// resume. When the file is replaced (a new device or inode) or shrinks, the
// watcher starts over on the new file: Seen restarts at 0, and the original
// skip no longer applies, since it counted lines of the old file.
func (w *Watcher) Seen() int { return w.seen }

// Dropped is how many event lines were longer than 64 KiB and so were
// counted in Seen but not returned.
func (w *Watcher) Dropped() int { return w.dropped }

// Capped reports an events file past 32 MiB. Only its first 32 MiB are read;
// the lines after that are never returned.
func (w *Watcher) Capped() bool { return w.capped }

// Poll returns the event lines that arrived since the last poll and how the
// run stands. One poll returns at most 50,000 lines from at most 4 MiB of the
// file; the next poll continues from there. When the run is lost, the poll
// that returns the last line inside the 32 MiB cap ends with
// "RUN-LOST id=<name> pid=<pid>"; until then a lost run's polls return its
// lines without it. A finished run reads as ended on every poll, including
// one from a watcher resumed past its RUN-END.
func (w *Watcher) Poll() ([]string, WatchState, error) {
	lines, ended, err := w.read(maxPollLines)
	if err != nil || ended {
		return lines, WatchEnded, err
	}
	state, pid, err := w.state()
	if err != nil {
		return lines, state, err
	}
	if (state == WatchEnded || state == WatchLost) && !w.more {
		// The owner may have written its last lines, RUN-END included, and
		// exited between the read and the liveness check: read once more
		// before judging. Nothing is writing the file now.
		next, ended, err := w.read(maxPollLines - len(lines))
		lines = append(lines, next...)
		if err != nil || ended {
			return lines, WatchEnded, err
		}
	}
	if state == WatchLost && !w.more {
		lines = append(lines, fmt.Sprintf("%s id=%s pid=%d", EventRunLost, w.name, pid))
	}
	return lines, state, nil
}

// read returns at most budget of the complete lines added since the last
// read, and sets w.more when it stopped at budget or maxPollBytes with lines
// unread inside the cap. ended is set when it returned RUN-END.
func (w *Watcher) read(budget int) (lines []string, ended bool, err error) {
	w.more = false
	f, st, err := w.d.openRegular(DirDone, w.name+extEvents)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("desk: open events for %s: %w", describe(w.name), err)
	}
	defer f.Close() //nolint:errcheck // read-only

	dev, ino := uint64(st.Dev), uint64(st.Ino) //nolint:gosec,unconvert // G115: Dev is int32 on darwin; only compared for equality
	if w.opened && (dev != w.dev || ino != w.ino || st.Size < w.off) {
		w.off, w.partial, w.dropping, w.seen, w.skip, w.capped = 0, nil, false, 0, 0, false
	}
	w.dev, w.ino, w.opened = dev, ino, true
	w.capped = w.capped || st.Size > maxEventBytes
	limit := min(st.Size, maxEventBytes)
	n := min(limit-w.off, maxPollBytes)
	if n <= 0 {
		return nil, false, nil
	}
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, w.off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	base := w.off
	w.off += int64(got)
	w.more = w.off < limit
	for data := buf[:got]; len(data) > 0; {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			w.hold(data)
			break
		}
		if len(lines) >= budget {
			// Give back this line and everything after it: the next read
			// starts here. A held partial line is only ever the start of the
			// buffer's first line, so it still lines up.
			w.off, w.more = base+int64(got-len(data)), true
			break
		}
		line := data[:i]
		data = data[i+1:]
		if w.dropping || len(w.partial)+len(line) > maxEventLine {
			w.partial, w.dropping = nil, false
			w.seen++
			w.dropped++
			continue
		}
		text := string(w.partial) + string(line)
		w.partial = nil
		w.seen++
		if w.seen <= w.skip {
			continue
		}
		lines = append(lines, text)
		if strings.HasPrefix(text, EventRunEnd+" ") {
			w.more = false
			return lines, true, nil
		}
	}
	return lines, false, nil
}

// hold keeps the start of a line whose newline has not arrived, up to
// maxEventLine; past that the line is dropped once its newline comes.
func (w *Watcher) hold(data []byte) {
	if w.dropping {
		return
	}
	if len(w.partial)+len(data) > maxEventLine {
		w.partial, w.dropping = nil, true
		return
	}
	w.partial = append(w.partial, data...)
}

// stateTries is how many times state looks for an item before it reports
// ErrNotFound.
const stateTries = 3

// state locates the item and judges its owner. It is reached when this
// watcher has not read RUN-END, which may still be in the file: past the
// cap, or before the skip a resumed watcher was given.
//
// The directories are read one after another, and an item can move between
// two of them (running/ to skipped/, skipped/ back to pending/) after the
// first is read and before the second is. Every move is one rename, so an
// item is always somewhere: one that is found nowhere was moving, and is
// looked for again.
func (w *Watcher) state() (WatchState, int, error) {
	for try := 1; ; try++ {
		st, pid, err := w.stateOnce()
		if !errors.Is(err, ErrNotFound) || try == stateTries {
			return st, pid, err
		}
	}
}

func (w *Watcher) stateOnce() (WatchState, int, error) {
	d := w.d
	if _, err := d.findKind(DirPending, w.name); err == nil {
		return WatchWaiting, 0, nil
	}
	// running/ before skipped/: the moves out of running/ (to skipped/ or
	// done/) then always find the item in the later directory.
	if kind, err := d.findKind(DirRunning, w.name); err == nil {
		meta, _, err := d.readMeta(DirRunning, w.name)
		if err != nil {
			return WatchWaiting, 0, err
		}
		switch {
		case meta.PID == 0 && d.claimLost(w.name, kind, meta):
			return WatchLost, meta.ClaimPID, nil // the claimer died before a run began
		case meta.PID == 0:
			return WatchWaiting, 0, nil
		case processAlive(meta.PID, meta.PIDStart):
			return WatchRunning, meta.PID, nil
		}
		return WatchLost, meta.PID, nil
	}
	if _, err := d.findKind(DirSkipped, w.name); err == nil {
		return WatchSkipped, 0, nil
	}
	if d.exists(path.Join(DirDone, w.name+extLog)) {
		// No events file is a legacy item, which never wrote any.
		if ended, exists := d.hasRunEnd(w.name); !exists || ended {
			return WatchEnded, 0, nil
		}
		meta, _, _ := d.readMeta(DirDone, w.name)
		return WatchLost, meta.PID, nil
	}
	return WatchWaiting, 0, ErrNotFound
}

// Watch polls name's events every interval, calling emit for each line, until
// the run ends, is lost or skipped and its lines are all read, or ctx is
// done. It returns the final state and the number of lines seen (the resume
// point).
func (d *Desk) Watch(ctx context.Context, name string, skip int, interval time.Duration, emit func(string)) (WatchState, int, error) {
	w, err := d.NewWatcher(name, skip)
	if err != nil {
		return WatchWaiting, skip, err
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		lines, state, err := w.Poll()
		for _, l := range lines {
			emit(l)
		}
		final := state == WatchEnded || state == WatchLost || state == WatchSkipped
		if err != nil || (final && !w.more) {
			return state, w.Seen(), err
		}
		if w.more {
			if err := ctx.Err(); err != nil {
				return state, w.Seen(), err
			}
			continue // lines are waiting; no reason to sleep
		}
		select {
		case <-ctx.Done():
			return state, w.Seen(), ctx.Err()
		case <-t.C:
		}
	}
}
