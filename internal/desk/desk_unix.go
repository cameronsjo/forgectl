//go:build unix

package desk

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/privdir"
)

const (
	dirMode  fs.FileMode = 0o700
	fileMode fs.FileMode = 0o600
	// ancestorMode is for directories created above the desk, such as
	// ~/.local/state. Making the desk private must not make those private.
	ancestorMode fs.FileMode = 0o755
	maxMetaBytes             = 64 << 10
)

// Desk is an open desk directory. Every path below it is resolved through a
// root pinned at [Open]; nothing re-resolves the desk's own name.
type Desk struct {
	path string
	fd   int // pinned descriptor on the desk directory, from privdir
	root *os.Root
	now  func() time.Time
	// grace is how long a finished script's leftover processes get between
	// SIGTERM and SIGKILL.
	grace time.Duration
}

// Open pins the desk directory at path, creating it when absent, and migrates
// it: the four protocol subdirectories are created or tightened to 0700 and
// their regular files to 0600. Anything else at the desk root is left alone.
//
// The desk directory itself is pinned through privdir first, which proves it
// is a directory this user owns and narrows it to 0700. The subdirectories are
// tightened after that, through the pinned root, so a chmod never lands on an
// object that was not proven ours.
func Open(dir string) (*Desk, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("desk: the desk directory must be an absolute path")
	}
	dir = filepath.Clean(dir)
	fd, err := privdir.Pin(privdir.Spec{
		Base:         filepath.Dir(dir),
		Leaf:         filepath.Base(dir),
		Mode:         dirMode,
		AncestorMode: ancestorMode,
		Create:       true,
	})
	if err != nil {
		return nil, fmt.Errorf("desk: open %s: %w", describe(dir), err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("desk: open %s: %w", describe(dir), err)
	}
	d := &Desk{path: dir, fd: fd, root: root, now: time.Now, grace: DefaultGrace}
	if err := d.bindRoot(); err != nil {
		_ = d.Close()
		return nil, err
	}
	if err := d.migrate(); err != nil {
		_ = d.Close()
		return nil, err
	}
	return d, nil
}

// bindRoot proves the os.Root opened by name is the directory privdir pinned,
// so a swap between the two opens is refused rather than followed.
func (d *Desk) bindRoot() error {
	dup, err := unix.Dup(d.fd)
	if err != nil {
		return fmt.Errorf("desk: dup pinned dir: %w", err)
	}
	pinned := os.NewFile(uintptr(dup), d.path)
	defer pinned.Close() //nolint:errcheck // read-only descriptor
	a, err := pinned.Stat()
	if err != nil {
		return fmt.Errorf("desk: stat pinned dir: %w", err)
	}
	b, err := d.root.Stat(".")
	if err != nil {
		return fmt.Errorf("desk: stat desk root: %w", err)
	}
	if !os.SameFile(a, b) {
		return errors.New("desk: the desk directory changed while it was being opened; refusing")
	}
	return nil
}

// Close releases the desk's descriptors.
func (d *Desk) Close() error {
	err := d.root.Close()
	if cerr := unix.Close(d.fd); err == nil {
		err = cerr
	}
	return err
}

// Path is the desk directory.
func (d *Desk) Path() string { return d.path }

func (d *Desk) migrate() error {
	for _, sub := range protocolDirs {
		if err := d.root.Mkdir(sub, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("desk: create %s/: %w", sub, err)
		}
		if err := d.tightenDir(sub); err != nil {
			return err
		}
	}
	return nil
}

// tightenDir narrows one protocol dir to 0700 and its regular files to 0600.
// Every chmod goes through a descriptor opened O_NOFOLLOW against the pinned
// desk, so a symlink swapped in after the listing is refused, never followed.
func (d *Desk) tightenDir(sub string) error {
	sfd, err := unix.Openat(d.fd, sub, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.EMLINK):
		return fmt.Errorf("desk: %s/ is not a directory; refusing", sub)
	case errors.Is(err, unix.EACCES):
		return fmt.Errorf("desk: %s/ is not readable by its owner; it needs mode 0700 (chmod 700 %s)", sub, describe(d.abs(sub)))
	case err != nil:
		return fmt.Errorf("desk: open %s/: %w", sub, err)
	}
	defer unix.Close(sfd) //nolint:errcheck // read-only descriptor
	if err := fchmodTo(sfd, dirMode); err != nil {
		return fmt.Errorf("desk: tighten %s/: %w", sub, err)
	}
	entries, err := d.list(sub)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		ffd, err := unix.Openat(sfd, e.Name(), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK) || errors.Is(err, unix.EACCES) {
			// Vanished or swapped for a symlink since the listing, or a mode
			// its owner cannot read (000, 0200). Left as it is: readItem
			// refuses an item it cannot read, so it never runs, and one odd
			// file must not stop every desk command.
			continue
		}
		if err != nil {
			return fmt.Errorf("desk: open %s/%s: %w", sub, describe(e.Name()), err)
		}
		var st unix.Stat_t
		err = unix.Fstat(ffd, &st)
		if err == nil && st.Mode&unix.S_IFMT == unix.S_IFREG {
			err = fchmodTo(ffd, fileMode)
		}
		_ = unix.Close(ffd)
		if err != nil {
			return fmt.Errorf("desk: tighten %s/%s: %w", sub, describe(e.Name()), err)
		}
	}
	return nil
}

