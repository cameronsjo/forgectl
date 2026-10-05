// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

//go:build unix

package runview

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// runRoot is an open run root. Every path below it is opened one component
// at a time from fd, so nothing below the root is ever resolved by name.
type runRoot struct {
	root *os.Root
	dir  *os.File // root's "." held open; fd is its descriptor
	fd   int
}

// openRunRoot opens the root named by the spec's root variable, which must
// be set and absolute. The root itself is trusted operator config, so a
// symlink in its own path is followed; nothing below it is.
func openRunRoot(env string) (*runRoot, error) {
	p := os.Getenv(env)
	switch {
	case p == "":
		return nil, fmt.Errorf("%w: $%s is unset", ErrRoot, clean(env))
	case !strings.HasPrefix(p, "/"):
		return nil, fmt.Errorf("%w: $%s must be an absolute path", ErrRoot, clean(env))
	}
	root, err := os.OpenRoot(p)
	if err != nil {
		return nil, fmt.Errorf("%w: $%s: %w", ErrRoot, clean(env), err)
	}
	dir, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("%w: $%s: %w", ErrRoot, clean(env), err)
	}
	return &runRoot{root: root, dir: dir, fd: int(dir.Fd())}, nil //nolint:gosec // G115: a descriptor fits in int
}

func (r *runRoot) Close() error {
	return errors.Join(r.dir.Close(), r.root.Close())
}

// pathError renders a failed open of rel. ELOOP (EMLINK on FreeBSD) is what
// O_NOFOLLOW returns for a symlink, and ENOTDIR what O_DIRECTORY returns for
// a non-directory: both are refusals, not missing files.
func pathError(rel string, err error) error {
	switch {
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.EMLINK):
		return fmt.Errorf("%s: %w: a symlink", clean(rel), ErrRefused)
	case errors.Is(err, unix.ENOTDIR):
		return fmt.Errorf("%s: %w: not a directory", clean(rel), ErrRefused)
	case errors.Is(err, unix.ENOENT):
		return fmt.Errorf("%s: %w", clean(rel), fs.ErrNotExist)
	}
	return fmt.Errorf("%s: %w", clean(rel), err)
}

// split turns a relative slash path into its components, dropping empty
// and "." parts. LoadSpec already refused "..", absolute paths and '\'
// roots; a ".." here is refused again rather than trusted.
func split(rel string) ([]string, error) {
	var out []string
	for _, p := range strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' }) {
		switch p {
		case ".":
			continue
		case "..":
			return nil, fmt.Errorf("%s: %w: '..'", clean(rel), ErrRefused)
		}
		out = append(out, p)
	}
	return out, nil
}

// openDir opens the directory at parts below dirfd. Each component is opened
// with O_NOFOLLOW and O_DIRECTORY, so a symlink or a non-directory anywhere
// on the path is refused by the kernel at the moment it is opened, with no
// window between a check and the open. The caller closes the result.
func openDir(dirfd int, parts []string, rel string) (int, error) {
	fd, err := unix.Dup(dirfd)
	if err != nil {
		return -1, err
	}
	unix.CloseOnExec(fd)
	for _, p := range parts {
		next, err := unix.Openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if err != nil {
			return -1, pathError(rel, err)
		}
		fd = next
	}
	return fd, nil
}

