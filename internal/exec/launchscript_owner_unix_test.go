//go:build unix

package exec

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// fakeInfo is an os.FileInfo whose Sys() carries a chosen owner.
type fakeInfo struct{ uid uint32 }

func (fakeInfo) Name() string       { return "d" }
func (fakeInfo) Size() int64        { return 0 }
func (fakeInfo) Mode() os.FileMode  { return os.ModeDir | 0o700 }
func (fakeInfo) ModTime() time.Time { return time.Time{} }
func (fakeInfo) IsDir() bool        { return true }
func (f fakeInfo) Sys() any         { return &syscall.Stat_t{Uid: f.uid} }

// TestOwnedByCurrentUser: the directory check accepts this user's own
// directory and refuses one owned by anyone else, and a FileInfo with no
// Stat_t.
func TestOwnedByCurrentUser(t *testing.T) {
	me := uint32(os.Geteuid()) //nolint:gosec // G115: a uid is a small non-negative number
	if !ownedByCurrentUser(fakeInfo{uid: me}) {
		t.Error("refused a directory owned by the effective user")
	}
	if ownedByCurrentUser(fakeInfo{uid: me + 1}) {
		t.Error("accepted a directory owned by another user")
	}
	if ownedByCurrentUser(noSys{}) {
		t.Error("accepted a FileInfo with no owner information")
	}
	dir := t.TempDir()
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ownedByCurrentUser(info) {
		t.Error("refused a real directory this process created")
	}
}

type noSys struct{ fakeInfo }

func (noSys) Sys() any { return nil }
