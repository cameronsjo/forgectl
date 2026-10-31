// Package hcadapter drives herdr-core's terminal service, `hc`, on behalf of
// the surface launcher.
//
// herdr-core splits what herdr 0.9 did in one server: `hc` runs and keeps
// terminals, and `herdr-app` only changes what the person's window shows. A
// worker needs only hc, so this adapter never calls herdr-app.
//
// The shape follows the other adapters. Every call goes through the sensitive
// runner, so the sealed cwd, display name, and bootstrap reach hc's argv and
// nothing else, and so a worker's screen text and a brief's text never reach
// the argv-logging Runner. Identity is checked on every call that acts on a
// terminal: the core's id, the terminal's address and creation time, and the
// forgectl_surface label carrying the ownership marker (see locate).
//
// The JSON shapes parsed here were measured on hc 0.1.15. The parsers are
// strict and fail closed: anything they cannot read is unreadable, never gone.
package hcadapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// ownerLabel is the label key carrying forgectl's ownership marker. The
// marker cannot be the terminal's name: hc names are at most 32 characters
// and the marker is 43.
const ownerLabel = "forgectl_surface"

// ledgerPrefix keeps an hc session's worker ledger apart from a herdr session
// of the same name: the ledger is keyed by the session string alone.
const ledgerPrefix = "hc:"

// defaultSession is the session name used when HC_SESSION is unset. It only
// names the ledger and seeds the fingerprint; hc itself is never told it.
const defaultSession = "default"

// maxSessionLen bounds HC_SESSION. hc accepts longer, but a value this long is
// not a session anyone typed.
const maxSessionLen = 64

// Output caps. An `hc get` reply is a few hundred bytes and an `hc read` reply
// is at most a few hundred rows, so both fit the seam's ceiling; truncation is
// reported as incomplete and read as unreadable.
const (
	stdoutCap = exec.MaxOutputBytes
	stderrCap = 1 << 14
)

// hc's documented exit codes (`hc --skill`, Status, recovery, and cleanup).
const (
	exitNotFound       = 3
	exitBlocked        = 9
	exitStatusMismatch = 10
	exitFinished       = 11
)

// ErrResolveSession reports that the hc session could not be chosen.
var ErrResolveSession = errors.New("hcadapter: cannot resolve the hc session")

// Adapter drives the hc core the environment selects, pinned at construction.
type Adapter struct {
	run    exec.SensitiveRunner
	hcPath string
	// session is HC_SESSION, or defaultSession when it is unset. hc reads
	// HC_SESSION from the environment every command inherits, so the pin is
	// that inheritance plus the fingerprint check: a later process whose
	// HC_SESSION differs reaches a core whose id does not match.
	session string
	source  backend.ServerSource
}

// New builds an adapter for the hc core the environment selects. hcPath must
// be absolute, for the reason every adapter's binary path is: the sensitive
// runner refuses to let a PATH lookup choose it.
func New(run exec.SensitiveRunner, hcPath string, getenv func(string) string) (*Adapter, error) {
	if run == nil {
		return nil, fmt.Errorf("%w: no runner", ErrResolveSession)
	}
	if !filepath.IsAbs(hcPath) {
		return nil, fmt.Errorf("%w: hc path must be absolute", ErrResolveSession)
	}
	session, source := defaultSession, backend.HcDefaultSessionServer()
	if s := getenv("HC_SESSION"); s != "" {
		if !validSession(s) {
			return nil, fmt.Errorf("%w: HC_SESSION must be 1-%d of a-z A-Z 0-9 - _ .", ErrResolveSession, maxSessionLen)
		}
		session, source = s, backend.HcNamedSessionServer()
	}
	return &Adapter{run: run, hcPath: hcPath, session: session, source: source}, nil
}

func validSession(s string) bool {
	if s == "" || len(s) > maxSessionLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// Kind reports the backend this adapter drives.
func (a *Adapter) Kind() backend.Kind { return backend.KindHc }

// Session is the worker-ledger key for this adapter's core: "hc:" and the
// session name, so it never collides with a herdr session's ledger.
func (a *Adapter) Session() string { return ledgerPrefix + a.session }

// ErrBadName reports a display name hc would refuse.
var ErrBadName = errors.New("hc: the name must be 1-32 of a-z 0-9 - _ ., start with a letter or digit, and not look like a terminal address (t7)")

// maxNameLen is hc's name limit.
const maxNameLen = 32

// CheckName applies hc's terminal-name rules, measured on hc 0.1.15, so a
// launch can refuse a name before it creates a worktree or a ledger row. The
// adapter itself cannot check: by the time it runs, the name is sealed.
func CheckName(name string) error {
	if name == "" || len(name) > maxNameLen {
		return ErrBadName
	}
	if !isLowerAlnum(name[0]) {
		return ErrBadName
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !isLowerAlnum(c) && c != '-' && c != '_' && c != '.' {
			return ErrBadName
		}
	}
	// hc refuses a name an address could be mistaken for: "t", a digit, and
	// then letters or digits ("t7", "t1r", "t12").
	if len(name) >= 2 && name[0] == 't' && name[1] >= '0' && name[1] <= '9' {
		rest := name[2:]
		addressLike := true
		for i := 0; i < len(rest); i++ {
			if !isLowerAlnum(rest[i]) {
				addressLike = false
				break
			}
		}
		if addressLike {
			return ErrBadName
		}
	}
	return nil
}

func isLowerAlnum(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') }

func (a *Adapter) command(kind exec.CommandKind, args ...exec.Arg) exec.SensitiveCommand {
	return exec.SensitiveCommand{
		Kind: kind,
		Path: exec.Secret(a.hcPath),
		// --json first on every verb that takes it is the caller's job; this
		// only fixes the binary and the caps. HC_SESSION and HC_TERMINAL_ID are
		// inherited: the first selects the core, and the second makes a worker
		// started from inside hc a child of the launcher's terminal, so hc's
		// own notes about a blocked or exited worker reach the coordinator.
		Args:      args,
		Env:       nil,
		StdoutCap: stdoutCap,
		StderrCap: stderrCap,
	}
}

// hcError is the error object hc prints on stderr.
type hcError struct {
	Error struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	} `json:"error"`
}

// errorKind reads hc's error kind from a complete stderr, or "" when stderr is
// truncated or not hc's error object.
func errorKind(out exec.BoundedOutput) string {
	raw, complete := out.CopyBytesForParse()
	if !complete {
		return ""
	}
	var e hcError
	if err := json.Unmarshal(raw, &e); err != nil {
		return ""
	}
	return e.Error.Kind
}

// classifyRunError maps a runner error onto the closed failure vocabulary, by
// the seam's sentinels (see tmuxadapter.classifyRunError for why not
// context's).
func classifyRunError(err error) backend.StartCause {
	switch {
	case errors.Is(err, exec.ErrCanceled):
		return backend.NewStartCause(backend.FailureCanceled, err)
	case errors.Is(err, exec.ErrTimeout):
		return backend.NewStartCause(backend.FailureTimeout, err)
	case errors.Is(err, exec.ErrInvalidCommand):
		return backend.NewStartCause(backend.FailureInternal, err)
	default:
		return backend.NewStartCause(backend.FailureUnavailable, err)
	}
}

// decodeStdout parses a complete stdout as one JSON value.
func decodeStdout(out exec.BoundedOutput, v any) error {
	raw, complete := out.CopyBytesForParse()
	if !complete {
		return errors.New("hc output was truncated")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return errors.New("hc output is not the expected JSON")
	}
	return nil
}
