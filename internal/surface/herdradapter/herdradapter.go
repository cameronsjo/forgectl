// Package herdradapter drives a herdr server on behalf of the surface launcher.
//
// It is the third of #332's three adapters, and the first thing to say about it
// is that the plan was wrong about its most basic property.
//
// The plan specifies herdr's endpoint pin as HERDR_CONFIG_PATH, and the
// sensitive seam ships exec.ReplaceHerdrConfigPath to carry it. On herdr 0.8.0
// that variable DOES NOT SELECT A SERVER — measured three ways: a config path
// naming a nonexistent file, and one naming a different socket, both resolved
// to the same endpoint as no config at all, and there is no --socket or
// --config global flag. An adapter pinning that way would pin nothing while
// appearing to. What selects a server is the SESSION: `--session <name>` is a
// global flag, and `herdr session list` maps each name to its own socket
// (forgectl#364).
//
// So herdr's pin is a flag, like tmux's -S, not an environment mutation like
// cmux's CMUX_SOCKET_PATH. That is better than it sounds: an argv operand can be
// asserted the same way tmux's socket pin is, rather than by comparing an
// environment the fake would have to model.
//
// Three more properties shape this package, all measured against a live 0.8.0
// server.
//
// The create reply is COMPLETE. `workspace create --json` returns the workspace,
// its tab, and its root pane in one envelope — workspace_id, tab_id, and pane_id
// together — so nothing has to be looked up afterwards. That matters because the
// bootstrap goes to a PANE, and a second call to find it would be a second place
// the answer could be lost.
//
// Errors are STRUCTURED. A refusal is JSON on stderr carrying a machine-readable
// code: {"error":{"code":"workspace_not_found",...}}. This package matches the
// code, never the prose, which is the thing cmux's adapter could not do and had
// to approximate with a substring.
//
// And the bootstrap needs a SEPARATE call. workspace.create takes only
// {cwd, env, focus, label} — no command field — so creating the surface and
// starting the harness in it are two operations. `herdr pane run <pane> <cmd>`
// sends the text and Enter in one call (a client-side composition; the protocol
// itself has send_text and send_keys and no run method). The consequence is the
// contract's most awkward case and it is handled explicitly in Start: a create
// that succeeds followed by a run that fails is RefKnown-with-cause, never a
// bare error, because the workspace exists and something has to close it.
package herdradapter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// maxSocketPathLen bounds the endpoint path at the smaller of the two sun_path
// limits (104 on Darwin, 108 on Linux), so a path the kernel would refuse is
// refused here with a diagnostic that names the reason.
const maxSocketPathLen = 104

// defaultSession is the session a bare `herdr` command talks to. It is matched
// by name against `herdr session list`, which reports it verbatim.
const defaultSession = "default"

// sessionEnv names the session when the operator has chosen one.
//
// It is read for a NAME, not for a socket or a config path, which is the whole
// correction this package is built on.
const sessionEnv = "HERDR_SESSION"

