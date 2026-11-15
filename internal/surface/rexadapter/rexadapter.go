// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

// Package rexadapter drives Rex, through its `rex` CLI, on behalf of the
// surface launcher.
//
// The shape follows hcadapter. Every call goes through the sensitive runner,
// so the sealed cwd and display name reach rex's argv and nothing else, and no
// log line or error string can render them.
//
// # The worker's environment and bootstrap
//
// Rex has no environment flag, and needs none: a worker's environment
// (FORGECTL_DRAIN_WORKER and the rest) is part of the sealed invocation the
// coordinator sends the trampoline over the bootstrap socket. What Rex must
// start is the bootstrap line, and that never reaches rex at all: Start
// writes it into a private launch script (exec.NewLaunchScript: a 0600 file
// in a fresh 0700 directory, removed by its own first line once sh has opened
// it, and by Start on every other path) and asks Rex to run `/bin/sh
// <script>`. Rex records and reports only /bin/sh and a random path.
//
// Residual exposure: a process running as the operator's uid can read the
// script for the milliseconds before sh opens it, and can read the
// trampoline's argv, which holds the same line for as long as the worker runs
// (true of every backend). A forgectl killed in that window leaves the 0600
// file behind with a spent nonce. start.go's decision comment lists the
// options refused (argv, initial_input, an env wrapper) and why.
//
// # What a worker is here
//
// Each launch gets a Rex session of its own, created detached with `rex new`
// (no app window opens). The session's label is the launch's ownership name
// (fc-surface-<tag>), its window's label is the display name, and its one
// block runs the launch script with --keep-open, so a finished block stays
// listed with its exit code instead of vanishing. Start then renames the
// block to forgectl.surface=<ownership name>. A reference names the block:
// the session id, the full block id, and the creation time that id embeds.
//
// # Identity
//
// Every call that acts on a block checks, in order (locate):
//
//  1. The Rex server: `rex whoami` must report API revision 14 (APIRevision;
//     any other refuses), a local unix-socket endpoint (Start also requires
//     Tailscale sharing reported disabled, at launch time only), and the instance_id
//     and host_id the reference was fingerprinted with
//     (backend.FingerprintRexServer). A restarted server is a new instance.
//  2. The block: `rex block ls` in the reference's session must list the
//     reference's full block id.
//  3. The owner marker: the block's label must be
//     forgectl.surface=<the reference's ownership name>. The reference also
//     stores the creation time its block id embeds, and a decoded reference
//     whose time disagrees with its id is refused; at use time that is a
//     consistency check on the id, not a second source, because both values
//     come from the same string.
//
// A label is not authenticated. Any Rex client can rename a block, any client
// can give a block forgectl's label, and labels are not unique. The label is
// the same strength as hc's forgectl.surface label, which is why the check
// also requires the full block id (UUIDv7, never handed out twice) and the
// server's instance_id: a forged label on another block fails the id, and a
// relabeled worker fails the label, so close refuses rather than guesses.
//
// Close removes the block (`rex block close`), and then removes the session
// only when it is the one Start created (its label is the ownership name) and
// it holds no other block. It never kills a session it did not create.
//
// # Parsing
//
// The JSON shapes and error texts parsed here were measured on Rex Beta
// 0.1.0-preview.19 (API revision 14), and fixtures from that run are in
// testdata. The parsers are strict and fail closed: anything they cannot
// positively read is unreadable, never gone. Only Rex's own "not found" for
// the exact id asked about, with its exit code 3, is evidence of absence. An
// exit code alone never is: rc 3 is also a child's exit code under --wait,
// and rc 1 is also a timeout.
package rexadapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
)

// DefaultBinaryName is the name a caller resolves when no locator supplies the
// rex path. Rex installs `rex` inside its app bundle and not on PATH, so on
// most machines a locator has to find it; this package takes only an absolute
// path either way.
const DefaultBinaryName = "rex"

// APIRevision is the Rex API revision every parser here was measured on. A
// server reporting any other revision is refused before anything is created
// or acted on: a preview CLI changes shape between revisions, and a parser
// reading a shape it was not built for is how a close lands on the wrong
// block.
const APIRevision = 14

// ownerLabelKey prefixes a block label carrying forgectl's ownership marker:
// forgectl.surface=fc-surface-<32 hex>, 60 characters, inside Rex's
// 128-character label limit.
const ownerLabelKey = "forgectl.surface="

// maxRexLabelLen is Rex's block and window label limit.
const maxRexLabelLen = 128

// Binaries the adapter runs besides rex. Both are named absolutely: the
// sensitive runner refuses a PATH lookup, and Rex may start a block with no
// PATH at all.
const (
	shellPath = "/bin/sh"
	testPath  = "/bin/test"
)

// Output caps. Every rex reply parsed here is a few kilobytes at most; a
// truncated one is unreadable.
const (
	stdoutCap = exec.MaxOutputBytes
	stderrCap = 1 << 14
)

// Bounds on the server identity strings, so a malformed reply cannot put an
// unbounded value into a fingerprint.
const (
	maxEndpointLen = 4096
	maxServerIDLen = 64
)

// localEndpointPrefix is the only endpoint scheme accepted. Rex can be pointed
// at a remote home server; a worker's cwd and name must not travel to another
// machine, and the bootstrap's socket would not reach this one from there.
// A unix endpoint says only how this CLI reaches the server, not who else may:
// the server must also report Tailscale sharing disabled (see parseWhoami),
// because a tailnet member sees every block's cwd and label. That check is
// made once, when a launch begins, and only for a launch.
const localEndpointPrefix = "unix://"

