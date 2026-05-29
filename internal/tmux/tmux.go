// Package tmux is the ops layer: a Client that drives tmux directly and
// delegates smart session creation to sesh. It knows nothing of Cobra or
// Bubble Tea — that decoupling is the whole point, and is what lets the CLI
// and TUI both be thin callers over a tested core.
package tmux

import (
	"os"

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

// New builds a Client over the given Runner.
func New(run exec.Runner, opts ...Option) *Client {
	c := &Client{
		run:     run,
		tmuxBin: "tmux",
		seshBin: "sesh",
		insideTmux: func() bool {
			return os.Getenv("TMUX") != ""
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// InsideTmux reports whether we're running inside a tmux client (so jumps use
// switch-client rather than attach-session).
func (c *Client) InsideTmux() bool { return c.insideTmux() }
