package net

// Test plan for net.go
//
// Client.Status / Client.Reachable / Client.Refresh (Classification: ops layer)
//   [x] Happy: fresh cache (age < ttl) is returned as-is; dialFunc is not called
//   [x] Happy: missing cache file (cache miss) probes and writes a fresh cache entry
//   [x] Happy: expired cache (age >= ttl) re-probes instead of trusting the stale entry
//   [x] Happy: successful probe (dialFunc returns a conn, no error) reports Reachable=true
//   [x] Happy: failed probe (dialFunc returns an error) reports Reachable=false, err=nil
//     (an unreachable network is a valid, non-error outcome)
//   [x] Boundary: corrupt cache file (invalid JSON) is treated as a miss, not fatal —
//     probes and overwrites the cache rather than returning an error
//   [x] Happy: Refresh bypasses a still-fresh cache and forces a new probe
//   [x] Boundary: an already-canceled context short-circuits before probing and
//     returns the context error

import (
	"context"
	"encoding/json"
	"errors"
	stdnet "net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// dialCounter wraps a canned dial result and counts invocations so tests can
// assert whether the cache path or the probe path fired.
type dialCounter struct {
	calls int
	conn  stdnet.Conn
	err   error
}

func (d *dialCounter) dial(_, _ string, _ time.Duration) (stdnet.Conn, error) {
	d.calls++
	return d.conn, d.err
}

// newTestClient builds a Client wired for tests: a temp-dir cache path, a
// fixed clock, and the given dial behavior.
func newTestClient(t *testing.T, now time.Time, dialer *dialCounter) *Client {
	t.Helper()
	cachePath := filepath.Join(t.TempDir(), "net-cache.json")
	return New(&exec.FakeRunner{},
		WithCachePath(cachePath),
		WithNow(func() time.Time { return now }),
		WithDialFunc(dialer.dial),
	)
}

func writeCacheFixture(t *testing.T, path string, entry cacheEntry) {
	t.Helper()
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal cache fixture: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir cache fixture dir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write cache fixture: %v", err)
	}
}

func TestReachable_FreshCache_DoesNotDial(t *testing.T) {
	now := time.Now()
	dialer := &dialCounter{}
	c := newTestClient(t, now, dialer)
	writeCacheFixture(t, c.cachePath, cacheEntry{Reachable: true, CheckedAt: now.Add(-5 * time.Second)})

	got, err := c.Reachable(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Errorf("Reachable = false, want true (from cache)")
	}
	if dialer.calls != 0 {
		t.Errorf("dialFunc called %d times, want 0 (fresh cache hit)", dialer.calls)
	}
}

func TestReachable_MissingCache_ProbesAndWritesCache(t *testing.T) {
	now := time.Now()
	dialer := &dialCounter{}
	c := newTestClient(t, now, dialer)
	// No fixture written — cachePath doesn't exist yet.

	got, err := c.Reachable(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Errorf("Reachable = false, want true (successful probe)")
	}
	if dialer.calls != 1 {
		t.Errorf("dialFunc called %d times, want 1 (cache miss)", dialer.calls)
	}

	entry, ok := readCache(c.cachePath)
	if !ok {
		t.Fatal("expected cache file to be written after a probe")
	}
	if !entry.Reachable {
		t.Errorf("written cache entry Reachable = false, want true")
	}
}

func TestReachable_ExpiredCache_ReProbes(t *testing.T) {
	now := time.Now()
	dialer := &dialCounter{}
	c := newTestClient(t, now, dialer)
	// Entry is older than the default 60s TTL.
	writeCacheFixture(t, c.cachePath, cacheEntry{Reachable: false, CheckedAt: now.Add(-90 * time.Second)})

	got, err := c.Reachable(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Errorf("Reachable = false, want true (expired cache must re-probe, not trust the stale false)")
	}
	if dialer.calls != 1 {
		t.Errorf("dialFunc called %d times, want 1 (expired cache)", dialer.calls)
	}
}

func TestReachable_FailedProbe_ReturnsFalseNotError(t *testing.T) {
	now := time.Now()
	dialer := &dialCounter{err: errors.New("dial tcp: connection refused")}
	c := newTestClient(t, now, dialer)

	got, err := c.Reachable(context.Background())
	if err != nil {
		t.Fatalf("an unreachable network must not surface as a Go error, got: %v", err)
	}
	if got {
		t.Errorf("Reachable = true, want false (dial failed)")
	}
	entry, ok := readCache(c.cachePath)
	if !ok {
		t.Fatal("expected cache file to be written even after a failed probe")
	}
	if entry.Reachable {
		t.Errorf("written cache entry Reachable = true, want false")
	}
}

func TestReachable_CorruptCache_TreatedAsMissNotFatal(t *testing.T) {
	now := time.Now()
	dialer := &dialCounter{}
	c := newTestClient(t, now, dialer)
	if err := os.MkdirAll(filepath.Dir(c.cachePath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(c.cachePath, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("write corrupt cache fixture: %v", err)
	}

	got, err := c.Reachable(context.Background())
	if err != nil {
		t.Fatalf("corrupt cache must not be fatal, got error: %v", err)
	}
	if !got {
		t.Errorf("Reachable = false, want true (corrupt cache should fall through to a probe)")
	}
	if dialer.calls != 1 {
		t.Errorf("dialFunc called %d times, want 1 (corrupt cache treated as miss)", dialer.calls)
	}
}

func TestRefresh_BypassesFreshCache_ForcesProbe(t *testing.T) {
	now := time.Now()
	dialer := &dialCounter{}
	c := newTestClient(t, now, dialer)
	writeCacheFixture(t, c.cachePath, cacheEntry{Reachable: false, CheckedAt: now.Add(-1 * time.Second)})

	status, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !status.Reachable {
		t.Errorf("Refresh Reachable = false, want true (must re-probe, ignoring the fresh cached false)")
	}
	if dialer.calls != 1 {
		t.Errorf("dialFunc called %d times, want 1 (--refresh forces a probe)", dialer.calls)
	}
}

func TestStatus_CanceledContext_ReturnsErrorWithoutDialing(t *testing.T) {
	now := time.Now()
	dialer := &dialCounter{}
	c := newTestClient(t, now, dialer)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Status(ctx)
	if err == nil {
		t.Fatal("expected an error from an already-canceled context")
	}
	if dialer.calls != 0 {
		t.Errorf("dialFunc called %d times, want 0 (canceled context must short-circuit)", dialer.calls)
	}
}
