// Package cmuxadapter drives a cmux server on behalf of the surface launcher.
//
// It is the second of #332's three adapters, and where the tmux adapter's shape
// was set by tmux's target grammar, this one's is set by three properties of the
// cmux CLI that were measured rather than assumed (cmux 0.64.22, protocol 2).
//
// First, the pin is an ENVIRONMENT variable, not a flag. cmux selects its socket
// from CMUX_SOCKET_PATH, so every command here carries an exec.EnvMutation
// instead of tmux's `-S`. That is the whole reason this package cannot use a
// plain exec.Runner even for its read paths: only the sensitive runner accepts
// an EnvMutation, and a read issued without the pin would answer from whatever
// server the ambient environment selects — which is precisely the fallback the
// design forbids after an identity has been fingerprinted.
//
// Second, cmux ANSWERS with the endpoint it bound. `capabilities` reports its
// own socket_path, which gives this adapter a control tmux never offered: it can
// assert that the server replying is the one it pinned. A reply from a different
// endpoint means the pin did not take, and that is refused rather than reported.
//
// Third, and this is the trap: cmux's `--json` envelope for CREATE changes its
// key names with the global `--id-format` flag. Without it the reply carries
// "workspace_ref": "workspace:4"; with `--id-format uuids` it carries
// "workspace_id": "<UUID>". A parser keyed on workspace_id silently finds
// nothing when the flag is missing, and one that fell back to any ref-shaped key
// would bind a POSITIONAL INDEX that shifts as workspaces open and close. So
// every command carries the global flag, and the create reply is required to
// yield a valid UUID — an absent or non-UUID id is a malformed response that
// reconciles, never an id.
//
// Two more measured facts shape the error handling. cmux exits ZERO and creates
// the workspace anyway when --cwd names a directory that does not exist, so a
// clean exit is not evidence the surface is where it was asked to be. And the
// legacy verbs (new-workspace, list-workspaces, …) print a deprecation notice on
// STDOUT ahead of the payload; this package uses the canonical `workspace`
// subcommands and sets CMUX_QUIET anyway, because a parser that reads prose as
// its first row is one release away from breaking.
package cmuxadapter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// maxSocketPathLen bounds the endpoint path. A Unix socket path is capped by
// sun_path — 104 bytes on Darwin, 108 on Linux — and the smaller bound is used
// so a path that would be refused by the kernel is refused here instead, with a
// diagnostic that names the reason.
const maxSocketPathLen = 104

// Adapter drives one cmux server, pinned to one socket for its whole life.
type Adapter struct {
	run exec.SensitiveRunner

	// cmuxPath is absolute: the sensitive runner refuses a relative path so the
	// binary is chosen here rather than by a PATH lookup against the live
	// process environment.
	cmuxPath string

	// socket and source are the pinned endpoint and the CHAIN that chose it.
	// Only the source travels in a reference — a reference recording a pathname
	// could authorize a server the chain would no longer select.
	socket string
	source backend.ServerSource

	// lstat is seamed so incarnation fingerprinting and the socket-ownership
	// check are testable without a live cmux.
	lstat func(string) (os.FileInfo, error)

	// selfUID is seamed alongside it for the same reason the trampoline's is:
	// the ownership refusal cannot be provoked honestly from a unit test —
	// planting a foreign-owned socket needs a second account — so without a seam
	// the suite would be indifferent to whether the comparison exists at all.
	selfUID func() int
}

// Option configures an Adapter at construction.
type Option func(*Adapter)

// WithLstat overrides the socket stat used for fingerprinting and ownership.
func WithLstat(fn func(string) (os.FileInfo, error)) Option {
	return func(a *Adapter) { a.lstat = fn }
}

// WithSelfUID overrides the uid the socket-ownership check compares against.
func WithSelfUID(fn func() int) Option {
	return func(a *Adapter) { a.selfUID = fn }
}

// ErrResolveSocket reports that the cmux server socket could not be chosen.
var ErrResolveSocket = errors.New("cmuxadapter: cannot resolve the cmux server socket")

