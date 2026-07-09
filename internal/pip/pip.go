package pip

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// Client edits a pip.conf file through the comment-preserving inifile model
// (see inifile.go): load -> mutate -> save, never a destructive rewrite.
type Client struct {
	run  exec.Runner
	path string
}

// Option configures a Client at construction.
type Option func(*Client)

// WithConfigPath overrides the pip.conf path Client reads/writes — used in
// tests to point at a temp file, and by callers who already know pip's
// effective config location (e.g. a discovered venv-local pip.conf).
func WithConfigPath(path string) Option {
	return func(c *Client) { c.path = path }
}

// New builds a Client over the given Runner. The Runner is kept for house
// consistency (New(run exec.Runner, ...) across every ops package — see
// internal/tmux, internal/net) and as a seam a future option could use to
// shell out to `pip config debug`/`pip config list -v` for a pip-reported
// path; Client's own methods never call it today. The default path is
// resolved via os.UserConfigDir(), the same OS-appropriate base pip itself
// uses via platformdirs (XDG_CONFIG_HOME, ~/Library/Application Support on
// macOS, ~/.config on Linux, %APPDATA% on Windows) — not a single hardcoded
// path. Override with WithConfigPath for anything more specific.
func New(run exec.Runner, opts ...Option) *Client {
	c := &Client{run: run, path: defaultConfigPath()}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// defaultConfigPath resolves the common pip.conf/pip.ini location for the
// current OS. Empty when os.UserConfigDir() itself fails (e.g. $HOME unset);
// callers should treat that as "must supply WithConfigPath".
func defaultConfigPath() string {
	if runtime.GOOS == "windows" {
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			return filepath.Join(appdata, "pip", "pip.ini")
		}
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "pip", "pip.conf")
}

// Path returns the pip.conf path this Client reads/writes.
func (c *Client) Path() string { return c.path }

// WithPath returns a copy of c pointed at a different pip.conf path, leaving
// c itself untouched. Used by the CLI's --path flag to override the default
// per-invocation without reconstructing the Runner. An empty path is a no-op
// (returns c unchanged) so callers can pass a possibly-unset flag value
// directly.
func (c *Client) WithPath(path string) *Client {
	if path == "" {
		return c
	}
	clone := *c
	clone.path = path
	return &clone
}

// Read loads and re-serializes pip.conf — a normalized (parse-then-render,
// byte-identical per the round-trip guarantee) view of the file's current
// content. A missing pip.conf yields empty bytes, not an error.
func (c *Client) Read(_ context.Context) ([]byte, error) {
	f, err := c.load()
	if err != nil {
		return nil, err
	}
	return f.Serialize(), nil
}

// Remove comments out every [section] key entry (see File.Remove) and
// persists the result. Returns the number of entries removed; 0 means no
// match was found and pip.conf was left untouched on disk.
func (c *Client) Remove(_ context.Context, section, key string) (int, error) {
	slog.Debug("Preparing to remove pip.conf entry.", "path", c.path, "section", section, "key", key)
	f, err := c.load()
	if err != nil {
		slog.Error("Failed to load pip.conf.", "path", c.path, "error", err)
		return 0, err
	}

	n := f.Remove(section, key)
	if n == 0 {
		slog.Debug("No matching pip.conf entries to remove.", "path", c.path, "section", section, "key", key)
		return 0, nil
	}

	if err := c.save(f); err != nil {
		slog.Error("Failed to save pip.conf after remove.", "path", c.path, "error", err)
		return 0, err
	}
	slog.Info("Successfully removed pip.conf entries.", "path", c.path, "section", section, "key", key, "count", n)
	return n, nil
}

// Restore un-comments every entry a prior Remove tagged (see File.Restore)
// and persists the result. Returns the number of lines restored; 0 means
// there was nothing to restore and pip.conf was left untouched on disk.
func (c *Client) Restore(_ context.Context) (int, error) {
	slog.Debug("Preparing to restore pip.conf entries.", "path", c.path)
	f, err := c.load()
	if err != nil {
		slog.Error("Failed to load pip.conf.", "path", c.path, "error", err)
		return 0, err
	}

	n := f.Restore()
	if n == 0 {
		slog.Debug("No removed pip.conf entries to restore.", "path", c.path)
		return 0, nil
	}

	if err := c.save(f); err != nil {
		slog.Error("Failed to save pip.conf after restore.", "path", c.path, "error", err)
		return 0, err
	}
	slog.Info("Successfully restored pip.conf entries.", "path", c.path, "count", n)
	return n, nil
}

// load reads and parses c.path. A missing file is not an error — it yields
// an empty File (mirrors config.Load's "missing file is defaults" contract).
func (c *Client) load() (*File, error) {
	data, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Debug("pip.conf not found; starting from an empty file.", "path", c.path)
			return NewFile(), nil
		}
		return nil, fmt.Errorf("read %s: %w", c.path, err)
	}
	return Parse(data), nil
}

// save serializes f and writes it to c.path, creating the parent directory
// if needed.
func (c *Client) save(f *File) error {
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if err := os.WriteFile(c.path, f.Serialize(), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", c.path, err)
	}
	return nil
}
