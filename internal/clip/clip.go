// Package clip is the ops layer for `forgectl y copy`/`forgectl y paste`: a
// thin wrapper around macOS's pbcopy/pbpaste. It knows nothing of Cobra —
// that decoupling is the house pattern (see internal/net, internal/docker).
//
// This is the clipboard half of issue #26; the shell-history half lives in
// internal/history, which reads $HISTFILE directly rather than through a
// shell shim.
package clip

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// errMacOSOnly is returned by Copy/Paste on any non-Darwin GOOS, so a Linux
// or Windows caller gets a clear message instead of a confusing
// `exec: "pbcopy": not found`.
var errMacOSOnly = errors.New("forgectl y: macOS only")

// errUnsupportedImageExt is returned by CopyImage for an extension with no
// known AppleScript image class mapping.
var errUnsupportedImageExt = errors.New("forgectl y: unsupported image extension")

// imageClassForExt maps a file extension to the AppleScript image class
// osascript's `read ... as «class ...»` needs to decode it. Extended here as
// new formats come up; unmapped extensions are a clear, actionable error
// rather than a confusing osascript failure.
var imageClassForExt = map[string]string{
	".png":  "PNGf",
	".tif":  "TIFF",
	".tiff": "TIFF",
	".jpg":  "JPEG",
	".jpeg": "JPEG",
	".gif":  "GIFf",
}

// Client copies to and pastes from the system clipboard via pbcopy/pbpaste,
// shelled through exec.Runner (never os/exec directly).
type Client struct {
	run exec.Runner

	// goos is runtime.GOOS by default; overridable via WithGOOS so tests can
	// exercise the non-Darwin guard path without needing to run on Linux/Windows.
	goos string

	// sensitive suppresses the "bytes" length field from Copy/Paste's log
	// lines — see WithSensitive.
	sensitive bool
}

// Option configures a Client at construction.
type Option func(*Client)

// WithGOOS overrides the platform the guard checks against — a test-only
// hook so the non-Darwin guard path can be exercised on any host.
func WithGOOS(goos string) Option {
	return func(c *Client) { c.goos = goos }
}

// WithSensitive suppresses the byte-length ("bytes", N) field from Copy and
// Paste's success/preparing log lines. Length is itself signal about a
// secret (a JWT and a 4-digit PIN log distinguishably even with the value
// masked), so a caller handling secret values opts into omitting it — the
// log line itself is kept, only the length is dropped. Additive and
// opt-in: a Client built without this option keeps its current logging
// unchanged (the y module's existing behavior doesn't move).
func WithSensitive() Option {
	return func(c *Client) { c.sensitive = true }
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

	if c.sensitive {
		slog.Debug("Preparing to copy to clipboard.")
	} else {
		slog.Debug("Preparing to copy to clipboard.", "bytes", len(s))
	}
	if _, err := c.run.RunWithInput(ctx, s, "pbcopy"); err != nil {
		slog.Error("Failed to copy to clipboard.", "error", err)
		return err
	}
	if c.sensitive {
		slog.Info("Successfully copied to clipboard.")
	} else {
		slog.Info("Successfully copied to clipboard.", "bytes", len(s))
	}
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
	if c.sensitive {
		slog.Info("Successfully pasted from clipboard.")
	} else {
		slog.Info("Successfully pasted from clipboard.", "bytes", len(out))
	}
	return out, nil
}

// CopyFile puts a POSIX file reference for path on the pasteboard (macOS
// `public.file-url`), via osascript — pbcopy carries only text, so it has no
// route to this type. Pasting into Finder, Mail, or a chat window attaches
// the file rather than dumping its path as a string. macOS only.
func (c *Client) CopyFile(ctx context.Context, path string) error {
	if c.goos != "darwin" {
		return errMacOSOnly
	}

	slog.Debug("Preparing to copy file reference to clipboard.", "path", path)
	if err := copyFileReference(ctx, c.run, path); err != nil {
		slog.Error("Failed to copy file reference to clipboard.", "error", err)
		return err
	}
	slog.Info("Successfully copied file reference to clipboard.")
	return nil
}

// CopyImage decodes the image at path and puts it on the pasteboard as image
// data (macOS `public.png`/`public.tiff`/`public.jpeg`/`public.gif`), via
// osascript — pbcopy carries only text, so it has no route to this type
// either. Pasting into Finder, Mail, or a chat window pastes a picture
// rather than a filename. The image class is chosen from path's extension;
// an unrecognized extension is a clear error rather than a silent no-op.
// macOS only.
func (c *Client) CopyImage(ctx context.Context, path string) error {
	if c.goos != "darwin" {
		return errMacOSOnly
	}

	ext := strings.ToLower(filepath.Ext(path))
	class, ok := imageClassForExt[ext]
	if !ok {
		return fmt.Errorf("%w: %q", errUnsupportedImageExt, ext)
	}

	slog.Debug("Preparing to copy image to clipboard.", "path", path, "class", class)
	if err := copyImageReference(ctx, c.run, path, class); err != nil {
		slog.Error("Failed to copy image to clipboard.", "error", err)
		return err
	}
	slog.Info("Successfully copied image to clipboard.")
	return nil
}
