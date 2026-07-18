// Package tmux is the ops layer: a Client that drives tmux directly and
// delegates smart session creation to sesh. It knows nothing of Cobra or
// Bubble Tea — that decoupling is the whole point, and is what lets the CLI
// and TUI both be thin callers over a tested core.
package tmux

import (
	"fmt"
	"os"
	osexec "os/exec"

	"github.com/cameronsjo/forgectl/internal/exec"
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
}

// Option configures a Client at construction.
type Option func(*Client)

// WithInsideTmux overrides the $TMUX detection — used in tests.
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
func (c *Client) InsideTmux() bool { return c.insideTmux() }
