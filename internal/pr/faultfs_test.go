package pr

import (
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"
)

// faultFS is the recordFS test double: it delegates to the real filesystem,
// logs every call by method name, and fails exactly one named step. "Write"
// and "Sync" are failed on the file handle it hands back.
type faultFS struct {
	t    *testing.T
	fail string
	// at is WHICH occurrence of the named step fails: 0 means every one, N
	// means only the Nth. Failing one occurrence is what stages a crash
	// BETWEEN two writes — a rename that lands, a window that gets created,
	// and then a rename that does not.
	at    int
	seen  int
	calls []string
	real  osRecordFS
}

func newFaultFS(t *testing.T, fail string) *faultFS {
	t.Helper()
	return &faultFS{t: t, fail: fail}
}

// newFaultFSAt fails only the at-th occurrence of the named step (1-based).
func newFaultFSAt(t *testing.T, fail string, at int) *faultFS {
	t.Helper()
	return &faultFS{t: t, fail: fail, at: at}
}

// shouldFail reports whether this occurrence of name is the one to fail.
func (f *faultFS) shouldFail(name string) bool {
	if f.fail != name {
		return false
	}
	f.seen++
	return f.at == 0 || f.seen == f.at
}

var errInjected = errors.New("injected fault")

func (f *faultFS) note(name string) { f.calls = append(f.calls, name) }

func (f *faultFS) called(name string) bool {
	for _, c := range f.calls {
		if c == name {
			return true
		}
	}
	return false
}

func (f *faultFS) OpenExclusive(path string) (recordFile, error) {
	f.note("OpenExclusive")
	if f.shouldFail("OpenExclusive") {
		return nil, errInjected
	}
	file, err := f.real.OpenExclusive(path)
	if err != nil {
		return nil, err
	}
	return &faultFile{recordFile: file, fs: f}, nil
}

func (f *faultFS) Rename(oldpath, newpath string) error {
	f.note("Rename")
	if f.shouldFail("Rename") {
		return errInjected
	}
	return f.real.Rename(oldpath, newpath)
}

func (f *faultFS) SyncDir(path string) error {
	f.note("SyncDir")
	if f.shouldFail("SyncDir") {
		return errInjected
	}
	return f.real.SyncDir(path)
}

func (f *faultFS) Remove(path string) error {
	f.note("Remove")
	return f.real.Remove(path)
}

func (f *faultFS) ReadFile(path string) ([]byte, error) {
	f.note("ReadFile")
	return f.real.ReadFile(path)
}

func (f *faultFS) Lstat(path string) (fs.FileInfo, error) {
	f.note("Lstat")
	return f.real.Lstat(path)
}

type faultFile struct {
	recordFile
	fs *faultFS
}

func (ff *faultFile) Write(p []byte) (int, error) {
	ff.fs.note("Write")
	if ff.fs.shouldFail("Write") {
		// A short write, not an error: the writer must treat it as failure.
		n, err := ff.recordFile.Write(p[:len(p)/2])
		if err != nil {
			return n, err
		}
		return n, nil
	}
	return ff.recordFile.Write(p)
}

func (ff *faultFile) Sync() error {
	ff.fs.note("Sync")
	if ff.fs.shouldFail("Sync") {
		return errInjected
	}
	return ff.recordFile.Sync()
}

func fixedTime() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }

var _ = os.ErrNotExist
