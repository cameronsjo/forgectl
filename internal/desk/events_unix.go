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

// Event line prefixes, the run-and-watch vocabulary. Only the desk writes
// done/<name>.events; step output never reaches it, so a step cannot print a
// line that ends the run for a watcher.
const (
	EventRunStart  = "RUN-START"
	EventStepStart = "STEP-START"
	EventStepEnd   = "STEP-END"
	EventStepSkip  = "STEP-SKIP"
	EventStepWarn  = "STEP-WARN"
	EventRunEnd    = "RUN-END"
	EventRunLost   = "RUN-LOST"
)

// EventLog appends event lines to done/<name>.events. Safe for concurrent use.
type EventLog struct {
	mu sync.Mutex
	f  *os.File
}

func (d *Desk) openEvents(name string) (*EventLog, error) {
	f, err := d.root.OpenFile(path.Join(DirDone, name+extEvents), os.O_WRONLY|os.O_CREATE|os.O_APPEND, fileMode)
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

// hasRunEnd reports whether done/<name>.events holds a RUN-END line, and
// whether the file exists at all.
func (d *Desk) hasRunEnd(name string) (ended, exists bool) {
	data, err := d.root.ReadFile(path.Join(DirDone, name+extEvents))
	if err != nil {
		return false, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, EventRunEnd+" ") {
			return true, true
		}
	}
	return false, true
}

// WatchState is how a watched run stands after a poll.
type WatchState int

const (
	// WatchWaiting: the item is pending, or claimed with no owner recorded yet.
	WatchWaiting WatchState = iota
	// WatchRunning: the owner process is alive.
	WatchRunning
	// WatchEnded: RUN-END was read (or a legacy done item, which has no events).
	WatchEnded
	// WatchLost: the owner is gone and there is no RUN-END; Poll emitted RUN-LOST.
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

// Watcher follows one item's event lines.
type Watcher struct {
	d       *Desk
	name    string
	skip    int
	seen    int
	off     int64
	partial []byte
}

// NewWatcher follows name's events, not returning the first skip lines (the
// resume point a previous watcher reported through Seen).
func (d *Desk) NewWatcher(name string, skip int) (*Watcher, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("desk: %q is not an item name (NN-name)", describe(name))
	}
	return &Watcher{d: d, name: name, skip: max(skip, 0)}, nil
}

// Seen is how many complete event lines have been read so far, including
// skipped ones; pass it as skip to resume.
func (w *Watcher) Seen() int { return w.seen }

// Poll returns the event lines that arrived since the last poll and how the
// run stands. When the run is lost, the last line returned is
// "RUN-LOST id=<name> pid=<pid>".
func (w *Watcher) Poll() ([]string, WatchState, error) {
	lines, ended, err := w.read()
	if err != nil || ended {
		return lines, WatchEnded, err
	}
	state, pid, err := w.state()
	if err != nil {
		return lines, state, err
	}
	if state == WatchLost {
		// The owner may have written RUN-END and exited between the read and
		// the liveness check: read once more before calling it lost.
		more, ended, err := w.read()
		lines = append(lines, more...)
		if err != nil || ended {
			return lines, WatchEnded, err
		}
		lines = append(lines, fmt.Sprintf("%s id=%s pid=%d", EventRunLost, w.name, pid))
	}
	return lines, state, nil
}

func (w *Watcher) read() (lines []string, ended bool, err error) {
	f, err := w.d.root.Open(path.Join(DirDone, w.name+extEvents))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("desk: open events for %s: %w", describe(w.name), err)
	}
	defer f.Close() //nolint:errcheck // read-only
	if _, err := f.Seek(w.off, io.SeekStart); err != nil {
		return nil, false, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, false, err
	}
	w.off += int64(len(data))
	data = append(w.partial, data...)
	cut := bytes.LastIndexByte(data, '\n')
	if cut < 0 {
		w.partial = data
		return nil, false, nil
	}
	w.partial = append([]byte(nil), data[cut+1:]...)
	for _, line := range strings.Split(string(data[:cut]), "\n") {
		w.seen++
		if w.seen <= w.skip {
			continue
		}
		lines = append(lines, line)
		if strings.HasPrefix(line, EventRunEnd+" ") {
			return lines, true, nil
		}
	}
	return lines, false, nil
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
	if _, err := d.findKind(DirRunning, w.name); err == nil {
		meta, _, err := d.readMeta(DirRunning, w.name)
		if err != nil {
			return WatchWaiting, 0, err
		}
		switch {
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
