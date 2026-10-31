//go:build unix

// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package ask

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
)

// Store layout under the desk directory.
const (
	// SubDir is the asks directory inside the desk directory.
	SubDir = "asks"
	// LogName is the append-only log inside SubDir.
	LogName = "log.jsonl"
	// maxLogBytes caps what a reader loads. A log past it is refused, not
	// truncated: a fold over part of the log would show answered asks as open.
	maxLogBytes = 32 << 20
	dirMode     = 0o700
	fileMode    = 0o600
	// notifyTimeout bounds one notify call.
	notifyTimeout = 10 * time.Second
)

// Notifier tells the asking agent about a response. addr is the ask's
// recorded terminal address; text is one line.
type Notifier interface {
	Notify(ctx context.Context, addr, text string) error
}

// Store is the asks log in one desk directory.
type Store struct {
	dir      string
	now      func() time.Time
	notifier Notifier
	readOnly bool
}

// Option configures a Store.
type Option func(*Store)

// WithClock sets the store's clock (tests).
func WithClock(now func() time.Time) Option { return func(s *Store) { s.now = now } }

// WithNotifier sets who is told about responses. Without one, a response to
// an ask with a notify address records a failed notify.
func WithNotifier(n Notifier) Option { return func(s *Store) { s.notifier = n } }

// Open opens the asks store in deskDir, creating deskDir/asks (mode 0700) if
// it is missing. deskDir must be absolute.
func Open(deskDir string, opts ...Option) (*Store, error) {
	s, err := newStore(deskDir, opts)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(deskDir, dirMode); err != nil {
		return nil, fmt.Errorf("ask: create the desk directory: %w", err)
	}
	if err := os.Mkdir(s.dir, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("ask: create %s: %w", SubDir, err)
	}
	if err := checkDir(s.dir); err != nil {
		return nil, err
	}
	return s, nil
}

