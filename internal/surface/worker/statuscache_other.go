//go:build !unix

package worker

import "github.com/cameronsjo/forgectl/internal/privdir"

// The cache rests on openat and O_NOFOLLOW, which this platform does not
// offer, so it refuses; `surface status` then reads without a cache.

// Read refuses.
func (StatusCache) Read(string) ([]byte, error) { return nil, privdir.ErrUnsupported }

// Write refuses.
func (StatusCache) Write(string, []byte) error { return privdir.ErrUnsupported }

// Entries refuses.
func (StatusCache) Entries() ([]StatusCacheEntry, error) { return nil, privdir.ErrUnsupported }

// Remove refuses.
func (StatusCache) Remove(StatusCacheEntry) error { return privdir.ErrUnsupported }
