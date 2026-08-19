// Package tmuxadapter drives a tmux server on behalf of the surface launcher.
//
// It is the first of #332's three adapters, and the shape of it is set by two
// constraints that pull in opposite directions.
//
// The first is #237: a tmux `-t` operand is not a name. tmux resolves it
// through native id, exact name, glob, then PREFIX, so `kill-session -t forge`
// happily kills `forge-review` when `forge` does not exist. Every action here
// therefore targets a native id ($N), and every id is bound to the server
// generation that minted it.
//
// The second is the sensitive-execution seam (#340). A StartSpec seals its cwd
// and its bootstrap command inside exec.Arg values whose payloads have no
// exported accessor — deliberately, because the bootstrap carries the socket
// path and the one-shot nonce that authenticate the handoff. Nothing outside
// internal/exec can read them back, so the create path cannot go through
// tmux.Client's plain-string API. It builds an argv of exec.Arg and hands it to
// a SensitiveRunner, which is the only code that reveals them.
//
// So the split is: CREATE goes through the sensitive runner, because only it
// can carry a sealed cwd and bootstrap. RECONCILE, PROBE, and CLOSE go through
// the same runner too, for one reason each — they are cheap, they keep every
// tmux argv in this package under one spelling of the socket pin, and the
// values they need (the ownership name, a native id) are plain. exec's
// CommandKind enum already names all four operations, which is the seam telling
// us this was the intended division.
package tmuxadapter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/tmux"
)

// identityFormat is the -F spec every command here asks for: the server
// incarnation, then the native id, then the session name.
//
// The name rides along because reconciliation matches on it — it is the only
// handle on a session whose create call failed to report one. Sharing one
// format across create, reconcile, and probe means the field count and order
// cannot drift between the command that mints an id and the command that finds
// it again.
const identityFormat = "#{pid}" + tmux.FieldSep + "#{start_time}" + tmux.FieldSep +
	"#{session_id}" + tmux.FieldSep + "#{session_name}"

// identityFieldCount is exact, not a minimum. A session NAME may legally
// contain the field separator, so a row carrying one splits into more fields
// than this and is dropped rather than misread — a shifted row would offer a
// well-formed-looking native id naming a different session.
const identityFieldCount = 4

// identityFormat travels as exec.Opaque rather than exec.MustFixed even though
// it is a compile-time constant, because it embeds FieldSep (0x1f) and the
// fixed-argument constructor refuses control characters outright — deliberately,
// so a mistyped constant cannot smuggle a terminal escape into an argv.
//
// That refusal is right and this is the sanctioned exception: the separator is
// the whole point of the format (a session name may contain spaces or tabs, so
// only an unprintable byte can delimit fields reliably). Opaque carries it
// without weakening the fixed-argument rule for everything else, and the only
// check it loses — the leading-dash refusal — cannot bite a string beginning
// with "#{".

// minMajor and minMinor are the floor at which #{pid} and #{start_time} exist.
// Below it tmux echoes the format string back instead of expanding it, which
// would arrive here as an unparseable row rather than an honest refusal.
const (
	minMajor = 2
	minMinor = 2
)

// Adapter drives one tmux server, pinned to one socket for its whole life.
type Adapter struct {
	run exec.SensitiveRunner

	// tmuxPath is absolute: the sensitive runner refuses a relative path so the
	// binary is chosen here rather than by a PATH lookup against the live
	// process environment.
	tmuxPath string

	// socket and source are the pinned endpoint and the CHAIN that chose it.
	// Only the source travels in a reference — a reference recording a pathname
	// could authorize a server the chain would no longer select.
	socket string
	source backend.ServerSource

	// lstat is seamed so incarnation fingerprinting is testable without a live
	// server.
	lstat func(string) (os.FileInfo, error)
}

// Option configures an Adapter at construction.
type Option func(*Adapter)

// WithLstat overrides the socket stat used for incarnation fingerprinting.
func WithLstat(fn func(string) (os.FileInfo, error)) Option {
	return func(a *Adapter) { a.lstat = fn }
}

// ErrResolveSocket reports that the tmux server socket could not be chosen.
var ErrResolveSocket = errors.New("tmuxadapter: cannot resolve the tmux server socket")

