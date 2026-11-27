// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package events

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/privdir"
)

func (s *Store) spec() privdir.Spec {
	return privdir.Spec{Base: s.base, Leaf: leafName, Mode: 0o700, AncestorMode: 0o755, Create: true}
}

// open pins the directory: created when write is set, else opened only if
// it exists. absent reports a reader that found no directory, which reads
// as an empty store.
func (s *Store) open(write bool) (fd int, absent bool, err error) {
	if write {
		fd, err = privdir.Pin(s.spec())
		return fd, false, err
	}
	fd, err = privdir.OpenChecked(s.spec())
	if errors.Is(err, privdir.ErrAbsent) {
		return -1, true, nil
	}
	return fd, false, err
}

// locked runs fn with the directory pinned and the lock held. A reader
// (write false) of a store that does not exist yet runs nothing and
// returns nil.
func (s *Store) locked(write bool, fn func(dir int) error) error {
	dir, absent, err := s.open(write)
	if err != nil {
		return fmt.Errorf("events store: %w", err)
	}
	if absent {
		return nil
	}
	defer unix.Close(dir) //nolint:errcheck // a directory descriptor; nothing to flush
	lock, err := privdir.OpenFile(dir, lockName, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("events store: %w", err)
	}
	defer lock.Close() //nolint:errcheck // the lock file holds no data
	how := unix.LOCK_SH
	if write {
		how = unix.LOCK_EX
	}
	if err := unix.Flock(int(lock.Fd()), how); err != nil {
		return fmt.Errorf("events store: lock: %w", err)
	}
	return fn(dir)
}

// serialPoll runs fn holding the poll lock, so two polls never overlap:
// one that fetched an older head cannot record after one that fetched a
// newer head. It is a separate file from the store lock, so readers and
// watchers are not held up while a poll waits on GitHub.
func (s *Store) serialPoll(fn func() error) error {
	dir, _, err := s.open(true)
	if err != nil {
		return fmt.Errorf("events store: %w", err)
	}
	defer unix.Close(dir) //nolint:errcheck // a directory descriptor; nothing to flush
	lock, err := privdir.OpenFile(dir, pollLock, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("events store: %w", err)
	}
	defer lock.Close() //nolint:errcheck // the lock file holds no data
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("events store: poll lock: %w", err)
	}
	return fn()
}

// readJSON fills v from name; a missing file leaves v as it is.
func readJSON(dir int, name string, v any) error {
	f, err := privdir.OpenFile(dir, name, os.O_RDONLY, 0o600)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only
	b, err := io.ReadAll(io.LimitReader(f, maxJSONBytes+1))
	if err != nil {
		return err
	}
	if len(b) > maxJSONBytes {
		return fmt.Errorf("%s is larger than %d bytes", name, maxJSONBytes)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s does not parse", name)
	}
	return nil
}

