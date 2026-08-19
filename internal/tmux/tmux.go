// Package tmux is the ops layer: a Client that drives tmux directly and
// delegates smart session creation to sesh. It knows nothing of Cobra or
// Bubble Tea — that decoupling is the whole point, and is what lets the CLI
// and TUI both be thin callers over a tested core.
package tmux

import (
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Client wraps tmux + sesh behind the exec.Runner seam.
type Client struct {
	run     exec.Runner
	tmuxBin string
	seshBin string

	// insideTmux is injectable so tests can flip the inside/outside branch
	// (the bit the old bash `s` script got subtly wrong). Defaults to a live
	// check of $TMUX.
	insideTmux func() bool

	// lookPath resolves a binary name to a PATH entry — injectable (like
	// insideTmux) so tests don't require a real sesh on PATH. Defaults to
	// os/exec.LookPath.
	lookPath func(string) (string, error)

	// Server-state seams keep the default-socket classifier deterministic in
	// same-package tests without exposing filesystem configuration publicly.
	getenv func(string) string
	// getuid is os.Getuid, NOT os.Geteuid, and the distinction is
	// load-bearing: tmux's own make_label() builds the default socket
	// directory from getuid(), so matching it exactly is what makes the
	// derived path the same path tmux would use. Verified against tmux
	// master, tmux.c make_label(): `uid = getuid();` then
	// `xasprintf(&base, "%s/tmux-%ld", path, (long)uid);` — real uid, and the
	// same value it later compares st_uid against. Under a setuid/setgid
	// forgectl the two diverge, and the failure is fail-OPEN — we would lstat
	// a tmux-<euid> directory that does not exist and classify a LIVE server
	// as serverAbsent, the one classification meaning "proceed".
	// ListWindows would then return (nil, nil), LiveReviews would report 0,
	// and the concurrency cap would grant a full batch on a machine already
	// running reviews.
	getuid func() int
	lstat  func(string) (os.FileInfo, error)

	// socket pins every command to one tmux server via `-S`, overriding the
	// environmental selection. Empty means the environmental mode, which is
	// what every pre-existing caller gets. It is set once at construction and
	// never mutated: tmuxArgs, currentSelector, and classifyServerFailure all
	// read it, and a client whose socket could move mid-life would have them
	// disagreeing about which server an in-flight identity belongs to.
	socket string
}

// MaxSocketPathLen bounds a pinned socket's absolute path. Unix sun_path is 104
// bytes on macOS; the margin leaves room for tmux's own suffixing.
//
// It is EXPORTED rather than private because the dependency direction forbids
// only the import, not the sharing: internal/tmux must not import the surface
// package that will become its caller, but surface may import tmux. So surface
// aliases this constant (the pattern internal/cli already uses) and a test
// there asserts the two are equal. Two independent literals are how a producer
// mints a socket its own consumer then refuses — forgectl#343, and the reason
// internal/cli stopped restating this bound.
const MaxSocketPathLen = 100

// Option configures a Client at construction.
type Option func(*Client)

// WithInsideTmux overrides the $TMUX detection — used in tests.
//
// It configures the ENVIRONMENTAL answer only. A socket-pinned client reports
// false unconditionally (see InsideTmux), so passing this to NewPinned is
// accepted and has no effect. That is by design rather than a dropped option:
// under a pin, $TMUX describes a client of a different server, so there is no
// environment for it to answer about.
func WithInsideTmux(fn func() bool) Option {
	return func(c *Client) { c.insideTmux = fn }
}

// WithBins overrides the tmux/sesh binary names (mostly for tests).
func WithBins(tmuxBin, seshBin string) Option {
	return func(c *Client) {
		c.tmuxBin = tmuxBin
		c.seshBin = seshBin
	}
}

// WithLookPath overrides the PATH-resolution check sesh calls use to confirm
// sesh is installed — used in tests to avoid depending on a real sesh binary.
func WithLookPath(fn func(string) (string, error)) Option {
	return func(c *Client) { c.lookPath = fn }
}

// NewPinned builds a Client bound to one tmux server by socket path, so every
// command it issues carries `-S <socket>` and reaches that server regardless of
// $TMUX or $TMUX_TMPDIR.
//
// It is a constructor rather than an Option because the pin has a validity
// requirement an Option could not report: an Option returns nothing, so a bad
// socket would be accepted silently and surface later as an unattributable tmux
// failure against a server nobody meant to reach.
//
// The path must be absolute and already clean. Relative is refused because it
// would resolve against whatever working directory the tmux child inherits —
// making the server a command reaches depend on where forgectl happened to be
// invoked, which is the exact ambiguity a pin exists to remove. Unclean is
// refused so the string compared in classifyServerFailure and the string lstat
// inspects are the same string, with no traversal left to normalize away.
//
// A pinned client never unsets $TMUX for the child, and does not need to: `-S`
// overrides socket selection outright, and tmux's nesting refusal fires only on
// a command that would ATTACH. Every command a pinned client can still issue is
// `-d` or read-only, because attaching and switching are refused outright — see
// ErrAttachUnavailableWhenPinned. That refusal is what makes this paragraph
// true; without it the attach branch would be both reachable and broken.
//
// CALLER OBLIGATION, and the pin is not a security boundary without it: the
// socket's parent directory — and every component above it — must be writable
// only by the invoking uid. This validates the path as a STRING; it cannot
// validate the filesystem, and the gap is a real one. The create path reads
// "absent" from an lstat and then asks tmux to create, so anyone who can write
// the parent directory can plant a live socket at the path in that window;
// tmux would then CONNECT to a foreign server, and every later selector check
// would agree, because the pin does match. Use a privately-owned 0700 directory
// (internal/surface's run dir is one) rather than a shared temp path.
func NewPinned(run exec.Runner, socket string, opts ...Option) (*Client, error) {
	// The empty case is subsumed by IsAbs — it exists only to say "empty"
	// instead of "must be absolute, got \"\"", which is a worse diagnostic for
	// the most likely mistake. It is not an independent control.
	if socket == "" {
		return nil, fmt.Errorf("%w: it is empty", ErrInvalidSocketPath)
	}
	if !filepath.IsAbs(socket) {
		return nil, fmt.Errorf("%w: must be absolute, got %q", ErrInvalidSocketPath, socket)
	}
	if filepath.Clean(socket) != socket {
		return nil, fmt.Errorf("%w: must be clean, got %q (want %q)", ErrInvalidSocketPath, socket, filepath.Clean(socket))
	}
	// Screen control characters before this path can reach an error message an
	// operator reads. It is caller-supplied and, in the environmental case, can
	// derive from $TMUX_TMPDIR — an absolute, lexically clean path is free to
	// carry an escape sequence that clears the terminal. Byte offset only: the
	// refusal must not print the very bytes it is refusing.
	for i, r := range socket {
		if termsafe.IsUnsafeTerminalRune(r) {
			return nil, fmt.Errorf("%w: carries a control character at byte %d", ErrInvalidSocketPath, i)
		}
	}
	// The same ceiling internal/surface applies, and for the same reason: a
	// too-long path fails at tmux's own bind as an unattributable error against
	// a server nobody meant to reach. Refusing here names the cause.
	if len(socket) > MaxSocketPathLen {
		return nil, fmt.Errorf("%w: %d bytes exceeds the %d-byte limit: %s",
			ErrInvalidSocketPath, len(socket), MaxSocketPathLen, termsafe.QuotePath(socket))
	}
	c := New(run, opts...)
	c.socket = socket
	return c, nil
}

// tmuxArgs prefixes a tmux argv with the client's socket pin. It is the single
// DEFINITION of the `-S` spelling — every call site in this package routes
// through it. It is not an enforcement mechanism: a call site can still forget
// to call it, and nothing here would notice. What catches that is
// TestPinnedClientPinsEveryCommand, which drives each method far enough to
// issue its command and asserts the recorded argv.
//
// The `-S <path>` spelling is separated rather than attached (`-S/path`) on
// purpose — classifyServerFailure recognizes a pinned argv by matching exactly
// these two leading elements, and one spelling on both sides is what keeps that
// match a comparison instead of a parse of tmux's option grammar.
//
// Both branches return a slice the caller may append to without touching
// anything else. The clone is not decorative: Go passes a named slice through a
// variadic parameter WITHOUT copying, so returning `args` directly would hand
// back the caller's own array — and CreateSession already appends to this
// result. Without the clone the hazard would also be mode-dependent, present
// unpinned and absent pinned, which is the worst shape for a latent bug.
func (c *Client) tmuxArgs(args ...string) []string {
	if c.socket == "" {
		return slices.Clone(args)
	}
	return append([]string{"-S", c.socket}, args...)
}

// New builds a Client over the given Runner.
func New(run exec.Runner, opts ...Option) *Client {
	c := &Client{
		run:     run,
		tmuxBin: "tmux",
		seshBin: "sesh",
		insideTmux: func() bool {
			return os.Getenv("TMUX") != ""
		},
		lookPath: osexec.LookPath,
		getenv:   os.Getenv,
		getuid:   os.Getuid,
		lstat:    os.Lstat,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// checkSeshAvailable confirms sesh resolves on PATH before a sesh-delegating
// call shells out to it, giving a clear "sesh not found" error instead of
// letting exec.Runner's generic not-found error surface unattributed.
// Mirrors ClaudePath's guard in internal/launch.
func (c *Client) checkSeshAvailable() error {
	if _, err := c.lookPath(c.seshBin); err != nil {
		return fmt.Errorf("sesh not found on PATH: %w", err)
	}
	return nil
}

// InsideTmux reports whether we're running inside a tmux client (so jumps use
// switch-client rather than attach-session).
//
// A pinned client always reports false. $TMUX names a client of whatever server
// the operator is attached to, which under a pin is a DIFFERENT server than the
// one every command reaches — so answering true would send `switch-client` to
// the pinned server on behalf of a client that is not connected to it.
func (c *Client) InsideTmux() bool {
	if c.socket != "" {
		return false
	}
	return c.insideTmux()
}
