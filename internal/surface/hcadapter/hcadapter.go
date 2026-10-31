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
// forgectl.surface label carrying the ownership marker (see locate).
//
// The JSON shapes parsed here were measured on hc 0.1.15. The parsers are
// strict and fail closed: anything they cannot read is unreadable, never gone.
package hcadapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// ownerLabel is the label key carrying forgectl's ownership marker. The
// marker cannot be the terminal's name: hc names are at most 32 characters
// and the marker is 43. Labels are a namespace shared with herdr-app (which
// writes "where" and "herdr.*"), so the key is namespaced and only this key
// is compared; the rest of the label set is never required to be forgectl's.
//
// A label is not authenticated: any hc client can write one. It is the same
// strength as herdr's workspace label, which is why locate also requires the
// recorded creation time and core id.
const ownerLabel = "forgectl.surface"

// spaceLabel is herdr-app's label for the space a terminal shows in. A
// terminal started with hc opts into the herdr-app window with
// herdr.space=<space id>, and without it stays hidden (herdr-app --skill).
// It is only ever written at Start, never compared: the window may move or
// drop it.
const spaceLabel = "herdr.space"

// maxSpaceIDLen bounds a herdr-app space id. herdr-app's ids are "sp" and 32
// hex digits; anything much longer is not one.
const maxSpaceIDLen = 64

// ErrBadSpace reports a herdr-app space id forgectl will not pass to hc.
var ErrBadSpace = errors.New("hc: a herdr-app space id is 1-64 letters, digits, hyphens or underscores, such as one herdr-app ls prints")

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

// hc's error kinds this package acts on. Errors are matched on kind, never
// on exit code: several kinds share code 1.
const (
	kindNotFound       = "not_found"
	kindNameTaken      = "name_taken"
	kindInvalidParams  = "invalid_params"
	kindSpawnFailed    = "spawn_failed"
	kindBlocked        = "blocked"
	kindStatusMismatch = "status_mismatch"
	kindFinished       = "finished"
	kindLive           = "live"
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
	// space, when set, is the herdr-app space Start labels a worker for, so
	// the window shows it. Empty leaves the worker out of the window.
	space string
	// selection is the digest of what picks the core (SelectionDigest).
	selection string
}

// selectionKeys are the variables that can choose which hc core a call
// reaches, measured on hc 0.1.15: the core's socket lives under
// $XDG_RUNTIME_DIR/herdr-core/<session>.sock, its state and config under the
// XDG state and config homes (or HOME without them), and HC_SOCKET names a
// socket outright. Over-inclusion only fails toward refusal: a value that
// differs makes forget refuse rather than call a core wiped.
var selectionKeys = []string{"HC_SOCKET", "XDG_RUNTIME_DIR", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "HOME"}

// selectionDigest hashes the resolved session and selectionKeys' values, so
// a ledger row records which core selection launched it without storing the
// paths themselves.
func selectionDigest(session string, getenv func(string) string) string {
	var b strings.Builder
	b.WriteString("hc-selection-v1\x00session=" + session + "\x00")
	for _, k := range selectionKeys {
		b.WriteString(k + "=" + getenv(k) + "\x00")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:16])
}

// SelectionDigest is a digest of everything that picks this adapter's core:
// the resolved HC_SESSION and selectionKeys, read at New. A launch records
// it, and forget calls a changed core id a wiped core only when the digest is
// the same now (Abandoned).
func (a *Adapter) SelectionDigest() string { return a.selection }

// ShowInSpace makes Start label each terminal it creates herdr.space=<id>,
// so the herdr-app window shows it in that space. It refuses an id outside
// herdr-app's shape before anything runs.
func (a *Adapter) ShowInSpace(id string) error {
	if !validSpaceID(id) {
		return ErrBadSpace
	}
	a.space = id
	return nil
}

func validSpaceID(s string) bool {
	if s == "" || len(s) > maxSpaceIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
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
	// "default" is hc's own name for the core an unset HC_SESSION selects, so
	// both spellings are one selection: a worker launched under one is closed
	// under the other.
	if s := getenv("HC_SESSION"); s != "" && s != defaultSession {
		if !validSession(s) {
			return nil, fmt.Errorf("%w: HC_SESSION must be 1-%d letters, digits, hyphens, underscores or dots", ErrResolveSession, maxSessionLen)
		}
		session, source = s, backend.HcNamedSessionServer()
	}
	return &Adapter{run: run, hcPath: hcPath, session: session, source: source, selection: selectionDigest(session, getenv)}, nil
}

// ValidSession reports whether s is an HC_SESSION value this package
// accepts: 1-64 letters, digits, '-', '_' or '.'.
func ValidSession(s string) bool { return validSession(s) }

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
		// only fixes the binary and the caps. The environment is inherited so
		// HC_SESSION selects the core, minus the launching session's handles
		// (exec.IsLauncherSessionHandle): a call that finds no core running
		// starts one, and that core passes its own environment to every
		// terminal it later starts, so the launcher's messaging socket and
		// token, session id, pane and terminal ids, and worker identity would
		// otherwise reach terminals forgectl never launched.
		//
		// Dropping HC_TERMINAL_* also hides the launcher's terminal from hc,
		// so whether hc still treats a worker as that terminal's child is not
		// measured. Start passes --outlive-parent and --no-notify either way,
		// so a worker never dies with the launcher's terminal or sends it
		// notes, whichever hc decides.
		Args:      args,
		Env:       []exec.EnvMutation{exec.UnsetLauncherSessionHandles()},
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