// fchmodTo sets fd's permission bits to mode when they differ.
func fchmodTo(fd int, mode fs.FileMode) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if uint32(st.Mode)&0o7777 == uint32(mode) { // all twelve bits, so a stray setgid is cleared too
		return nil
	}
	return unix.Fchmod(fd, uint32(mode))
}

// list reads a protocol subdirectory, sorted by name.
func (d *Desk) list(sub string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(d.root.FS(), sub)
	if err != nil {
		return nil, fmt.Errorf("desk: read %s/: %w", sub, err)
	}
	return entries, nil
}

// refusal is an item that must not run: not a regular file, more than one
// link, or too large. It matches ErrRefused.
type refusal struct{ reason string }

func (r *refusal) Error() string        { return "desk: item refused: " + r.reason }
func (r *refusal) Is(target error) bool { return target == ErrRefused }

// readItem reads an item's bytes through the pinned descriptor. The final
// component is opened O_NOFOLLOW, so a symlink is refused rather than
// followed, and the open file must be a regular file with exactly one link:
// a hard link elsewhere could change the bytes after the hash is checked.
// O_NONBLOCK keeps a FIFO planted at the name from blocking the open.
func (d *Desk) readItem(sub, file string) (data []byte, mtime time.Time, err error) {
	dfd, err := unix.Openat(d.fd, sub, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("desk: open %s/: %w", sub, err)
	}
	defer unix.Close(dfd) //nolint:errcheck // read-only descriptor
	fd, err := unix.Openat(dfd, file, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil, time.Time{}, ErrNotFound
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.EMLINK):
		// EMLINK is FreeBSD's answer to O_NOFOLLOW on a symlink.
		return nil, time.Time{}, &refusal{"a symlink"}
	case errors.Is(err, unix.EACCES):
		return nil, time.Time{}, &refusal{"not readable"}
	case err != nil:
		return nil, time.Time{}, fmt.Errorf("desk: open %s/%s: %w", sub, describe(file), err)
	}
	f := os.NewFile(uintptr(fd), file)
	defer f.Close() //nolint:errcheck // read-only descriptor
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, time.Time{}, fmt.Errorf("desk: stat %s/%s: %w", sub, describe(file), err)
	}
	switch {
	case st.Mode&unix.S_IFMT != unix.S_IFREG:
		return nil, time.Time{}, &refusal{"not a regular file"}
	case st.Nlink != 1:
		return nil, time.Time{}, &refusal{fmt.Sprintf("a hard link (link count %d)", st.Nlink)}
	case st.Size > maxItemBytes:
		return nil, time.Time{}, &refusal{"larger than 1 MiB"}
	}
	data, err = io.ReadAll(io.LimitReader(f, maxItemBytes+1))
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("desk: read %s/%s: %w", sub, describe(file), err)
	}
	if len(data) > maxItemBytes {
		return nil, time.Time{}, &refusal{"larger than 1 MiB"}
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("desk: stat %s/%s: %w", sub, describe(file), err)
	}
	return data, fi.ModTime().UTC(), nil
}

func metaName(sub, name string) string { return path.Join(sub, name+extMeta) }

// openRegular opens a root-relative path for reading, refusing anything but
// a regular file without ever blocking or following a final symlink: the
// name is Lstat'ed, opened O_NONBLOCK (so a FIFO planted at it cannot hang
// the open), and the open file must be that same regular file. os.Root keeps
// every component inside the desk but follows a final symlink that stays in
// it; the Lstat/SameFile pair refuses that too. A missing file reads as
// fs.ErrNotExist; anything else that is not a regular file is ErrRefused.
func (d *Desk) openRegular(p string) (*os.File, error) {
	before, err := d.root.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrRefused, describe(p))
	}
	f, err := d.root.OpenFile(p, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s changed while it was opened", ErrRefused, describe(p))
	}
	return f, nil
}

// readRegular reads a regular file through openRegular, refusing one larger
// than limit.
func (d *Desk) readRegular(p string, limit int64) ([]byte, error) {
	f, err := d.openRegular(p)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrRefused, describe(p), limit)
	}
	return data, nil
}

