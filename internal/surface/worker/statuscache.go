package worker

import (
	"errors"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
)

// `surface status` caches only what cannot change for a head commit: a
// PR's changed-file list and modes. One file per head,
// status-cache-<head>.json, sits beside the queue in the surface directory
// and goes through the same pinned directory and verified open. `surface
// merge` and the drain's merge never read it.

// ErrBadCacheKey reports a cache key that is not a full lowercase commit id.
var ErrBadCacheKey = errors.New("worker: a status cache key is a 40-character lowercase commit id")

// StatusCache is the status cache under one state base.
type StatusCache struct {
	stateBase string
}

// OpenStatusCache returns the cache in $XDG_STATE_HOME/forgectl/surface.
func OpenStatusCache() (StatusCache, error) {
	base, err := config.LaunchUsageBase()
	if err != nil {
		return StatusCache{}, err
	}
	return StatusCache{stateBase: base}, nil
}

// StatusCacheEntry is one cache file: its head and modification time.
type StatusCacheEntry struct {
	Head    string
	ModTime time.Time
}

// ErrStatusCacheChanged reports an entry rewritten since it was listed.
var ErrStatusCacheChanged = errors.New("worker: the status cache entry changed since it was listed")

// statusCacheHead returns the head a cache file name names.
func statusCacheHead(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, "status-cache-")
	if !ok {
		return "", false
	}
	head, ok := strings.CutSuffix(rest, ".json")
	if !ok {
		return "", false
	}
	if _, err := statusCacheName(head); err != nil {
		return "", false
	}
	return head, true
}

func statusCacheName(head string) (string, error) {
	if len(head) != 40 {
		return "", ErrBadCacheKey
	}
	for _, r := range head {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", ErrBadCacheKey
		}
	}
	return "status-cache-" + head + ".json", nil
}