// New builds an adapter pinned to the tmux server the environment selects.
//
// The socket is resolved ONCE, here, and every later command carries it by
// explicit `-S`. That is what makes close and probe reach the same server the
// create reached: re-deriving it per call would let an operator's $TMUX
// changing mid-launch silently move the target, which is the failure the
// ServerSource type exists to describe.
//
// tmuxPath must be absolute. getenv and getuid are parameters rather than
// package-level reads so the resolution is a pure function of its inputs and
// the tests exercise the same code production does.
func New(run exec.SensitiveRunner, tmuxPath string, getenv func(string) string, getuid func() int, opts ...Option) (*Adapter, error) {
	if run == nil {
		return nil, fmt.Errorf("%w: no runner", ErrResolveSocket)
	}
	if !filepath.IsAbs(tmuxPath) {
		return nil, fmt.Errorf("%w: tmux path must be absolute", ErrResolveSocket)
	}
	socket, source, err := resolveSocket(getenv, getuid)
	if err != nil {
		return nil, err
	}
	a := &Adapter{
		run:      run,
		tmuxPath: tmuxPath,
		socket:   socket,
		source:   source,
		lstat:    os.Lstat,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a, nil
}

// resolveSocket picks the server endpoint and records which chain picked it.
//
// $TMUX wins when set, because it names the server the operator is actually
// looking at — putting the surface anywhere else would create a session they
// cannot see. Its value is `<socket>,<pid>,<session>`, so only the first field
// is the path.
func resolveSocket(getenv func(string) string, getuid func() int) (string, backend.ServerSource, error) {
	if inherited := getenv("TMUX"); inherited != "" {
		socket, _, _ := strings.Cut(inherited, ",")
		if socket == "" {
			return "", backend.ServerSource{}, fmt.Errorf("%w: $TMUX names no socket", ErrResolveSocket)
		}
		if err := checkSocketPath(socket); err != nil {
			return "", backend.ServerSource{}, err
		}
		return socket, backend.TmuxCurrentServer(), nil
	}
	root := getenv("TMUX_TMPDIR")
	if root == "" {
		root = "/tmp"
	}
	// The uid is os.Getuid, not Geteuid: tmux's own make_label() builds this
	// directory from the real uid, so matching it exactly is what makes the
	// derived path the same path tmux would choose.
	socket := filepath.Join(root, "tmux-"+strconv.Itoa(getuid()), "default")
	if err := checkSocketPath(socket); err != nil {
		return "", backend.ServerSource{}, err
	}
	return socket, backend.TmuxDefaultServer(), nil
}

// checkSocketPath applies the same shape rules tmux.NewPinned does, so a path
// this package resolves cannot be one the pinned client would refuse.
func checkSocketPath(socket string) error {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket {
		return fmt.Errorf("%w: socket path must be absolute and clean", ErrResolveSocket)
	}
	if len(socket) > tmux.MaxSocketPathLen {
		return fmt.Errorf("%w: socket path exceeds %d bytes", ErrResolveSocket, tmux.MaxSocketPathLen)
	}
	return nil
}

// Kind reports the backend this adapter drives.
func (a *Adapter) Kind() backend.Kind { return backend.KindTmux }

// pinned prefixes an argv with this adapter's socket pin. Every command in this
// package goes through it, so there is one spelling of `-S` and no call site
// can issue an unpinned command by forgetting it.
func (a *Adapter) pinned(args ...exec.Arg) []exec.Arg {
	return append([]exec.Arg{exec.MustFixed("-S"), exec.Opaque(a.socket)}, args...)
}

func (a *Adapter) command(kind exec.CommandKind, args ...exec.Arg) exec.SensitiveCommand {
	return exec.SensitiveCommand{
		Kind: kind,
		Path: exec.Secret(a.tmuxPath),
		Args: a.pinned(args...),
		// $TMUX is left alone deliberately: `-S` overrides socket selection
		// outright, and tmux's nesting refusal fires only on a command that
		// would ATTACH. Nothing here attaches — every command is `-d` or
		// read-only.
		Env: nil,
		// Both caps must be positive — the runner refuses a zero one rather
		// than substituting a default, so that a command's output bound is
		// always a decision someone made.
		StdoutCap: stdoutCap,
		StderrCap: stderrCap,
	}
}
