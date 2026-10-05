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

// Watcher limits. A poll reads at most maxPollBytes of new events, and an
// event line longer than maxEventLine is dropped and counted, never held.
const (
	maxPollBytes = 32 << 20
	maxEventLine = 64 << 10
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

// Poll returns the event lines that arrived since the last poll and how the
// run stands. When the run is lost, the last line returned is
// "RUN-LOST id=<name> pid=<pid>". One poll reads at most 32 MiB of new
// events; the next poll continues from there.
func (w *Watcher) Poll() ([]string, WatchState, error) {
	lines, ended, _, err := w.read()
	if err != nil || ended {
		return lines, WatchEnded, err
	}
	state, pid, err := w.state()
	if err != nil {
		return lines, state, err
	}
	if state == WatchLost {
		// The owner may have written RUN-END and exited between the read and
		// the liveness check: read to the end once more before calling it
		// lost. Nothing is writing the file now, so this ends.
		for more := true; more; {
			var next []string
			next, ended, more, err = w.read()
			lines = append(lines, next...)
			if err != nil || ended {
				return lines, WatchEnded, err
			}
		}
		lines = append(lines, fmt.Sprintf("%s id=%s pid=%d", EventRunLost, w.name, pid))
	}
	return lines, state, nil
}

// read returns the complete lines added since the last read. more is true
// when it stopped at maxPollBytes with more of the file unread.
func (w *Watcher) read() (lines []string, ended, more bool, err error) {
	f, st, err := w.d.openRegular(DirDone, w.name+extEvents)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, false, nil
	}
	if err != nil {
		return nil, false, false, fmt.Errorf("desk: open events for %s: %w", describe(w.name), err)
	}
	defer f.Close() //nolint:errcheck // read-only

	dev, ino := uint64(st.Dev), uint64(st.Ino) //nolint:gosec,unconvert // G115: Dev is int32 on darwin; only compared for equality
	if w.opened && (dev != w.dev || ino != w.ino || st.Size < w.off) {
		w.off, w.partial, w.dropping, w.seen, w.skip = 0, nil, false, 0, 0
	}
	w.dev, w.ino, w.opened = dev, ino, true
	n := min(st.Size-w.off, maxPollBytes)
	if n <= 0 {
		return nil, false, false, nil
	}
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, w.off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, false, err
	}
	w.off += int64(got)
	more = w.off < st.Size
	for data := buf[:got]; len(data) > 0; {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			w.hold(data)
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
			return lines, true, false, nil
		}
	}
	return lines, false, more, nil
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

// state locates the item and judges its owner. It is only reached when no
// RUN-END has been read.
func (w *Watcher) state() (WatchState, int, error) {
	d := w.d
	if _, err := d.findKind(DirPending, w.name); err == nil {
		return WatchWaiting, 0, nil
	}
	if _, err := d.findKind(DirSkipped, w.name); err == nil {
		return WatchSkipped, 0, nil
	}
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
	if d.exists(path.Join(DirDone, w.name+extLog)) {
		if _, exists := d.hasRunEnd(w.name); !exists {
			return WatchEnded, 0, nil // legacy: no events were ever written
		}
		meta, _, _ := d.readMeta(DirDone, w.name)
		return WatchLost, meta.PID, nil
	}
	return WatchWaiting, 0, ErrNotFound
}

// Watch polls name's events every interval, calling emit for each line, until
// the run ends, is lost or skipped, or ctx is done. It returns the final state
// and the number of lines seen (the resume point).
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
		if err != nil || state == WatchEnded || state == WatchLost || state == WatchSkipped {
			return state, w.Seen(), err
		}
		select {
		case <-ctx.Done():
			return state, w.Seen(), ctx.Err()
		case <-t.C:
		}
	}
}
