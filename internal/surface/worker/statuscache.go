package worker

import (
	"errors"

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
