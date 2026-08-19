package surface

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/cameronsjo/forgectl/internal/privdir"
	"github.com/cameronsjo/forgectl/internal/termsafe"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// MaxSocketPathLen bounds the bootstrap socket's absolute path.
//
// A Unix socket address is a fixed-size struct: sun_path is 104 bytes on
// Darwin and 108 on Linux, and an over-long path fails at bind with
// "invalid argument" — a message that says nothing about length. One
// conservative bound for both platforms keeps the refusal here, where it can
// explain itself, and keeps a single number rather than a per-platform pair
// that could drift.
//
// It is exported because the pre-start bootstrap classifier validates incoming
// socket paths against the same bound (forgectl#343). Producer and validator
// disagreeing would mean forgectl emitting a bootstrap its own re-entry
// refuses — the kind of defect that only shows up on the longest temp path
// somebody happens to have.
//
// tmux.NewPinned validates against the same bound for the same reason, and the
// two are asserted equal at COMPILE time below rather than by a test, so drift
// cannot ship. The assertion lives here because the dependency runs this way:
// surface may import tmux, never the reverse.
const MaxSocketPathLen = 100

// Two independent literals of this bound are how a producer mints a socket its
// own consumer refuses. This makes that a build failure: if the constants ever
// diverge, one of these subtractions is negative and an untyped unsigned
// constant expression overflows.
const (
	_ uint = MaxSocketPathLen - tmux.MaxSocketPathLen
	_ uint = tmux.MaxSocketPathLen - MaxSocketPathLen
)

// runDirPrefix namespaces the private directory in a shared temp root, so an
// operator listing /tmp can tell what made it.
const runDirPrefix = "fc-"

// runDirEntropyBytes sizes the directory's random suffix. It is deliberately
// small: the directory is already unguessable enough at 32 bits given it lives
// for one launch and is mode 0700, and every byte here costs two characters of
// the socket-path budget.
const runDirEntropyBytes = 8

// socketName is the socket's filename, one character because the path budget
// is the scarce resource and nothing reads this name for meaning.
const socketName = "s"

var (
	// ErrRunDir reports a private run directory that could not be created or
	// could not be trusted.
	ErrRunDir = errors.New("surface: cannot create a private run directory")
	// ErrSocketPathTooLong reports a socket path that would not fit in a Unix
	// socket address.
	ErrSocketPathTooLong = errors.New("surface: socket path is too long to bind")
)

// RunDir is a private, single-launch directory holding the bootstrap socket.
//
// It is mode 0700 and owned by this user, proven through privdir before the
// path is handed to anything. That proof is what makes binding by path safe
// here: there is no bindat, so the socket is still created by name — but an
// attacker cannot place anything in a directory they cannot enter, and the
// directory's identity was established by descriptor rather than by re-reading
// the name.
type RunDir struct {
	// once guards Close. The doc on Close names the reason idempotency is
	// wanted — cleanup runs on paths where proving it ran exactly once is
	// harder than making a second call harmless — and that same shape is what
	// makes two *concurrent* calls plausible: a defer racing an error-path
	// cleanup. Without this, both would read a live fd before either cleared
	// it, and the second unix.Close would land on whatever number the runtime
	// had since handed out, which in a process about to open a socket is not
	// hypothetical.
	once     sync.Once
	closeErr error

	// open distinguishes "holds a descriptor" from the zero value. A bare
	// `fd >= 0` test would treat the zero value's fd 0 as live and close
	// stdin, and RunDir is exported, so `var d RunDir; defer d.Close()` is a
	// shape a caller can write.
	open bool
	fd   int
	path string
}

// NewRunDir creates a fresh private directory under base, or under the
// system temp directory when base is empty.
//
// The socket path is length-checked *before* anything is created, so an
// operator with a long TMPDIR gets an explanation rather than a directory
// nobody can bind in.
func NewRunDir(base string) (*RunDir, error) {
	if base == "" {
		base = os.TempDir()
	}
	if !filepath.IsAbs(base) {
		return nil, fmt.Errorf("%w: base %s is not absolute", ErrRunDir, termsafe.QuotePath(base))
	}
	base = filepath.Clean(base)

	if err := checkBase(base); err != nil {
		return nil, err
	}

	leaf, err := randomLeaf()
	if err != nil {
		return nil, err
	}

	// Checked before creation: a refusal must not leave a directory behind.
	socket := filepath.Join(base, leaf, socketName)
	if len(socket) > MaxSocketPathLen {
		return nil, fmt.Errorf("%w: %d bytes exceeds the %d-byte limit; "+
			"set TMPDIR to a shorter path (socket would be %s)",
			ErrSocketPathTooLong, len(socket), MaxSocketPathLen, termsafe.QuotePath(socket))
	}
	// The bootstrap classifier refuses a socket path carrying a control
	// character, so producing one here would mean emitting a bootstrap
	// forgectl's own re-entry rejects. TMPDIR is operator-supplied and can
	// carry anything.
	for i, r := range socket {
		if termsafe.IsUnsafeTerminalRune(r) {
			return nil, fmt.Errorf("%w: socket path carries a control character at byte %d; "+
				"set TMPDIR to a path without one", ErrRunDir, i)
		}
	}

	fd, err := privdir.Pin(privdir.Spec{
		Base:         base,
		Leaf:         leaf,
		Mode:         0o700,
		AncestorMode: 0o755,
		Create:       true,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRunDir, err)
	}

	return &RunDir{open: true, fd: fd, path: filepath.Join(base, leaf)}, nil
}