// New builds an adapter pinned to the cmux server the environment selects.
//
// The socket is resolved ONCE, here, and every later command carries it in
// CMUX_SOCKET_PATH. That is what makes close and probe reach the same server the
// create reached: re-deriving it per call would let the operator's environment
// changing mid-launch silently move the target, which is the failure the
// ServerSource type exists to describe.
//
// Only the path SHAPE is validated here. Whether anything is listening, and
// whether what is listening is ours, are live questions answered at readiness —
// where an absent socket is an honest "cmux is not running" rather than a
// construction error, and where the answer is fresh at the moment it is used.
// tmux's adapter checks its socket DIRECTORY at construction because it goes on
// to CREATE the server; this adapter only ever connects to one cmux already
// started, so there is nothing here to make safe in advance.
func New(run exec.SensitiveRunner, cmuxPath string, getenv func(string) string, opts ...Option) (*Adapter, error) {
	if run == nil {
		return nil, fmt.Errorf("%w: no runner", ErrResolveSocket)
	}
	if !filepath.IsAbs(cmuxPath) {
		return nil, fmt.Errorf("%w: cmux path must be absolute", ErrResolveSocket)
	}
	socket, source, err := resolveSocket(getenv)
	if err != nil {
		return nil, err
	}
	a := &Adapter{
		run:      run,
		cmuxPath: cmuxPath,
		socket:   socket,
		source:   source,
		lstat:    os.Lstat,
		selfUID:  os.Geteuid,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a, nil
}

// resolveSocket picks the server endpoint and records which chain picked it.
//
// CMUX_SOCKET_PATH wins when set, because it names the server the operator has
// deliberately pointed their CLI at; putting the surface anywhere else would
// create a workspace they are not looking at. Otherwise the default is derived
// the same way cmux derives it, from the XDG state directory — verified against
// a live server, whose capabilities reply names exactly this path.
//
// The derivation is a pure function of getenv rather than a call to
// os.UserConfigDir so that the resolution the tests exercise is the resolution
// production runs.
func resolveSocket(getenv func(string) string) (string, backend.ServerSource, error) {
	if pinned := getenv("CMUX_SOCKET_PATH"); pinned != "" {
		if err := checkSocketPath(pinned); err != nil {
			return "", backend.ServerSource{}, err
		}
		return pinned, backend.CmuxEnvServer(), nil
	}
	state := getenv("XDG_STATE_HOME")
	if state == "" {
		home := getenv("HOME")
		if home == "" {
			return "", backend.ServerSource{}, fmt.Errorf("%w: neither XDG_STATE_HOME nor HOME is set", ErrResolveSocket)
		}
		state = filepath.Join(home, ".local", "state")
	}
	socket := filepath.Join(state, "cmux", "cmux.sock")
	if err := checkSocketPath(socket); err != nil {
		return "", backend.ServerSource{}, err
	}
	return socket, backend.CmuxDefaultServer(), nil
}

// checkSocketPath applies the shape rules a socket path must satisfy before it
// is worth trying: absolute, clean, and short enough for sun_path.
func checkSocketPath(socket string) error {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket {
		return fmt.Errorf("%w: socket path must be absolute and clean", ErrResolveSocket)
	}
	if len(socket) > maxSocketPathLen {
		return fmt.Errorf("%w: socket path exceeds %d bytes", ErrResolveSocket, maxSocketPathLen)
	}
	return nil
}

// Kind reports the backend this adapter drives.
func (a *Adapter) Kind() backend.Kind { return backend.KindCmux }

// pinnedEnv is the environment every command in this package carries.
//
// The socket replacement is the pin, and it is the only one that is
// load-bearing. CMUX_QUIET is set alongside it because cmux's legacy verbs
// print a deprecation notice to STDOUT before their payload; this package uses
// the canonical verbs, so the notice does not fire today, and the flag is set
// anyway so that a future rename cannot turn a parser's first row into prose.
//
// Nothing else in the inherited environment is touched. In particular cmux's
// own authentication variable is left exactly as the operator set it: this
// package never reads, logs, or reimplements that password, it simply lets the
// CLI's existing auth flow run.
func pinnedEnv(socket string) []exec.EnvMutation {
	return []exec.EnvMutation{
		exec.ReplaceCmuxSocketPath(socket),
		exec.SetCmuxQuiet(),
	}
}

// globalArgs are the flags that must precede every subcommand.
//
// `--id-format uuids` is not cosmetic. It is what makes create's `--json` reply
// carry "workspace_id" with a UUID instead of "workspace_ref" with a positional
// index — a key rename, not just a value change, so its absence presents as a
// missing field rather than a wrong one. Emitting it on every command means no
// call site can forget it, exactly as tmux's socket pin does.
func globalArgs() []exec.Arg {
	return []exec.Arg{exec.MustFixed("--id-format"), exec.MustFixed("uuids")}
}

func (a *Adapter) command(kind exec.CommandKind, args ...exec.Arg) exec.SensitiveCommand {
	return exec.SensitiveCommand{
		Kind:      kind,
		Path:      exec.Secret(a.cmuxPath),
		Args:      append(globalArgs(), args...),
		Env:       pinnedEnv(a.socket),
		StdoutCap: stdoutCap,
		StderrCap: stderrCap,
	}
}
