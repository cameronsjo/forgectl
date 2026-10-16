//go:build unix

package attention

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/privdir"
)

const (
	// spoolParent and spoolLeaf place the spool at <state>/cadence/attention.
	spoolParent = "cadence"
	spoolLeaf   = "attention"
	dirMode     = 0o700
	fileMode    = 0o600
	// ancestorMode is for directories created above <state>/cadence.
	ancestorMode = 0o755
	// maxRecordBytes caps a record read back. One written here is far
	// smaller: MaxItems texts of MaxText runes.
	maxRecordBytes = 128 << 10
	recordExt      = ".json"
)

var (
	recordNameRe = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9_-]{0,63})\.json$`)
	tmpNameRe    = regexp.MustCompile(`^\.[A-Za-z0-9][A-Za-z0-9_-]{0,63}\.json\.tmp-[A-Za-z0-9]{1,32}$`)
)

// tmpSuffix names a write's temporary file; a test pins it.
var tmpSuffix = func() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Spool is the open spool directory, pinned by descriptor: after OpenSpool
// no operation resolves the directory's name again.
type Spool struct {
	path string
	fd   int
}

// OpenSpool opens <stateBase>/cadence/attention. With create it makes the
// directory (0700) when absent. It refuses a spool directory that is a
// symlink, is not this user's, or has any permission bit beyond 0700: an
// existing directory is never narrowed, since a wide one means something
// other than forgectl made or changed it. Without create, an absent spool
// is ErrNoSpool.
func OpenSpool(stateBase string, create bool) (*Spool, error) {
	if !filepath.IsAbs(stateBase) {
		return nil, errors.New("attention: the state directory must be an absolute path")
	}
	spec := privdir.Spec{
		Base:         filepath.Join(filepath.Clean(stateBase), spoolParent),
		Leaf:         spoolLeaf,
		Mode:         dirMode,
		AncestorMode: ancestorMode,
		Create:       create,
	}
	path := filepath.Join(spec.Base, spec.Leaf)
	fd, err := privdir.OpenChecked(spec)
	if errors.Is(err, privdir.ErrAbsent) {
		if !create {
			return nil, ErrNoSpool
		}
		fd, err = privdir.Pin(spec)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnsafeSpool, privdir.Reason(err))
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd) // refusing; the stat error is the one returned
		return nil, fmt.Errorf("%w: stat: %w", ErrUnsafeSpool, err)
	}
	if perm := uint32(st.Mode) & 0o7777; perm != dirMode {
		_ = unix.Close(fd) // refusing; the mode is the error returned
		return nil, fmt.Errorf("%w: the spool directory has mode %04o; it must be 0700 (fix it with chmod 700)", ErrUnsafeSpool, perm)
	}
	return &Spool{path: path, fd: fd}, nil
}

// Path is the spool directory, for messages.
func (s *Spool) Path() string { return s.path }

// Close releases the directory descriptor.
func (s *Spool) Close() error { return unix.Close(s.fd) }

// Put writes r as <session>.json: validated, encoded, written to a fresh
// O_EXCL temporary file (0600) in the spool, then renamed over the record.
// An existing record name that is not a regular file is refused.
func (s *Spool) Put(r Record) error {
	r, err := r.sanitize()
	if err != nil {
		return err
	}
	name := r.Session + recordExt
	if err := s.refuseNonRegular(name); err != nil {
		return err
	}
	// termsafe:allow-raw-json persisted spool record, never command output; list re-cleans it on read
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	suffix, err := tmpSuffix()
	if err != nil {
		return fmt.Errorf("attention: tmp name: %w", err)
	}
	tmp := "." + name + ".tmp-" + suffix
	f, err := privdir.OpenFile(s.fd, tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return fmt.Errorf("attention: create %s: %s", tmp, privdir.Reason(err))
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = unix.Unlinkat(s.fd, tmp, 0) // cleanup; the write error is the one returned
		return fmt.Errorf("attention: write %s: %w", tmp, err)
	}
	if err := unix.Renameat(s.fd, tmp, s.fd, name); err != nil {
		_ = unix.Unlinkat(s.fd, tmp, 0) // cleanup; the rename error is the one returned
		return fmt.Errorf("attention: rename %s: %w", name, err)
	}
	return nil
}

// Remove deletes the session's record. An absent record is not an error;
// a record name that is not a regular file is refused.
func (s *Spool) Remove(session string) error {
	if !ValidSession(session) {
		return invalid("session is not a session id")
	}
	name := session + recordExt
	var st unix.Stat_t
	err := unix.Fstatat(s.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil
	case err != nil:
		return fmt.Errorf("attention: stat %s: %w", name, err)
	case st.Mode&unix.S_IFMT != unix.S_IFREG:
		return fmt.Errorf("%w: %s is not a regular file; refusing", ErrUnsafeSpool, name)
	}
	if err := unix.Unlinkat(s.fd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("attention: remove %s: %w", name, err)
	}
	return nil
}

// Get reads the session's record. A missing record is ok=false; one that
// is unsafe or malformed is an error.
func (s *Spool) Get(session string) (Record, bool, error) {
	if !ValidSession(session) {
		return Record{}, false, invalid("session is not a session id")
	}
	r, _, err := s.read(session + recordExt)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	return r, true, nil
}

// List reads every record and prunes as it goes (see [shouldPrune]): a
// record whose pane is gone from the caller's herdr server, or one older than
// ttl with no pane the caller can see. A malformed record, a record name that
// is a symlink or hardlink, and an entry that is not a record are skipped and
// counted, never returned; a malformed record older than ttl, and a leftover
// temporary file older than ttl, are removed. Records come back in name
// order.
func (s *Spool) List(now time.Time, ttl time.Duration, live Liveness) ([]Record, int, error) {
	names, err := s.names()
	if err != nil {
		return nil, 0, err
	}
	var recs []Record
	skipped := 0
	for _, name := range names {
		if tmpNameRe.MatchString(name) {
			s.removeIfOlder(name, now, ttl)
			continue
		}
		if !recordNameRe.MatchString(name) {
			skipped++
			continue
		}
		r, st, err := s.read(name)
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue // removed since the listing
		case err != nil:
			skipped++
			if st == nil {
				// Refused before a stat (a symlink, a hardlink, another
				// user's file): age the name itself, never what it points at.
				st = s.lstat(name)
			}
			if st != nil && now.Sub(mtime(st)) > ttl && (live.Keep == "" || name != live.Keep+recordExt) {
				s.removeSame(name, st)
			}
			continue
		}
		if shouldPrune(r, now.Sub(mtime(st)), ttl, live) {
			s.removeSame(name, st)
			continue
		}
		recs = append(recs, r)
	}
	return recs, skipped, nil
}

// EvictPane removes every valid record other than keep's that names pane on
// socket: that pane now runs session keep, so those records are from a
// session it replaced (after /clear, or a new session in the same pane).
func (s *Spool) EvictPane(pane, socket, keep string) error {
	if pane == "" || socket == "" {
		return nil
	}
	names, err := s.names()
	if err != nil {
		return err
	}
	for _, name := range names {
		m := recordNameRe.FindStringSubmatch(name)
		if m == nil || m[1] == keep {
			continue
		}
		r, st, err := s.read(name)
		if err != nil {
			continue
		}
		if r.Pane == pane && r.Socket == socket {
			s.removeSame(name, st)
		}
	}
	return nil
}

// read opens name through privdir.OpenFile (no symlink, one link, this
// user's, 0600) and decodes it strictly. The record must name the session
// its file is named for. st is the opened file's stat when the open got that
// far, so a caller can age and remove a malformed record.
func (s *Spool) read(name string) (Record, *unix.Stat_t, error) {
	f, err := privdir.OpenFile(s.fd, name, os.O_RDONLY, fileMode)
	if err != nil {
		return Record{}, nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return Record{}, nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
	if err != nil {
		return Record{}, &st, err
	}
	if len(data) > maxRecordBytes {
		return Record{}, &st, invalid("record is over %d bytes", maxRecordBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r Record
	if err := dec.Decode(&r); err != nil {
		return Record{}, &st, invalid("record is not valid JSON")
	}
	if dec.More() {
		return Record{}, &st, invalid("record has trailing data")
	}
	r, err = r.sanitize()
	if err != nil {
		return Record{}, &st, err
	}
	if r.Session+recordExt != name {
		return Record{}, &st, invalid("record names a session other than its file")
	}
	return r, &st, nil
}

// names lists the spool through a fresh descriptor on the pinned directory,
// so the listing never resolves the spool's name.
func (s *Spool) names() ([]string, error) {
	fd, err := unix.Openat(s.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("attention: list %s: %w", s.path, err)
	}
	dir := os.NewFile(uintptr(fd), s.path)
	defer dir.Close() //nolint:errcheck // read-only
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("attention: list %s: %w", s.path, err)
	}
	slices.Sort(names)
	return names, nil
}

// refuseNonRegular refuses a name that exists and is not a regular file.
func (s *Spool) refuseNonRegular(name string) error {
	var st unix.Stat_t
	err := unix.Fstatat(s.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil
	case err != nil:
		return fmt.Errorf("attention: stat %s: %w", name, err)
	case st.Mode&unix.S_IFMT != unix.S_IFREG:
		return fmt.Errorf("%w: %s is not a regular file; refusing", ErrUnsafeSpool, name)
	}
	return nil
}

// lstat is name's own stat, not following a symlink, or nil.
func (s *Spool) lstat(name string) *unix.Stat_t {
	var st unix.Stat_t
	if err := unix.Fstatat(s.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil
	}
	return &st
}

// removeSame unlinks name only while it is still the file that was read, so
// a record rewritten since is kept.
func (s *Spool) removeSame(name string, read *unix.Stat_t) {
	var now unix.Stat_t
	if err := unix.Fstatat(s.fd, name, &now, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return
	}
	if now.Dev != read.Dev || now.Ino != read.Ino {
		return
	}
	_ = unix.Unlinkat(s.fd, name, 0) // prune is best effort: a record left behind is pruned on the next list
}

// removeIfOlder unlinks a leftover temporary file older than ttl. A symlink
// at a temporary name is unlinked, never followed.
func (s *Spool) removeIfOlder(name string, now time.Time, ttl time.Duration) {
	var st unix.Stat_t
	if err := unix.Fstatat(s.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return
	}
	if now.Sub(mtime(&st)) > ttl {
		_ = unix.Unlinkat(s.fd, name, 0) // best effort, as removeSame
	}
}

func mtime(st *unix.Stat_t) time.Time {
	sec, nsec := st.Mtim.Unix()
	return time.Unix(sec, nsec)
}
