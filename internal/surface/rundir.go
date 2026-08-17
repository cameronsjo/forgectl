package surface

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/privdir"
	"github.com/cameronsjo/forgectl/internal/termsafe"
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
const MaxSocketPathLen = 100

// runDirPrefix namespaces the private directory in a shared temp root, so an
// operator listing /tmp can tell what made it.
const runDirPrefix = "fc-"

// runDirEntropyBytes sizes the directory's random suffix. It is deliberately
// small: the directory is already unguessable enough at 32 bits given it lives
// for one launch and is mode 0700, and every byte here costs two characters of
// the socket-path budget.
const runDirEntropyBytes = 4

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

	return &RunDir{fd: fd, path: filepath.Join(base, leaf)}, nil
}

// SocketPath is where the bootstrap listener binds.
func (d *RunDir) SocketPath() string { return filepath.Join(d.path, socketName) }

// Path is the directory itself.
func (d *RunDir) Path() string { return d.path }

// Close releases the descriptor and removes the directory with everything in
// it. It is safe to call more than once, because cleanup runs on paths where
// proving it ran exactly once is harder than making it idempotent.
func (d *RunDir) Close() error {
	if d == nil {
		return nil
	}
	var firstErr error
	if d.fd >= 0 {
		if err := closeFD(d.fd); err != nil {
			firstErr = fmt.Errorf("%w: release descriptor: %w", ErrRunDir, err)
		}
		d.fd = -1
	}
	if d.path != "" {
		if err := os.RemoveAll(d.path); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%w: remove %s: %w", ErrRunDir,
				termsafe.QuotePath(d.path), termsafe.Error(err))
		}
		d.path = ""
	}
	return firstErr
}

// randomLeaf draws the directory's name.
func randomLeaf() (string, error) {
	buf := make([]byte, runDirEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("%w: draw directory name: %w", ErrRunDir, err)
	}
	return runDirPrefix + hex.EncodeToString(buf), nil
}
