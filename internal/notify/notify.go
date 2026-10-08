// Package notify posts a desktop notification on macOS through osascript. It
// knows nothing of forgectl's own types: a caller hands it a title and a body
// as plain strings, and on any other platform Notify is a no-op.
//
// ARGV-PASSING IS LOAD-BEARING. The title and body arrive as osascript
// positional arguments (`on run argv`), and the AppleScript source is the
// constant notifyScript. Neither value is ever spliced into the script, so a
// body carrying quotes, `& do shell script "…"`, or `$(...)` stays a string
// and can never be read as AppleScript or shell syntax. The script must never
// be built with fmt.Sprintf or concatenation — that one change would turn
// every caller-supplied string into code. internal/clip's file_darwin.go
// follows the same rule for the same reason.
package notify

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// notifyScript shows argv[1] as the notification body under argv[0] as its
// title. Both arrive as positional arguments; see the package doc.
const notifyScript = "on run argv\n\tdisplay notification (item 2 of argv) with title (item 1 of argv)\nend run"

// Caps on the two fields, in runes of termsafe.SafeLineMax output. A
// notification banner shows far less than either; the caps bound what a
// hostile or runaway value can put on the osascript command line.
const (
	maxTitleRunes = 64
	maxBodyRunes  = 256
)

// defaultTimeout bounds one osascript invocation. A notification is a
// courtesy: a hung osascript must not hold up the caller that asked for it.
const defaultTimeout = 5 * time.Second

// Client posts notifications through exec.Runner (never os/exec directly).
type Client struct {
	run exec.Runner

	// goos is runtime.GOOS by default; overridable via WithGOOS so tests can
	// exercise both the darwin path and the non-darwin no-op on any host.
	goos string

	// timeout bounds each osascript call.
	timeout time.Duration
}

// Option configures a Client at construction.
type Option func(*Client)

// WithGOOS overrides the platform Notify checks against — a test-only hook,
// as in internal/clip.
func WithGOOS(goos string) Option {
	return func(c *Client) { c.goos = goos }
}

// New builds a Client over the given Runner.
func New(run exec.Runner, opts ...Option) *Client {
	c := &Client{
		run:     run,
		goos:    runtime.GOOS,
		timeout: defaultTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Supported reports whether Notify posts anything on this platform: only
// darwin does. Elsewhere Notify is a no-op that returns nil, so a caller that
// wants to know whether anyone was told asks this first.
func (c *Client) Supported() bool { return c.goos == "darwin" }

// Notify posts one desktop notification. On any platform but darwin it
// returns nil without spawning anything. Both strings are rendered inert and
// capped with termsafe.SafeLineMax before they reach osascript, and the
// returned error is sanitized, because *exec.CommandError echoes its argv
// verbatim.
func (c *Client) Notify(ctx context.Context, title, body string) error {
	if c.goos != "darwin" {
		return nil
	}
	title = termsafe.SafeLineMax(title, maxTitleRunes)
	body = termsafe.SafeLineMax(body, maxBodyRunes)

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if _, err := c.run.Run(ctx, "osascript", "-e", notifyScript, "--", title, body); err != nil {
		return fmt.Errorf("osascript notification: %w", termsafe.Error(err))
	}
	return nil
}
