// Package net is the ops layer for `forgectl net`: a cached internal-network
// reachability probe. It knows nothing of Cobra — that decoupling is the
// house pattern (see internal/tmux, internal/projects).
//
// Unlike tmux/projects, the probe itself doesn't shell out: it dials TCP
// directly via the standard library rather than through the exec.Runner
// seam. Client still holds a Runner field for consistency with the rest of
// the codebase (and in case a future probe needs it), but Reachable/Status/
// Refresh never call it.
package net

import (
	"context"
	"log/slog"
	stdnet "net"
	"strconv"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
)

// Built-in defaults applied when the [net] config section (or an individual
// field within it) is unset.
const (
	defaultProbeHost  = "1.1.1.1"
	defaultProbePort  = 443
	defaultTTLSeconds = 60
	defaultTimeoutMs  = 1000
)

// Status is the outcome of a reachability check: the answer plus when it was
// determined — from a cache hit or a fresh probe. This is what the CLI
// renders and the --json shape reports.
type Status struct {
	Reachable bool
	CheckedAt time.Time
}

// dialFunc mirrors the stdlib net.DialTimeout signature — the seam tests
// inject to fake a reachable/unreachable network without a live socket.
type dialFunc func(network, address string, timeout time.Duration) (stdnet.Conn, error)

// Client probes internal-network reachability and caches the answer on disk
// (config.NetCachePath()) for ttl, so repeated calls within that window don't
// re-dial.
type Client struct {
	run exec.Runner

	host    string
	port    int
	ttl     time.Duration
	timeout time.Duration

	cachePath string
	dial      dialFunc
	now       func() time.Time
}

// Option configures a Client at construction.
type Option func(*Client)

// WithNetConfig applies the [net] config section, filling in any field left
// zero with New's built-in default rather than overwriting it.
func WithNetConfig(nc config.NetConfig) Option {
	return func(c *Client) {
		if nc.ProbeHost != "" {
			c.host = nc.ProbeHost
		}
		if nc.ProbePort != 0 {
			c.port = nc.ProbePort
		}
		if nc.TTLSeconds != 0 {
			c.ttl = time.Duration(nc.TTLSeconds) * time.Second
		}
		if nc.TimeoutMs != 0 {
			c.timeout = time.Duration(nc.TimeoutMs) * time.Millisecond
		}
	}
}

// WithDialFunc overrides the dial implementation — used in tests to fake a
// reachable/unreachable network without a live socket.
func WithDialFunc(fn func(network, address string, timeout time.Duration) (stdnet.Conn, error)) Option {
	return func(c *Client) { c.dial = fn }
}

// WithNow overrides the clock used for cache freshness checks — used in
// tests so TTL expiry is deterministic.
func WithNow(fn func() time.Time) Option {
	return func(c *Client) { c.now = fn }
}

// WithCachePath overrides the on-disk cache location (default:
// config.NetCachePath()) — used in tests to point at a temp file.
func WithCachePath(path string) Option {
	return func(c *Client) { c.cachePath = path }
}

// New builds a Client over the given Runner (kept for house consistency; see
// the package doc).
func New(run exec.Runner, opts ...Option) *Client {
	c := &Client{
		run:     run,
		host:    defaultProbeHost,
		port:    defaultProbePort,
		ttl:     defaultTTLSeconds * time.Second,
		timeout: defaultTimeoutMs * time.Millisecond,
		dial: func(network, address string, timeout time.Duration) (stdnet.Conn, error) {
			return stdnet.DialTimeout(network, address, timeout)
		},
		now: time.Now,
	}
	if path, err := config.NetCachePath(); err == nil {
		c.cachePath = path
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Reachable reports whether the configured endpoint is reachable, cache-first
// (see Status). It is a convenience wrapper for callers that don't need the
// checked-at timestamp.
func (c *Client) Reachable(ctx context.Context) (bool, error) {
	status, err := c.Status(ctx)
	return status.Reachable, err
}

// Status returns the cached reachability answer if it's still fresh,
// otherwise probes and caches a new one. A corrupt or missing cache file is
// treated as a miss, not an error.
func (c *Client) Status(ctx context.Context) (Status, error) {
	return c.check(ctx, false)
}

// Refresh forces a fresh probe, bypassing any cached answer (--refresh).
func (c *Client) Refresh(ctx context.Context) (Status, error) {
	return c.check(ctx, true)
}

// check is the shared cache-then-probe path. forceProbe skips the cache read
// entirely (Refresh); otherwise a fresh cache entry short-circuits the probe.
func (c *Client) check(ctx context.Context, forceProbe bool) (Status, error) {
	if err := ctx.Err(); err != nil {
		return Status{}, err
	}

	if !forceProbe {
		if entry, ok := readCache(c.cachePath); ok {
			if age := c.now().Sub(entry.CheckedAt); age >= 0 && age < c.ttl {
				return Status{Reachable: entry.Reachable, CheckedAt: entry.CheckedAt}, nil
			}
		}
	}

	return c.probe(), nil
}

// probe dials the configured endpoint once, caches the answer, and returns
// it. A dial failure means "unreachable", not a Go error — the caller always
// gets a definite answer. This is the one place the package logs at more than
// debug level: the probe and its resulting decision.
func (c *Client) probe() Status {
	addr := stdnet.JoinHostPort(c.host, strconv.Itoa(c.port))
	slog.Debug("Preparing to probe network reachability.", "addr", addr, "timeout", c.timeout)

	// Deliberately a timeout-bounded DialTimeout, not a ctx-threaded DialContext:
	// the probe is a sub-second reachability check and ctx cancellation is caught
	// before the dial (check short-circuits on ctx.Err()). If a future pr/poll
	// consumer needs to abort an in-flight dial, swap dialFunc for a DialContext seam.
	conn, err := c.dial("tcp", addr, c.timeout)
	if conn != nil {
		_ = conn.Close()
	}

	status := Status{Reachable: err == nil, CheckedAt: c.now()}
	slog.Info("Probed network reachability.", "addr", addr, "reachable", status.Reachable)

	if err := writeCache(c.cachePath, cacheEntry{Reachable: status.Reachable, CheckedAt: status.CheckedAt}); err != nil {
		slog.Warn("Failed to write network reachability cache.", "path", c.cachePath, "error", err)
	}
	return status
}
