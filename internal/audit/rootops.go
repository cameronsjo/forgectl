package audit

import (
	"fmt"
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
	// names lists a directory's entry names, with no per-entry stat.
	names func(dir string) ([]string, error)
	// lstat describes an entry without following a final symlink.
	lstat func(name string) (fs.FileInfo, error)
	// stat follows a symlink, but only within the root; a target outside it
	// is an error.
	stat func(name string) (fs.FileInfo, error)
}

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
		names: func(dir string) ([]string, error) {
			// dirOpenFlags carries O_DIRECTORY|O_NONBLOCK where the platform has
			// them: a directory swapped for a FIFO between the parent's lstat
			// and this open fails fast instead of blocking for a writer.
			f, err := r.OpenFile(dir, dirOpenFlags, 0)
			if err != nil {
				return nil, err
			}
			defer func() { _ = f.Close() }()
			return f.Readdirnames(-1)
		},
		lstat: r.Lstat,
		stat:  r.Stat,
	}
}
