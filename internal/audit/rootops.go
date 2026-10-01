package audit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// This file (with its per-platform dirOpenFlags siblings) is the audit
// package's entire filesystem surface, and the only place
// TestAuditSource_NoUnconfinedFilesystemCalls allows a stat, listing or open.
// Everything below the root goes through one *os.Root, so no listing or stat
// resolves outside it, even under a racing symlink swap.

// fsOps is the scanner's whole filesystem surface. ScanInjection binds it to
// an *os.Root; a test binds a failing or counting double to prove no
// metadata arrives any other way.
type fsOps struct {
	// names lists at most limit of a directory's entry names, with no
	// per-entry stat. A directory with more than limit entries returns
	// limit of them, in the order the OS lists them, and is never read
	// whole (#994).
	names func(dir string, limit int) ([]string, error)
	// lstat describes an entry without following a final symlink.
	lstat func(name string) (fs.FileInfo, error)
	// stat follows a symlink, but only within the root; a target outside it
	// is an error.
	stat func(name string) (fs.FileInfo, error)
	// sniff reports whether the first sniffBytes of a regular file hold
	// privateKeyMarker. It is the package's only content read: the bytes are
	// cleared before it returns and never leave it, so a caller learns one
	// boolean and nothing of what the file says.
	sniff func(name string) (bool, error)
}

// sniffBytes caps the one content read the package makes.
const sniffBytes = 4096

// privateKeyMarker ends every PEM private-key BEGIN line: "-----BEGIN
// PRIVATE KEY-----", "-----BEGIN RSA PRIVATE KEY-----", "-----BEGIN
// OPENSSH PRIVATE KEY-----", "-----BEGIN ENCRYPTED PRIVATE KEY-----".
var privateKeyMarker = []byte("PRIVATE KEY-----")

// errNotRegular refuses a sniff of anything the open did not find to be a
// regular file: a FIFO or device swapped in after the Lstat.
var errNotRegular = errors.New("not a regular file")

// currentEUID is the effective uid foreign-owner compares against, or -1
// where the platform has none.
func currentEUID() int { return os.Geteuid() }

// openRootOps opens the scan root and returns its fsOps plus the closer the
// caller defers. The root path itself is the caller's and is followed as the
// OS follows any path; nothing below it escapes the Root.
func openRootOps(abs string) (fsOps, func(), error) {
	r, err := os.OpenRoot(abs)
	if err != nil {
		return fsOps{}, nil, fmt.Errorf("open audit root %s: %w", termsafe.QuotePath(abs), termsafe.Error(err))
	}
	return rootOps(r), func() { _ = r.Close() }, nil
}

func rootOps(r *os.Root) fsOps {
	return fsOps{
		names: func(dir string, limit int) ([]string, error) {
			// dirOpenFlags carries O_DIRECTORY|O_NONBLOCK where the platform has
			// them: a directory swapped for a FIFO between the parent's lstat
			// and this open fails fast instead of blocking for a writer.
			f, err := r.OpenFile(dir, dirOpenFlags, 0)
			if err != nil {
				return nil, err
			}
			defer func() { _ = f.Close() }()
			return readNames(f, limit)
		},
		lstat: r.Lstat,
		stat:  r.Stat,
		sniff: func(name string) (bool, error) { return sniffRoot(r, name) },
	}
}

// dirReadBatch is the most names one Readdirnames call asks for. readNames
// never asks for more than its limit has left, so it holds no name past the
// limit.
const dirReadBatch = 256

// readNames reads at most limit names from the directory f, in batches of at
// most dirReadBatch, and stops at the limit or the end of the directory.
func readNames(f *os.File, limit int) ([]string, error) {
	var out []string
	for len(out) < limit {
		batch, err := f.Readdirnames(min(dirReadBatch, limit-len(out)))
		out = append(out, batch...)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// sniffRoot is fsOps.sniff over r. The open carries O_NONBLOCK where the
// platform has it, so a FIFO swapped in after the Lstat cannot block it, and
// the fstat after the open refuses anything that is not a regular file. The
// root confines the open: a symlink swapped in resolves only inside the
// root, and the fstat then judges what was actually opened.
func sniffRoot(r *os.Root, name string) (bool, error) {
	f, err := r.OpenFile(name, fileOpenFlags, 0)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errNotRegular
	}
	buf := make([]byte, sniffBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		clear(buf)
		return false, err
	}
	found := bytes.Contains(buf[:n], privateKeyMarker)
	clear(buf)
	return found, nil
}
