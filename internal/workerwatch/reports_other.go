//go:build !unix

package workerwatch

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// errBriefDirSymlink reports a brief dir that is itself a symlink.
var errBriefDirSymlink = errors.New("the brief dir is a symlink")

// openBriefDir opens the brief dir. Without openat, later reads resolve the
// path again; the caller checks each handle is a regular file.
func openBriefDir(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errBriefDirSymlink
	}
	return os.Open(path) //nolint:gosec // G304: the brief dir the workers file names
}

// openReportAt opens a file in the brief dir for reading. The caller checks
// the handle is a regular file.
func openReportAt(dir *os.File, name string) (*os.File, error) {
	return os.Open(filepath.Join(dir.Name(), name)) //nolint:gosec // G304: a name reportNameRe matched, inside the brief dir
}

// statAt stats name in the brief dir by path, without following a symlink.
func statAt(dir *os.File, name string) (fs.FileInfo, error) {
	return os.Lstat(filepath.Join(dir.Name(), name))
}