// openFile opens the regular file at rel below dirfd: every directory on the
// way through openDir, the leaf with O_NOFOLLOW (a symlink is refused) and
// O_NONBLOCK (a FIFO cannot hang the open), then fstat'd so only a regular
// file is read.
func openFile(dirfd int, rel string) (*os.File, unix.Stat_t, error) {
	var st unix.Stat_t
	parts, err := split(rel)
	if err != nil {
		return nil, st, err
	}
	if len(parts) == 0 {
		return nil, st, fmt.Errorf("%s: %w: empty path", clean(rel), ErrRefused)
	}
	dir, err := openDir(dirfd, parts[:len(parts)-1], rel)
	if err != nil {
		return nil, st, err
	}
	fd, err := unix.Openat(dir, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	_ = unix.Close(dir)
	if err != nil {
		return nil, st, pathError(rel, err)
	}
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, st, pathError(rel, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, st, fmt.Errorf("%s: %w: not a regular file", clean(rel), ErrRefused)
	}
	return os.NewFile(uintptr(fd), rel), st, nil //nolint:gosec // G115: fd >= 0 here
}

// statDir reports whether rel below dirfd is a real directory, opened the
// same way openFile walks, and its mtime.
func statDir(dirfd int, rel string) (time.Time, error) {
	parts, err := split(rel)
	if err != nil {
		return time.Time{}, err
	}
	fd, err := openDir(dirfd, parts, rel)
	if err != nil {
		return time.Time{}, err
	}
	defer unix.Close(fd) //nolint:errcheck // read-only
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return time.Time{}, pathError(rel, err)
	}
	return mtimeOf(&st), nil
}

func mtimeOf(st *unix.Stat_t) time.Time {
	sec, nsec := st.Mtim.Unix()
	return time.Unix(sec, nsec).UTC()
}

func devIno(st *unix.Stat_t) (uint64, uint64) {
	return uint64(st.Dev), uint64(st.Ino) //nolint:gosec,unconvert // G115: Dev is int32 on darwin; only compared for equality
}

// tracked is one open file of a run and whether it replaced what the cursor
// last read: a different device or inode, or a size below the cursor's
// offset (truncated in place).
type tracked struct {
	f        *os.File
	st       unix.Stat_t
	replaced bool
	err      error
}

func track(dirfd int, rel string, cur *Cursor) tracked {
	f, st, err := openFile(dirfd, rel)
	if err != nil {
		return tracked{err: err}
	}
	dev, ino := devIno(&st)
	replaced := cur.opened && (dev != cur.Dev || ino != cur.Ino || st.Size < cur.Offset)
	return tracked{f: f, st: st, replaced: replaced}
}

func (t tracked) close() {
	if t.f != nil {
		_ = t.f.Close()
	}
}

// readLines reads what was added to t's file since cur and calls line for
// each complete line, in order, with Seq set to its line number from 1.
// Reading stops at maxFileBytes from the start of the file; capped reports
// that the file is larger. A line longer than maxLineBytes is dropped and
// counted, including one still waiting for its newline. The start of a last
// line with no newline yet is held in cur until the next read completes it.
func readLines(t tracked, cur *Cursor, line func(seq int, b []byte)) (capped bool, err error) {
	dev, ino := devIno(&t.st)
	cur.Dev, cur.Ino, cur.opened = dev, ino, true
	end := min(t.st.Size, maxFileBytes)
	capped = t.st.Size > maxFileBytes
	if end <= cur.Offset {
		return capped, nil
	}
	buf := make([]byte, end-cur.Offset)
	n, err := t.f.ReadAt(buf, cur.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return capped, err
	}
	cur.Offset += int64(n)
	for data := buf[:n]; len(data) > 0; {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			hold(cur, data)
			break
		}
		seg := data[:i]
		data = data[i+1:]
		cur.lines++
		if cur.dropping || len(cur.held)+len(seg) > maxLineBytes {
			cur.held, cur.dropping = nil, false
			cur.dropped++
			continue
		}
		if len(cur.held) > 0 {
			seg = append(cur.held, seg...)
			cur.held = nil
		}
		line(cur.lines, seg)
	}
	return capped, nil
}

// hold keeps the start of a line whose newline has not arrived, up to
// maxLineBytes; past that the line is dropped when its newline comes.
func hold(cur *Cursor, data []byte) {
	if cur.dropping {
		return
	}
	if len(cur.held)+len(data) > maxLineBytes {
		cur.held, cur.dropping = nil, true
		return
	}
	cur.held = append(cur.held, data...)
}
