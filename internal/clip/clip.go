// Package clip is the ops layer for `forgectl y copy`/`forgectl y paste`: a
// thin wrapper around macOS's pbcopy/pbpaste. It knows nothing of Cobra —
// that decoupling is the house pattern (see internal/net, internal/docker).
//
// This is the clipboard half of issue #26 only; the shell-history-reading
// half is deferred (it depends on a shell-integration convention that
// doesn't exist in this repo yet).
package clip

import (
	"context"
	"errors"
	"log/slog"
	"runtime"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// errMacOSOnly is returned by Copy/Paste on any non-Darwin GOOS, so a Linux
// or Windows caller gets a clear message instead of a confusing
// `exec: "pbcopy": not found`.
var errMacOSOnly = errors.New("forgectl y: macOS only")

// Client copies to and pastes from the system clipboard via pbcopy/pbpaste,
// shelled through exec.Runner (never os/exec directly).
type Client struct {
	run exec.Runner

	// goos is runtime.GOOS by default; overridable via WithGOOS so tests can
	// exercise the non-Darwin guard path without needing to run on Linux/Windows.
	goos string
}

// Option configures a Client at construction.
type Option func(*Client)

// WithGOOS overrides the platform the guard checks against — a test-only
// hook so the non-Darwin guard path can be exercised on any host.
func WithGOOS(goos string) Option {
	return func(c *Client) { c.goos = goos }
}

// New builds a Client over the given Runner.
func New(run exec.Runner, opts ...Option) *Client {
	c := &Client{
		run:  run,
		goos: runtime.GOOS,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Copy writes s to the system clipboard via `pbcopy`.
func (c *Client) Copy(ctx context.Context, s string) error {
	if c.goos != "darwin" {
		return errMacOSOnly
	}

	slog.Debug("Preparing to copy to clipboard.", "bytes", len(s))
	if _, err := c.run.RunWithInput(ctx, s, "pbcopy"); err != nil {
		slog.Error("Failed to copy to clipboard.", "error", err)
		return err
	}
	slog.Info("Successfully copied to clipboard.", "bytes", len(s))
	return nil
}

// Paste returns the system clipboard's current contents via `pbpaste`.
func (c *Client) Paste(ctx context.Context) (string, error) {
	if c.goos != "darwin" {
		return "", errMacOSOnly
	}

	slog.Debug("Preparing to paste from clipboard.")
	out, err := c.run.Run(ctx, "pbpaste")
	if err != nil {
		slog.Error("Failed to paste from clipboard.", "error", err)
		return "", err
	}
	slog.Info("Successfully pasted from clipboard.", "bytes", len(out))
	return out, nil
}