// OpenReadOnly opens the store without creating anything. A desk with no
// asks directory reads as an empty log.
func OpenReadOnly(deskDir string, opts ...Option) (*Store, error) {
	s, err := newStore(deskDir, opts)
	if err != nil {
		return nil, err
	}
	s.readOnly = true
	if err := checkDir(s.dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return s, nil
}

func newStore(deskDir string, opts []Option) (*Store, error) {
	if !filepath.IsAbs(deskDir) {
		return nil, errors.New("ask: the desk directory must be an absolute path")
	}
	s := &Store{dir: filepath.Join(filepath.Clean(deskDir), SubDir), now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// checkDir refuses an asks directory that is a symlink or not a directory,
// and narrows a group- or world-accessible one to 0700.
func checkDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("ask: %s: %w", SubDir, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("ask: %s is not a plain directory", SubDir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, dirMode); err != nil {
			return fmt.Errorf("ask: narrow %s to 0700: %w", SubDir, err)
		}
	}
	return nil
}

// LogPath is the log's path.
func (s *Store) LogPath() string { return filepath.Join(s.dir, LogName) }

// openLog opens the log without following a symlink, refusing anything that
// is not a regular file.
func (s *Store) openLog(flags int) (*os.File, error) {
	f, err := os.OpenFile(s.LogPath(), flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, fileMode) //nolint:gosec // G304: a fixed name inside the desk's asks directory
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("ask: %s is not a regular file", LogName)
	}
	return f, nil
}

// ReadFrom returns the complete records after byte offset off, and the
// offset just past the last complete one. A line still being written (no
// newline yet) is left for the next read. A line that is not a record is
// skipped and counted in bad.
func (s *Store) ReadFrom(off int64) (recs []Record, next int64, bad int, err error) {
	f, err := s.openLog(os.O_RDONLY)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, 0, nil
	}
	if err != nil {
		return nil, off, 0, fmt.Errorf("ask: open the log: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	fi, err := f.Stat()
	if err != nil {
		return nil, off, 0, err
	}
	if fi.Size() > maxLogBytes {
		return nil, off, 0, fmt.Errorf("ask: %s is larger than %d MiB; move it aside to start a new log (answered asks in it will no longer show)", LogName, maxLogBytes>>20)
	}
	if off < 0 || off > fi.Size() {
		return nil, off, 0, fmt.Errorf("ask: offset %d is outside the log (size %d)", off, fi.Size())
	}
	data, err := io.ReadAll(io.NewSectionReader(f, off, fi.Size()-off))
	if err != nil {
		return nil, off, 0, err
	}
	recs, used, bad := parseRecords(data, off)
	return recs, off + used, bad, nil
}

// parseRecords parses complete lines of data, whose first byte sits at
// offset base in the log. It returns the bytes consumed.
func parseRecords(data []byte, base int64) (recs []Record, used int64, bad int) {
	for {
		nl := bytes.IndexByte(data[used:], '\n')
		if nl < 0 {
			return recs, used, bad
		}
		line := data[used : used+int64(nl)]
		used += int64(nl) + 1
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil || !rec.wellFormed() {
			bad++
			continue
		}
		rec.Offset = base + used
		recs = append(recs, rec)
	}
}

// wellFormed reports a record with exactly one payload whose ids have the
// shape the store writes.
func (r Record) wellFormed() bool {
	n := 0
	if r.Ask != nil {
		n++
		if !ValidID(r.Ask.ID) {
			return false
		}
	}
	if r.Response != nil {
		n++
		if !ValidID(r.Response.ID) || !ValidID(r.Response.Ask) {
			return false
		}
	}
	if r.Notified != nil {
		n++
		if !ValidID(r.Notified.Response) {
			return false
		}
	}
	return n == 1
}

// Records reads the whole log.
func (s *Store) Records() ([]Record, error) {
	recs, _, _, err := s.ReadFrom(0)
	return recs, err
}

// Items reads the log and folds it.
func (s *Store) Items() ([]Item, error) {
	recs, err := s.Records()
	if err != nil {
		return nil, err
	}
	return Fold(recs, s.now()), nil
}

// withLock runs fn holding the log's exclusive lock.
func (s *Store) withLock(fn func() error) error {
	if s.readOnly {
		return errors.New("ask: the store was opened read-only")
	}
	return config.WithFileLock(s.LogPath(), fn)
}

// appendRecord writes one record as one line. The caller holds the lock.
func (s *Store) appendRecord(rec Record) error {
	rec.V = recordVersion
	// termsafe:allow-raw-json persisted log record, never command output
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	f, err := s.openLog(os.O_RDWR | os.O_APPEND | os.O_CREATE)
	if err != nil {
		return fmt.Errorf("ask: open the log for append: %w", err)
	}
	// A writer that died mid-line left a torn last line. Appending straight
	// after it would glue this record onto it and lose both, so end it first.
	if torn, err := endsTorn(f); err != nil {
		_ = f.Close()
		return fmt.Errorf("ask: check the log's last line: %w", err)
	} else if torn {
		line = append([]byte{'\n'}, line...)
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return fmt.Errorf("ask: append to the log: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("ask: sync the log: %w", err)
	}
	return f.Close()
}

// endsTorn reports whether f is non-empty and its last byte is not a newline.
func endsTorn(f *os.File) (bool, error) {
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return false, err
	}
	var b [1]byte
	if _, err := f.ReadAt(b[:], fi.Size()-1); err != nil {
		return false, err
	}
	return b[0] != '\n', nil
}

// newID returns prefix and 8 random hex digits not already used in items.
func newID(prefix string, taken func(string) bool) (string, error) {
	for range 8 {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		id := prefix + hex.EncodeToString(b[:])
		if !taken(id) {
			return id, nil
		}
	}
	return "", errors.New("ask: could not pick an unused id")
}

// Add validates a and appends it, returning it with its id and time set.
func (s *Store) Add(a Ask) (Ask, error) {
	a, err := Normalize(a)
	if err != nil {
		return a, err
	}
	err = s.withLock(func() error {
		recs, err := s.Records()
		if err != nil {
			return err
		}
		used := map[string]bool{}
		for _, r := range recs {
			if r.Ask != nil {
				used[r.Ask.ID] = true
			}
		}
		open := 0
		for _, it := range Fold(recs, s.now()) {
			if it.State != StateAnswered && it.Ask.From == a.From {
				open++
			}
		}
		if open >= MaxOpenPerAsker {
			who := a.From
			if who == "" {
				who = "an unnamed asker"
			}
			return fmt.Errorf("%w: %s already has %d open asks (the limit); wait for answers before asking more", ErrTooMany, clip(who, 40), open)
		}
		a.ID, err = newID("ask-", func(id string) bool { return used[id] })
		if err != nil {
			return err
		}
		a.At = s.now().UTC()
		return s.appendRecord(Record{Ask: &a})
	})
	return a, err
}

// Respond records r against its ask, then tells the asking agent when the
// ask carries a notify address. The check that the ask is open and accepts
// r.Kind and the append happen under one lock, so two front ends answering
// at once cannot both answer it. A failed notify does not undo the response:
// it is recorded as a notified line with ok=false and returned as notifyErr.
func (s *Store) Respond(ctx context.Context, r Response) (it Item, saved Response, notifyErr error, err error) {
	if !ValidID(r.Ask) {
		return Item{}, r, nil, fmt.Errorf("%w: %q", ErrNotFound, clip(r.Ask, 40))
	}
	err = s.withLock(func() error {
		recs, err := s.Records()
		if err != nil {
			return err
		}
		now := s.now()
		items := Fold(recs, now)
		cur, ok := Find(items, r.Ask)
		if !ok {
			return fmt.Errorf("%w: %s", ErrNotFound, r.Ask)
		}
		r, err = CheckResponse(cur, r, now)
		if err != nil {
			return err
		}
		used := map[string]bool{}
		for _, rec := range recs {
			if rec.Response != nil {
				used[rec.Response.ID] = true
			}
		}
		r.ID, err = newID("resp-", func(id string) bool { return used[id] })
		if err != nil {
			return err
		}
		r.At = now.UTC()
		r.Origin = OriginOf(r.Via)
		if err := s.appendRecord(Record{Response: &r}); err != nil {
			return err
		}
		it = cur
		return nil
	})
	if err != nil {
		return Item{}, r, nil, err
	}
	if it.Ask.Notify != "" {
		notifyErr = s.notify(ctx, it.Ask, r)
	}
	items, rerr := s.Items()
	if rerr == nil {
		if fresh, ok := Find(items, r.Ask); ok {
			it = fresh
		}
	}
	return it, r, notifyErr, nil
}

// notify sends the response to the ask's terminal and records the outcome.
func (s *Store) notify(ctx context.Context, a Ask, r Response) error {
	var sendErr error
	if s.notifier == nil {
		sendErr = errors.New("no notifier configured")
	} else {
		cctx, cancel := context.WithTimeout(ctx, notifyTimeout)
		sendErr = s.notifier.Notify(cctx, a.Notify, NotifyText(a, r))
		cancel()
	}
	n := Notified{Response: r.ID, At: s.now().UTC(), Address: a.Notify, OK: sendErr == nil}
	if sendErr != nil {
		n.Error = clip(flatten(sendErr.Error(), 300), 300)
	}
	if err := s.withLock(func() error { return s.appendRecord(Record{Notified: &n}) }); err != nil {
		return errors.Join(sendErr, fmt.Errorf("record the notify outcome: %w", err))
	}
	if sendErr != nil {
		return fmt.Errorf("ask: notify %s: %w", a.Notify, sendErr)
	}
	return nil
}