// writeAtomic replaces name with b: written to name.tmp, synced, renamed.
func writeAtomic(dir int, name string, b []byte) error {
	tmp := name + ".tmp"
	if err := unix.Unlinkat(dir, tmp, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	f, err := privdir.OpenFile(dir, tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return unix.Renameat(dir, tmp, dir, name)
}

func writeJSON(dir int, name string, v any) error {
	// termsafe:allow-raw-json a private state file, not terminal output; every value in it is validated
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if len(b) > maxJSONBytes {
		return fmt.Errorf("%s would be larger than %d bytes", name, maxJSONBytes)
	}
	return writeAtomic(dir, name, b)
}

// appendEvents adds one line per event to name, then cuts the file to its
// newest keepLines lines once it passes maxLogBytes.
func appendEvents(dir int, name string, evs []Event) error {
	if len(evs) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for _, e := range evs {
		if err := e.Validate(); err != nil {
			return err
		}
		// termsafe:allow-raw-json a private feed line, not terminal output; Validate ran just above
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	f, err := privdir.OpenFile(dir, name, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		_ = f.Close()
		return err
	}
	info, err := f.Stat()
	_ = f.Close()
	if err != nil || info.Size() <= maxLogBytes {
		return err
	}
	lines, err := readLines(dir, name)
	if err != nil {
		return err
	}
	if len(lines) > keepLines {
		lines = lines[len(lines)-keepLines:]
	}
	return writeAtomic(dir, name, append(bytes.Join(lines, []byte("\n")), '\n'))
}

// readLines is name's lines, without their newlines; a missing file is none.
func readLines(dir int, name string) ([][]byte, error) {
	f, err := privdir.OpenFile(dir, name, os.O_RDONLY, 0o600)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	var out [][]byte
	sc := bufio.NewScanner(io.LimitReader(f, 2*maxLogBytes))
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	for sc.Scan() {
		if line := bytes.TrimSpace(sc.Bytes()); len(line) > 0 {
			out = append(out, append([]byte(nil), line...))
		}
	}
	return out, sc.Err()
}

// Watch adds refs to session's watch list.
func (s *Store) Watch(session string, refs []Ref) error {
	if !ValidSession(session) {
		return ErrBadSession
	}
	return s.locked(true, func(dir int) error {
		var f watchFile
		if err := readJSON(dir, watchName, &f); err != nil {
			return err
		}
		w := parseWatch(f)
		if err := w.add(session, refs); err != nil {
			return err
		}
		return writeJSON(dir, watchName, w.file())
	})
}

// Unwatch takes refs off session's list, or every ref when refs is empty.
func (s *Store) Unwatch(session string, refs []Ref) error {
	if !ValidSession(session) {
		return ErrBadSession
	}
	return s.locked(true, func(dir int) error {
		var f watchFile
		if err := readJSON(dir, watchName, &f); err != nil {
			return err
		}
		w := parseWatch(f)
		w.remove(session, refs)
		return writeJSON(dir, watchName, w.file())
	})
}

// Watched is every session's watch list.
func (s *Store) Watched() (Watching, error) {
	w := Watching{}
	err := s.locked(false, func(dir int) error {
		var f watchFile
		if err := readJSON(dir, watchName, &f); err != nil {
			return err
		}
		w = parseWatch(f)
		return nil
	})
	return w, err
}

// State is ref's last snapshot; ok is false when the poller has none.
func (s *Store) State(ref Ref) (snap PRState, ok bool, err error) {
	err = s.locked(false, func(dir int) error {
		var st stateFile
		if err := readJSON(dir, stateName, &st); err != nil {
			return err
		}
		got, found := st.PRs[ref.String()]
		if found && got.Validate() == nil && got.Ref() == ref {
			snap, ok = got, true
		}
		return nil
	})
	return snap, ok, err
}

// Record stores fresh snapshots and returns the events they make, stamped.
// Each event goes to the machine log and to the feed of every session that
// watches its PR. A merged or closed PR comes off every watch list after its
// event is written.
func (s *Store) Record(now time.Time, snaps []PRState) ([]Event, error) {
	var out []Event
	err := s.locked(true, func(dir int) error {
		var wf watchFile
		if err := readJSON(dir, watchName, &wf); err != nil {
			return err
		}
		w := parseWatch(wf)
		st := stateFile{PRs: map[string]PRState{}}
		if err := readJSON(dir, stateName, &st); err != nil {
			return err
		}
		if st.PRs == nil {
			st.PRs = map[string]PRState{}
		}
		feeds := map[string][]Event{}
		finished := false
		for _, snap := range snaps {
			if err := snap.Validate(); err != nil {
				continue
			}
			key := snap.Ref().String()
			var old *PRState
			if prev, ok := st.PRs[key]; ok && prev.Validate() == nil {
				old = &prev
			}
			evs := Diff(old, snap)
			st.Seq = stamp(evs, st.Seq, now)
			st.PRs[key] = snap
			for _, sid := range w.watchers(snap.Ref()) {
				feeds[sid] = append(feeds[sid], evs...)
			}
			out = append(out, evs...)
			if snap.State != "OPEN" {
				w.finish(snap.Ref())
				finished = true
			}
		}
		if err := appendEvents(dir, logName, out); err != nil {
			return err
		}
		for sid, evs := range feeds {
			if err := appendEvents(dir, feedName(sid), evs); err != nil {
				return err
			}
		}
		prune(&st, w, now)
		if err := writeJSON(dir, stateName, st); err != nil {
			return err
		}
		if finished {
			return writeJSON(dir, watchName, w.file())
		}
		return nil
	})
	return out, err
}

// Feed is session's events with Seq above since, oldest first, and the
// highest Seq in the feed (since when it is empty). A line that does not
// parse or validate is skipped and counted.
func (s *Store) Feed(session string, since int64) (evs []Event, last int64, skipped int, err error) {
	if !ValidSession(session) {
		return nil, since, 0, ErrBadSession
	}
	last = since
	err = s.locked(false, func(dir int) error {
		lines, err := readLines(dir, feedName(session))
		if err != nil {
			return err
		}
		for _, line := range lines {
			var e Event
			if json.Unmarshal(line, &e) != nil || e.Validate() != nil {
				skipped++
				continue
			}
			if e.Seq > last {
				last = e.Seq
			}
			if e.Seq > since {
				evs = append(evs, e)
			}
		}
		return nil
	})
	return evs, last, skipped, err
}