// Adapter drives one herdr session, pinned for its whole life.
type Adapter struct {
	run exec.SensitiveRunner

	// herdrPath is absolute: the sensitive runner refuses a relative path so the
	// binary is chosen here rather than by a PATH lookup against the live
	// process environment.
	herdrPath string

	// session is the pin. Every command carries it as `--session <name>`, and
	// source records the CHAIN that chose it — a reference recording the name
	// alone could authorize a server the chain would no longer select.
	session string
	source  backend.ServerSource

	// lstat is seamed so incarnation fingerprinting and the socket-ownership
	// check are testable without a live herdr.
	lstat func(string) (os.FileInfo, error)

	// selfUID is seamed for the same reason the trampoline's is: the ownership
	// refusal cannot be provoked honestly from a unit test, so without a seam
	// the suite would be indifferent to whether the comparison exists.
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

// ErrResolveSession reports that the herdr session could not be chosen.
var ErrResolveSession = errors.New("herdradapter: cannot resolve the herdr session")

// ErrNoSuchSession and ErrSessionNotRunning are separate because they send an
// operator to different places — a typo in the session name, versus a session to
// start — and they share a failure class, so the class cannot carry the
// difference. Being sentinels rather than anonymous errors is also what makes
// each separably testable: the roster-presence check is otherwise masked by the
// running check, since an absent name yields a zero row that is not running.
var (
	ErrNoSuchSession     = errors.New("herdradapter: no herdr session by that name exists")
	ErrSessionNotRunning = errors.New("herdradapter: that herdr session is not running")
)

// New builds an adapter pinned to one herdr session.
//
// The session is chosen ONCE, here, and every later command carries it. That is
// what makes close and probe reach the same server the create reached.
//
// Only the NAME is settled at construction. Whether that session is running,
// which socket it owns, and whether we own that socket are live questions
// answered at readiness — where a stopped session is an honest "herdr is not
// running there" rather than a construction error.
func New(run exec.SensitiveRunner, herdrPath string, getenv func(string) string, opts ...Option) (*Adapter, error) {
	if run == nil {
		return nil, fmt.Errorf("%w: no runner", ErrResolveSession)
	}
	if !filepath.IsAbs(herdrPath) {
		return nil, fmt.Errorf("%w: herdr path must be absolute", ErrResolveSession)
	}
	session, source, err := resolveSession(getenv)
	if err != nil {
		return nil, err
	}
	a := &Adapter{
		run:       run,
		herdrPath: herdrPath,
		session:   session,
		source:    source,
		lstat:     os.Lstat,
		selfUID:   os.Geteuid,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a, nil
}

// resolveSession picks the session and records which chain picked it.
//
// The name is validated rather than passed through, because it becomes a
// command-line operand: herdr session names are plain identifiers, and anything
// carrying a separator, whitespace, or a leading dash is either not a session
// name or is trying to be another argument.
func resolveSession(getenv func(string) string) (string, backend.ServerSource, error) {
	if named := getenv(sessionEnv); named != "" {
		if err := validSessionName(named); err != nil {
			return "", backend.ServerSource{}, err
		}
		return named, backend.HerdrNamedSessionServer(), nil
	}
	return defaultSession, backend.HerdrDefaultSessionServer(), nil
}

// maxSessionNameLen bounds a session name. herdr's own names are short; the cap
// exists so an operator's environment cannot put an unbounded string on a
// command line.
const maxSessionNameLen = 64

// validSessionName keeps the pin to a shape that is unambiguously one operand.
func validSessionName(name string) error {
	if len(name) > maxSessionNameLen {
		return fmt.Errorf("%w: session name exceeds %d bytes", ErrResolveSession, maxSessionNameLen)
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("%w: session name may not begin with a dash", ErrResolveSession)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return fmt.Errorf("%w: session name may contain only letters, digits, and -._", ErrResolveSession)
		}
	}
	return nil
}

// checkSocketPath applies the shape rules a socket path must satisfy before it
// is worth trusting. The path comes from herdr's own status reply rather than
// from us, which is exactly why it is checked.
func checkSocketPath(socket string) error {
	if socket == "" {
		return errors.New("herdr reported no socket for this session")
	}
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket {
		return errors.New("herdr reported a socket path that is not absolute and clean")
	}
	if len(socket) > maxSocketPathLen {
		return errors.New("herdr reported a socket path longer than the platform allows")
	}
	return nil
}

// Kind reports the backend this adapter drives.
func (a *Adapter) Kind() backend.Kind { return backend.KindHerdr }

// pinned prefixes an argv with this adapter's session pin. Every command goes
// through it, so there is one spelling of `--session` and no call site can issue
// an unpinned command by forgetting it.
//
// The name travels as exec.Opaque rather than MustFixed because it is dynamic —
// it can come from the environment — and Opaque's leading-dash refusal is a
// second line behind validSessionName's.
func (a *Adapter) pinned(args ...exec.Arg) []exec.Arg {
	return append([]exec.Arg{exec.MustFixed("--session"), exec.Opaque(a.session)}, args...)
}

func (a *Adapter) command(kind exec.CommandKind, args ...exec.Arg) exec.SensitiveCommand {
	return exec.SensitiveCommand{
		Kind: kind,
		Path: exec.Secret(a.herdrPath),
		Args: a.pinned(args...),
		// No environment mutation. herdr's pin is the flag above; the seam's
		// ReplaceHerdrConfigPath does not select a server and using it here
		// would be pinning with something that pins nothing (forgectl#364).
		Env:       nil,
		StdoutCap: stdoutCap,
		StderrCap: stderrCap,
	}
}