// readMeta reads <sub>/<name>.meta.json; ok is false when there is none.
func (d *Desk) readMeta(sub, name string) (m Meta, ok bool, err error) {
	p := metaName(sub, name)
	fi, err := d.root.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Meta{}, false, nil
	case err != nil:
		return Meta{}, false, fmt.Errorf("desk: stat %s: %w", describe(p), err)
	case !fi.Mode().IsRegular() || fi.Size() > maxMetaBytes:
		return Meta{}, false, fmt.Errorf("desk: %s is not a meta file; refusing", describe(p))
	}
	data, err := d.readRegular(p, maxMetaBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return Meta{}, false, nil
	}
	if err != nil {
		return Meta{}, false, fmt.Errorf("desk: read %s: %w", describe(p), err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return Meta{}, false, fmt.Errorf("desk: parse %s: %w", describe(p), err)
	}
	if m.SHA256 != "" && !ValidSHA256(m.SHA256) {
		// The hash is printed and compared; one that is not hex could carry
		// terminal escapes into `desk status`. The item is refused instead.
		return Meta{}, false, &refusal{reason: "its meta's sha256 is not 64 lowercase hex characters"}
	}
	return m, true, nil
}

// writeTemp writes data to a fresh dot-named file in sub and returns its
// root-relative path. Scans ignore dot names, so a half-written temp file is
// never read as an item.
func (d *Desk) writeTemp(sub, prefix string, data []byte) (string, error) {
	tmp := path.Join(sub, "."+prefix+"."+rand.Text()+".tmp")
	f, err := d.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return "", fmt.Errorf("desk: create temp file in %s/: %w", sub, err)
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = d.root.Remove(tmp)
		return "", fmt.Errorf("desk: write temp file in %s/: %w", sub, werr)
	}
	return tmp, nil
}

func encodeMeta(m Meta) ([]byte, error) {
	data, err := encodeJSON(m)
	if err != nil {
		return nil, fmt.Errorf("desk: encode meta: %w", err)
	}
	return data, nil
}

// writeMeta replaces <sub>/<name>.meta.json atomically.
func (d *Desk) writeMeta(sub, name string, m Meta) error {
	data, err := encodeMeta(m)
	if err != nil {
		return err
	}
	tmp, err := d.writeTemp(sub, name+".meta", data)
	if err != nil {
		return err
	}
	if err := d.root.Rename(tmp, metaName(sub, name)); err != nil {
		_ = d.root.Remove(tmp)
		return fmt.Errorf("desk: write meta for %s: %w", name, err)
	}
	return nil
}

// createMeta writes <sub>/<name>.meta.json only if it does not exist yet, and
// whole: a link(2) of a finished temp file, so no reader sees half a file and
// two desks sighting the same item cannot overwrite each other.
func (d *Desk) createMeta(sub, name string, m Meta) (created bool, err error) {
	data, err := encodeMeta(m)
	if err != nil {
		return false, err
	}
	tmp, err := d.writeTemp(sub, name+".meta", data)
	if err != nil {
		return false, err
	}
	defer d.root.Remove(tmp) //nolint:errcheck // best effort; a dot-named leftover is ignored by scans
	err = d.root.Link(tmp, metaName(sub, name))
	switch {
	case errors.Is(err, fs.ErrExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("desk: create meta for %s: %w", name, err)
	}
	return true, nil
}

// move renames an item and its meta from one protocol dir to another. The
// item's rename is the step that can fail on a race (ENOENT); the meta follows
// and may be absent for a legacy item.
func (d *Desk) move(name string, kind Kind, from, to string) error {
	file := name + kind.Ext()
	if err := d.root.Rename(path.Join(from, file), path.Join(to, file)); err != nil {
		return err
	}
	if err := d.root.Rename(metaName(from, name), metaName(to, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("desk: move meta for %s: %w", name, err)
	}
	return nil
}

// exists reports whether the root-relative path p exists (without following
// a final symlink).
func (d *Desk) exists(p string) bool {
	_, err := d.root.Lstat(p)
	return err == nil
}

// findKind returns the kind of an item present in sub, or ErrNotFound.
func (d *Desk) findKind(sub, name string) (Kind, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("desk: %q is not an item name (NN-name)", describe(name))
	}
	for _, k := range [...]Kind{KindScript, KindBatch} {
		if d.exists(path.Join(sub, name+k.Ext())) {
			return k, nil
		}
	}
	return "", ErrNotFound
}

// abs is the absolute path of a root-relative path, for handing to a child
// process. Children are started on paths below the pinned root.
func (d *Desk) abs(p string) string { return filepath.Join(d.path, filepath.FromSlash(p)) }

// LogPath is the absolute path of an item's done/<name>.log.
func (d *Desk) LogPath(name string) string { return d.abs(path.Join(DirDone, name+extLog)) }

// EventsPath is the absolute path of an item's done/<name>.events.
func (d *Desk) EventsPath(name string) string { return d.abs(path.Join(DirDone, name+extEvents)) }

// lastLine returns the final line of a log (without its newline), reading at
// most the last 4 KiB.
func (d *Desk) lastLine(p string) (string, error) {
	f, err := d.openRegular(p)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	const tail = 4096
	off := max(fi.Size()-tail, 0)
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	buf = bytes.TrimSuffix(buf, []byte("\n"))
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		buf = buf[i+1:]
	}
	return strings.TrimSuffix(string(buf), "\r"), nil
}
