package clean

import (
	"context"
	"log/slog"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
)

// CacheKind identifies a package-manager cache target ScanCaches understands
// — issue #4's follow-on scope. Opt-in (never scanned unless the CLI's
// --caches flag is set): clearing a warm package-manager cache is slower and
// more disruptive than dep/build-dir reclaim (which only removes trivially
// regenerable output), so it doesn't ship as a default-on target.
type CacheKind string

const (
	CacheNpm  CacheKind = "npm"
	CachePnpm CacheKind = "pnpm"
	CachePip  CacheKind = "pip"
	CacheGo   CacheKind = "go"
	CacheBrew CacheKind = "brew"
)

// CacheItem is one package-manager cache location, sized and (optionally)
// pruned. Mirrors Item's Skipped/Err shape for the CLI's shared
// preview/confirm/apply pattern, but Path/Size come from the tool's OWN
// locate command plus dirSize — never from Scan, since these directories
// live outside --root entirely and aren't discovered by walking it.
type CacheItem struct {
	Kind CacheKind
	Path string // resolved cache directory; "" when the tool wasn't detected
	Size int64  // best-effort size estimate: dirSize(Path)

	// Skipped is true when the tool isn't installed, isn't on PATH, or its
	// own "where's my cache" query failed/returned nothing — never treated
	// as a hard error, since an unavailable tool is an everyday, expected
	// state on any given machine, not a forgectl failure.
	Skipped    bool
	SkipReason string

	// Applied is true only once the tool's own prune/clean command has run
	// successfully (Apply only). Err carries a failed prune attempt.
	Applied bool
	Err     error
}

// cacheProbe is how ScanCaches locates and PruneCaches reclaims one tool's
// cache — always through the tool's OWN command, never a raw filesystem
// delete, since a package manager's cache layout and invalidation rules are
// its own to manage, not forgectl's to reimplement.
type cacheProbe struct {
	kind CacheKind
	// locate runs a READ-ONLY "where's my cache" query. A failure (tool not
	// on PATH, command errors) or empty output means "not detected" (ok
	// false) — degrades to a skip, never propagated as a scan error, so one
	// missing tool never aborts the others.
	locate func(ctx context.Context, run exec.Runner) (dir string, ok bool)
	// prune runs the tool's own cache-clearing command.
	prune func(ctx context.Context, run exec.Runner) error
}

// defaultCacheProbes is the whole opt-in package-manager cache surface.
// Each entry's locate is the tool's own "where is my cache" command — never
// a hardcoded well-known path, since a user's environment (npm config,
// pnpm's content-addressable store location, GOCACHE, Homebrew's prefix)
// can relocate any of these.
var defaultCacheProbes = []cacheProbe{
	{
		kind: CacheNpm,
		locate: func(ctx context.Context, run exec.Runner) (string, bool) {
			return locateViaCommand(ctx, run, "npm", "config", "get", "cache")
		},
		prune: func(ctx context.Context, run exec.Runner) error {
			_, err := run.Run(ctx, "npm", "cache", "clean", "--force")
			return err
		},
	},
	{
		kind: CachePnpm,
		locate: func(ctx context.Context, run exec.Runner) (string, bool) {
			return locateViaCommand(ctx, run, "pnpm", "store", "path")
		},
		prune: func(ctx context.Context, run exec.Runner) error {
			_, err := run.Run(ctx, "pnpm", "store", "prune")
			return err
		},
	},
	{
		kind: CachePip,
		locate: func(ctx context.Context, run exec.Runner) (string, bool) {
			return locateViaCommand(ctx, run, "pip", "cache", "dir")
		},
		prune: func(ctx context.Context, run exec.Runner) error {
			_, err := run.Run(ctx, "pip", "cache", "purge")
			return err
		},
	},
	{
		kind: CacheGo,
		locate: func(ctx context.Context, run exec.Runner) (string, bool) {
			return locateViaCommand(ctx, run, "go", "env", "GOCACHE")
		},
		prune: func(ctx context.Context, run exec.Runner) error {
			_, err := run.Run(ctx, "go", "clean", "-cache")
			return err
		},
	},
	{
		kind: CacheBrew,
		locate: func(ctx context.Context, run exec.Runner) (string, bool) {
			return locateViaCommand(ctx, run, "brew", "--cache")
		},
		prune: func(ctx context.Context, run exec.Runner) error {
			_, err := run.Run(ctx, "brew", "cleanup", "-s")
			return err
		},
	},
}

// locateViaCommand runs a read-only "where's my cache" command and reports
// (dir, true) on non-empty trimmed output. A Run error or blank output is
// "not detected" (ok=false) rather than an error return — a missing tool
// degrades to a skip, exactly like Client.ApplyReport's fail-safe direction
// for an unconfirmable git status, but inverted: here the safe default is
// "skip a tool we can't positively identify", not "treat it as present".
func locateViaCommand(ctx context.Context, run exec.Runner, name string, args ...string) (string, bool) {
	out, err := run.Run(ctx, name, args...)
	if err != nil {
		return "", false
	}
	dir := strings.TrimSpace(out)
	return dir, dir != ""
}

// matchesCacheFilter reports whether kind passes kinds. An empty kinds slice
// matches every Kind — mirrors matchesTypeFilter in scan.go.
func matchesCacheFilter(kind CacheKind, kinds []CacheKind) bool {
	if len(kinds) == 0 {
		return true
	}
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// ScanCaches locates and sizes every configured package-manager cache. kinds
// filters which probes run; empty means every one of defaultCacheProbes. An
// undetected tool yields a Skipped item (with a reason) rather than being
// silently omitted, so the CLI can report why a cache didn't show up, same
// as a dirty-tree skip does for dep/build dirs.
func (c *Client) ScanCaches(ctx context.Context, kinds []CacheKind) []CacheItem {
	items := make([]CacheItem, 0, len(defaultCacheProbes))
	for _, probe := range defaultCacheProbes {
		if !matchesCacheFilter(probe.kind, kinds) {
			continue
		}
		dir, ok := probe.locate(ctx, c.run)
		if !ok {
			items = append(items, CacheItem{
				Kind:       probe.kind,
				Skipped:    true,
				SkipReason: "not detected (tool not on PATH, or its cache location is unset)",
			})
			continue
		}
		items = append(items, CacheItem{
			Kind: probe.kind,
			Path: dir,
			Size: dirSize(dir),
		})
	}
	return items
}

// PruneCaches runs each non-skipped item's own prune command, ISOLATED per
// tool — one tool failing to prune (a stale lock, a permissions issue, the
// binary crashing) never prevents the rest from running. Only meaningful
// behind the CLI's --apply + confirmation gate; ScanCaches alone is the
// dry-run path and never mutates anything.
func (c *Client) PruneCaches(ctx context.Context, items []CacheItem) []CacheItem {
	out := make([]CacheItem, len(items))
	copy(out, items)

	probesByKind := make(map[CacheKind]cacheProbe, len(defaultCacheProbes))
	for _, p := range defaultCacheProbes {
		probesByKind[p.kind] = p
	}

	for i := range out {
		if out[i].Skipped {
			continue
		}
		probe, ok := probesByKind[out[i].Kind]
		if !ok {
			continue
		}
		if err := probe.prune(ctx, c.run); err != nil {
			out[i].Err = err
			slog.Error("Failed to prune package-manager cache.", "kind", out[i].Kind, "path", out[i].Path, "error", err)
			continue
		}
		out[i].Applied = true
		slog.Info("Successfully pruned package-manager cache.", "kind", out[i].Kind, "path", out[i].Path, "size", out[i].Size)
	}
	return out
}