// Script consumption bounds: how long Start waits for the launch script to
// remove itself (sh has opened it), and how often it looks.
const (
	defaultConsumeTimeout = 10 * time.Second
	defaultConsumePoll    = 20 * time.Millisecond
)

// ErrNew reports an adapter that cannot be built.
var ErrNew = errors.New("rexadapter: cannot build the adapter")

// launchScript is the part of exec.LaunchScript the adapter uses, so a test
// can watch its lifecycle.
type launchScript interface {
	Path() string
	Consumed() (bool, error)
	Remove() error
}

// Adapter drives the Rex server a bare `rex` reaches, pinned at construction.
type Adapter struct {
	run     exec.SensitiveRunner
	rexPath string
	source  backend.ServerSource

	scriptDir      string
	newScript      func(dir string, line exec.Arg) (launchScript, error)
	consumeTimeout time.Duration
	consumePoll    time.Duration
	sleep          func(time.Duration)
	// sessionLabel is the label Start gives a launch's session and the one
	// reconcile and Close look for: the ownership name. Only the live test
	// replaces it, so its one scratch session carries the name it announces.
	sessionLabel func(backend.RecoveryTag) string
}

// Option configures an Adapter.
type Option func(*Adapter)

// WithScriptDir puts launch scripts under dir, which must be absolute, in
// place of os.TempDir().
func WithScriptDir(dir string) Option { return func(a *Adapter) { a.scriptDir = dir } }

// New builds an adapter. rexPath must be absolute, for the reason every
// adapter's binary path is: the sensitive runner refuses to let a PATH lookup
// choose it. A locator, or the caller's own PATH lookup of DefaultBinaryName,
// supplies it.
func New(run exec.SensitiveRunner, rexPath string, opts ...Option) (*Adapter, error) {
	if run == nil {
		return nil, fmt.Errorf("%w: no runner", ErrNew)
	}
	if !filepath.IsAbs(rexPath) {
		return nil, fmt.Errorf("%w: the rex path must be absolute", ErrNew)
	}
	a := &Adapter{
		run:       run,
		rexPath:   rexPath,
		source:    backend.RexDefaultServer(),
		scriptDir: os.TempDir(),
		newScript: func(dir string, line exec.Arg) (launchScript, error) {
			return exec.NewLaunchScript(dir, line)
		},
		consumeTimeout: defaultConsumeTimeout,
		consumePoll:    defaultConsumePoll,
		sleep:          time.Sleep,
		sessionLabel:   backend.RecoveryTag.OwnershipName,
	}
	for _, o := range opts {
		o(a)
	}
	if !filepath.IsAbs(a.scriptDir) {
		return nil, fmt.Errorf("%w: the launch script directory must be absolute", ErrNew)
	}
	return a, nil
}

// Kind reports the backend this adapter drives.
func (a *Adapter) Kind() backend.Kind { return backend.KindRex }

// ownerLabel is the block label carrying a launch's ownership marker.
func ownerLabel(tag backend.RecoveryTag) string { return ownerLabelKey + tag.OwnershipName() }

// command builds one rex call. Every call carries two global flags first:
// --autostart=false, so a call that finds no server fails rather than
// starting one (a server forgectl started would inherit this process's
// environment and pass it to every block it ever runs), and --color=never,
// so the stderr text classified below carries no escape sequences. The
// launching session's handles are dropped from the environment either way.
func (a *Adapter) command(kind exec.CommandKind, args ...exec.Arg) exec.SensitiveCommand {
	all := make([]exec.Arg, 0, len(args)+2)
	all = append(all, exec.MustFixed("--autostart=false"), exec.MustFixed("--color=never"))
	all = append(all, args...)
	return exec.SensitiveCommand{
		Kind:      kind,
		Path:      exec.Secret(a.rexPath),
		Args:      all,
		Env:       []exec.EnvMutation{exec.UnsetLauncherSessionHandles()},
		StdoutCap: stdoutCap,
		StderrCap: stderrCap,
	}
}

// classifyRunError maps a runner error onto the closed failure vocabulary by
// the seam's sentinels, as hcadapter does.
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

// targetKind is the noun in Rex's "not found" error.
type targetKind string

const (
	targetSession targetKind = "session"
	targetBlock   targetKind = "block"
)

// rexNotFoundExit is the exit code Rex gives a missing target.
const rexNotFoundExit = 3

// notFound reports whether a failed call is Rex saying that exactly target,
// of exactly that kind, does not exist: exit code 3 and a complete stderr
// that is the one line `Error: <kind> target "<target>" not found`. Both
// halves are required. The exit code alone collides with a child's exit code
// under --wait, and the text alone would let any failure that quoted the id
// read as absence. target is always an id this adapter parsed, so it carries
// no character Rex would escape inside the quotes.
func notFound(res exec.SensitiveResult, kind targetKind, target string) bool {
	if res.ExitCode != rexNotFoundExit {
		return false
	}
	raw, complete := res.Stderr.CopyBytesForParse()
	if !complete {
		return false
	}
	line := strings.TrimSuffix(string(raw), "\n")
	return line == "Error: "+string(kind)+" target \""+target+"\" not found"
}

// decodeOne parses raw as exactly one JSON value. Unknown fields are
// ignored, as the measurement recommends for Rex's flat objects; a missing
// required field is each parser's to refuse.
func decodeOne(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(v); err != nil {
		return errors.New("rex output is not the expected JSON")
	}
	if dec.More() {
		return errors.New("rex output has content after its JSON value")
	}
	return nil
}
