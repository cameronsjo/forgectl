package env

import (
	"io/fs"
	"testing"
	"time"
)

// modeInfo is an fs.FileInfo that reports only a mode, for the Lstat shapes
// Windows produces and a unix test cannot create.
type modeInfo fs.FileMode

func (m modeInfo) Name() string      { return "scratch" }
func (modeInfo) Size() int64         { return 0 }
func (m modeInfo) Mode() fs.FileMode { return fs.FileMode(m) }
func (modeInfo) ModTime() time.Time  { return time.Time{} }
func (m modeInfo) IsDir() bool       { return fs.FileMode(m).IsDir() }
func (modeInfo) Sys() any            { return nil }

// TestScratchDirIntact_AcceptsAReparseDirectoryAndRefusesLinks is #810: the
// swapped-symlink guard compared the type to exactly ModeDir, so a Windows
// cloud placeholder directory (ModeDir|ModeIrregular under Go 1.23+) was
// refused and left without its .gitignore. Symlinks and junctions (which
// Lstat reports without ModeDir) must still be refused.
//
// Mutation: restore info.Mode().Type() == fs.ModeDir and the reparse row
// fails; return true and the link rows do.
func TestScratchDirIntact_AcceptsAReparseDirectoryAndRefusesLinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode fs.FileMode
		want bool
	}{
		{"plain directory", fs.ModeDir | 0o700, true},
		{"reparse-point directory", fs.ModeDir | fs.ModeIrregular | 0o700, true},
		{"symlink", fs.ModeSymlink | 0o777, false},
		{"junction", fs.ModeIrregular | 0o666, false},
		{"regular file", 0o600, false},
	} {
		if got := scratchDirIntact(modeInfo(tc.mode)); got != tc.want {
			t.Errorf("%s (%v): scratchDirIntact = %v, want %v", tc.name, tc.mode, got, tc.want)
		}
	}
}
