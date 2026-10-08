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
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// pathError renders a failed open of p. ELOOP (EMLINK on FreeBSD) is what
// O_NOFOLLOW returns for a symlink: a refusal, not a missing file.
func pathError(p string, err error) error {
	switch {
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.EMLINK):
		return fmt.Errorf("%s: %w: a symlink", clean(p), ErrRefused)
	case errors.Is(err, unix.ENOENT):
		return fmt.Errorf("%s: %w", clean(p), fs.ErrNotExist)
	}
	return fmt.Errorf("%s: %w", clean(p), err)
}

// openLog opens the regular file at p. The directory holding it is the
// person's own choice and is followed as given; the file itself is opened
// with O_NOFOLLOW (a symlink is refused) and O_NONBLOCK (a FIFO cannot hang
// the open), then fstat'd so only a regular file is read.
func openLog(p string) (*os.File, unix.Stat_t, error) {
	var st unix.Stat_t
	dir, err := unix.Open(filepath.Dir(p), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, st, pathError(filepath.Dir(p), err)
	}
	fd, err := unix.Openat(dir, filepath.Base(p), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	_ = unix.Close(dir)
	if err != nil {
		return nil, st, pathError(p, err)
	}
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, st, pathError(p, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, st, fmt.Errorf("%s: %w: not a regular file", clean(p), ErrRefused)
	}
	return os.NewFile(uintptr(fd), p), st, nil //nolint:gosec // G115: fd >= 0 here
}

func mtimeOf(st *unix.Stat_t) time.Time {
	sec, nsec := st.Mtim.Unix()
	return time.Unix(sec, nsec).UTC()
}

func devIno(st *unix.Stat_t) (uint64, uint64) {
	return uint64(st.Dev), uint64(st.Ino) //nolint:gosec,unconvert // G115: Dev is int32 on darwin; only compared for equality
}

// replaced reports whether st is not the file cur last read: a different
// device or inode, or a size below the cursor's offset (truncated in place).
func replaced(st *unix.Stat_t, cur *Cursor) bool {
	dev, ino := devIno(st)
	return cur.opened && (dev != cur.Dev || ino != cur.Ino || st.Size < cur.Offset)
}

// readLines reads what was added to f since cur and calls line for each
// complete line, in order, with Seq set to its line number from 1. Reading
// stops at maxFileBytes from the start of the file; capped reports that the
// file is larger. A line longer than maxLineBytes is dropped and counted,
// including one still waiting for its newline. The start of a last line with
// no newline yet is held in cur until the next read completes it.
func readLines(f *os.File, st *unix.Stat_t, cur *Cursor, line func(seq int, b []byte)) (capped bool, err error) {
	cur.Dev, cur.Ino = devIno(st)
	cur.opened = true
	end := min(st.Size, maxFileBytes)
	capped = st.Size > maxFileBytes
	if end <= cur.Offset {
		return capped, nil
	}
	buf := make([]byte, end-cur.Offset)
	n, err := f.ReadAt(buf, cur.Offset)
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
