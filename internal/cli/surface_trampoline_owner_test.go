//go:build unix

package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// The socket-owner check's three properties, one test each.
//
// Two of them cannot be provoked honestly: planting a foreign-owned socket
// needs a second account, and producing a FileInfo whose Sys() is not a
// *syscall.Stat_t needs a filesystem this machine does not have. Without a seam
// the suite is indifferent to both — the check could be deleted outright and
// everything would stay green — so lstatFn and selfEUID are driven from here.

// fakeInfo is a FileInfo with a caller-chosen mode and Sys().
type fakeInfo struct {
	mode fs.FileMode
	sys  any
}

func (f fakeInfo) Name() string       { return "s" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() (t timeT) { return }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return f.sys }

// TestCheckSocketOwner_RefusesAForeignOwner is the property the check exists
// for, and nothing else in the package asserts it.
func TestCheckSocketOwner_RefusesAForeignOwner(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "s")

	// The control first: a socket this user owns is accepted. Without it the
	// refusal below is consistent with a check that refuses everything.
	realSocket := newUnixSocketFile(t)
	if err := checkSocketOwner(realSocket); err != nil {
		t.Fatalf("checkSocketOwner refused a socket this user owns: %v", err)
	}

	original := lstatFn
	t.Cleanup(func() { lstatFn = original })
	lstatFn = func(string) (os.FileInfo, error) {
		return fakeInfo{mode: os.ModeSocket, sys: statWithUID(os.Geteuid() + 1)}, nil
	}

	if err := checkSocketOwner(socket); !errors.Is(err, errSocketUnsafe) {
		t.Errorf("a socket owned by another account = %v, want errSocketUnsafe", err)
	}
}

// TestCheckSocketOwner_ComparesAgainstThisProcess pins the other operand. A
// check that read the owner correctly and compared it against a constant would
// pass the test above and still admit the wrong account.
func TestCheckSocketOwner_ComparesAgainstThisProcess(t *testing.T) {
	socket := newUnixSocketFile(t)

	original := selfEUID
	t.Cleanup(func() { selfEUID = original })
	selfEUID = func() int { return os.Geteuid() + 1 }

	if err := checkSocketOwner(socket); !errors.Is(err, errSocketUnsafe) {
		t.Errorf("checkSocketOwner with a shifted self-uid = %v, want errSocketUnsafe", err)
	}
}

// TestCheckSocketOwner_RefusesUnreadableOwnership is the fail-closed case: a
// filesystem that cannot report ownership has not established that the socket
// is ours.
func TestCheckSocketOwner_RefusesUnreadableOwnership(t *testing.T) {
	original := lstatFn
	t.Cleanup(func() { lstatFn = original })
	lstatFn = func(string) (os.FileInfo, error) {
		return fakeInfo{mode: os.ModeSocket, sys: nil}, nil
	}

	if err := checkSocketOwner("/anywhere"); !errors.Is(err, errSocketUnsafe) {
		t.Errorf("unreadable ownership = %v, want errSocketUnsafe", err)
	}
}

// TestCheckSocketOwner_DoesNotFollowSymlinks pins Lstat rather than Stat.
//
// Following the link would authenticate the *target's* ownership while the
// connection goes through a link someone else placed. The fixture is a symlink
// to a socket this user owns — so the target passes every check, and only the
// refusal to dereference distinguishes the two implementations.
func TestCheckSocketOwner_DoesNotFollowSymlinks(t *testing.T) {
	target := newUnixSocketFile(t)

	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	// The target itself is fine, which is what makes this test about the
	// symlink and not about the socket.
	if err := checkSocketOwner(target); err != nil {
		t.Fatalf("the symlink's target was itself refused: %v", err)
	}

	if err := checkSocketOwner(link); !errors.Is(err, errSocketUnsafe) {
		t.Errorf("a symlink to a valid socket = %v, want errSocketUnsafe "+
			"(the check must not dereference)", err)
	}
}
