package ghostty

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// fallbackBin is where cmux bundles its own Ghostty build when "ghostty"
// isn't on PATH — verified live against a running cmux install (cmux
// launches Ghostty for terminal surfaces; see forgectl#8). It's a legitimate
// second home for the binary, not a guess: cmux ships a real `ghostty`
// executable at this path regardless of whether Ghostty.app itself has ever
// linked one onto PATH.
const fallbackBin = "/Applications/cmux.app/Contents/Resources/bin/ghostty"

// Client wraps the `ghostty +<action>` CLI behind the exec.Runner seam.
// Ghostty exposes no IPC/socket and no AppleScript — this CLI plus the
// config file at ~/.config/ghostty/config is the only control surface
// (forgectl#7).
type Client struct {
	run exec.Runner
	bin string

	// lookPath resolves a binary name to a PATH entry, and stat checks the
	// cmux fallback path — both injectable (lookPath mirrors internal/tmux's
	// Client) so tests can exercise every resolution branch without
	// depending on what's actually installed. Default to os/exec.LookPath
	// and os.Stat.
	lookPath func(string) (string, error)
	stat     func(string) (os.FileInfo, error)
}

// Option configures a Client at construction.
type Option func(*Client)

// WithBin overrides the resolved ghostty binary directly, skipping PATH/
// fallback resolution entirely — used in tests.
func WithBin(bin string) Option {
	return func(c *Client) { c.bin = bin }
}

// WithLookPath overrides PATH resolution — used in tests to exercise the
// fallback-path branch without depending on what's actually installed.
func WithLookPath(fn func(string) (string, error)) Option {
	return func(c *Client) { c.lookPath = fn }
}

// WithStat overrides the fallback-path existence check — used in tests to
// exercise the "neither resolves" branch deterministically.
func WithStat(fn func(string) (os.FileInfo, error)) Option {
	return func(c *Client) { c.stat = fn }
}

// New builds a Client, resolving the ghostty binary: PATH first, then the
// cmux-bundled fallback. If neither resolves, bin stays empty and every
// Client method returns a clear "not found" error rather than letting
// exec.Runner surface a generic not-found error for a bare "ghostty".
func New(run exec.Runner, opts ...Option) *Client {
	c := &Client{run: run, lookPath: osexec.LookPath, stat: os.Stat}
	for _, opt := range opts {
		opt(c)
	}
	if c.bin == "" {
		c.bin = c.resolveBin()
	}
	return c
}

// resolveBin implements the PATH-then-fallback resolution order.
func (c *Client) resolveBin() string {
	if p, err := c.lookPath("ghostty"); err == nil {
		return p
	}
	if _, err := c.stat(fallbackBin); err == nil {
		return fallbackBin
	}
	return ""
}

// checkAvailable reports a clear error when no ghostty binary resolved,
// rather than letting the underlying exec.Runner call fail with a generic
// "command not found" for whatever bin ended up empty.
func (c *Client) checkAvailable() error {
	if c.bin == "" {
		return fmt.Errorf("ghostty not found on PATH or at %s", fallbackBin)
	}
	return nil
}

// Themes returns every theme ghostty knows about — Cameron's custom ones
// from ~/.config/ghostty/themes/ and the built-ins bundled in the app
// resources — with the currently active theme marked (forgectl#7's
// acceptance criterion: "themes lists all themes and marks the active one").
func (c *Client) Themes(ctx context.Context) ([]Theme, error) {
	if err := c.checkAvailable(); err != nil {
		return nil, err
	}
	out, err := c.run.Run(ctx, c.bin, "+list-themes", "--plain")
	if err != nil {
		return nil, fmt.Errorf("list themes: %w", err)
	}
	themes := ParseThemes(out)

	cfg, err := c.Config(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve active theme: %w", err)
	}
	active := cfg.Theme()
	for i := range themes {
		themes[i].Active = themes[i].Name == active
	}
	return themes, nil
}

// Keybinds returns every active ghostty keybinding, parsed from
// `+list-keybinds` — not hard-coded, forgectl#7's other acceptance
// criterion.
func (c *Client) Keybinds(ctx context.Context) ([]Keybind, error) {
	if err := c.checkAvailable(); err != nil {
		return nil, err
	}
	out, err := c.run.Run(ctx, c.bin, "+list-keybinds")
	if err != nil {
		return nil, fmt.Errorf("list keybinds: %w", err)
	}
	return ParseKeybinds(out), nil
}

// Config returns ghostty's resolved configuration (`+show-config`). It
// exists for Themes' internal use (finding the active theme) — ConfigKey is
// deliberately unclaimed in the module manifest, so this is not a
// `forgectl config` fold-in (forgectl#7 scope: deferred).
func (c *Client) Config(ctx context.Context) (Config, error) {
	if err := c.checkAvailable(); err != nil {
		return Config{}, err
	}
	out, err := c.run.Run(ctx, c.bin, "+show-config", "--default=false", "--docs=false")
	if err != nil {
		return Config{}, fmt.Errorf("show config: %w", err)
	}
	return ParseConfig(out), nil
}