// SocketPath is where the bootstrap listener binds. It is empty after Close,
// rather than a bare "s" — a relative path handed onward would be refused far
// from here, by the bootstrap classifier's absoluteness check, with a message
// that says nothing about a use-after-close.
func (d *RunDir) SocketPath() string {
	if d == nil || d.path == "" {
		return ""
	}
	return filepath.Join(d.path, socketName)
}

// Path is the directory itself, empty after Close.
func (d *RunDir) Path() string {
	if d == nil {
		return ""
	}
	return d.path
}

// Close releases the descriptor and removes the directory with everything in
// it.
//
// It is safe to call more than once, and from more than one goroutine, because
// cleanup runs on paths where proving it ran exactly once is harder than
// making a second call harmless. Repeat calls return the first call's result.
//
// Removing by path is safe against a symlink swapped in after the descriptor
// closes: os.RemoveAll unlinks a symlink rather than following it, and its
// recursion opens directories with O_NOFOLLOW. That is a property of the
// standard library rather than a documented API guarantee, so it is worth
// re-checking on a Go upgrade.
func (d *RunDir) Close() error {
	if d == nil {
		return nil
	}
	d.once.Do(func() {
		if d.open {
			if err := closeFD(d.fd); err != nil {
				d.closeErr = fmt.Errorf("%w: release descriptor: %w", ErrRunDir, err)
			}
			d.open = false
		}
		if d.path != "" {
			if err := os.RemoveAll(d.path); err != nil && d.closeErr == nil {
				d.closeErr = fmt.Errorf("%w: remove %s: %w", ErrRunDir,
					termsafe.QuotePath(d.path), termsafe.Error(err))
			}
			d.path = ""
		}
	})
	return d.closeErr
}

// checkBase refuses a base another user could rename entries in.
//
// This is the check the whole directory guarantee silently rested on. privdir
// proves the *leaf* is ours, but hands back a path — there is no bindat, so the
// socket is created by name — and the window between that proof and the bind is
// closed only by the base being one an attacker cannot manipulate. On macOS
// that is free, because os.TempDir() is a per-user 0700 directory. On Linux
// /tmp is 1777, and it is the **sticky bit**, not the permissions, that stops
// another user renaming our entry out from under us.
//
// Strip the sticky bit from a world-writable base — a TMPDIR pointed at a
// shared directory, or a container image that ran `chmod 777 /tmp` without
// restoring it — and an attacker can rename our proven leaf away and drop a
// symlink at the same name before the bind lands, putting the socket somewhere
// they control at whatever the umask allows.
//
// privdir deliberately does not check its base: its other consumer's base is a
// user-owned state directory, and the "trusted for placement only" contract is
// right there. The consumer whose base is /tmp is the one that owes this.
func checkBase(base string) error {
	info, err := os.Stat(base)
	if err != nil {
		// A missing base is privdir's business — it creates one. Anything else
		// is reported there too, with better context.
		return nil //nolint:nilerr // absence and stat errors are privdir's to classify
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: base %s is not a directory", ErrRunDir, termsafe.QuotePath(base))
	}
	if info.Mode().Perm()&0o002 != 0 && info.Mode()&os.ModeSticky == 0 {
		return fmt.Errorf("%w: base %s is writable by other users and not sticky, so another "+
			"user could replace the run directory between its creation and the socket bind",
			ErrRunDir, termsafe.QuotePath(base))
	}
	return nil
}

// randomLeaf draws the directory's name.
func randomLeaf() (string, error) {
	buf := make([]byte, runDirEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("%w: draw directory name: %w", ErrRunDir, err)
	}
	return runDirPrefix + hex.EncodeToString(buf), nil
}
