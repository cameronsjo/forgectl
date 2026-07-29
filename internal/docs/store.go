package docs

import "sync/atomic"

// Store holds the Index that request handlers should read, behind an atomic
// pointer so the live-reload Watcher can publish a rebuilt index without
// locking readers out and without any handler observing a half-built one.
//
// A plain *Index passed to NewHandler was correct for PR1, where the index was
// immutable for the process's lifetime. Live reload makes it mutable, and a
// mutex around it would be the wrong tool: an index rebuild walks the whole
// tree (slow, and it can fail), while reads are frequent and must never block
// on one. Pointer-swapping a fully-built replacement gives readers a stable
// snapshot for the duration of a request at zero read cost.
//
// What makes the swap safe is a property PR1 already established rather than
// anything added here: Index's accessors (List, Roots) return COPIES, so a
// handler that has loaded a *Index cannot observe it mutate underneath itself
// — the old index simply stays alive until the last handler holding it
// returns, and is then collected. Nothing mutates an Index in place; a change
// on disk produces a new one.
type Store struct {
	current atomic.Pointer[Index]
}

// NewStore wraps an initial Index. idx must be non-nil — a docs server with
// no index is a programming error, not a runtime state to tolerate.
func NewStore(idx *Index) *Store {
	s := &Store{}
	s.current.Store(idx)
	return s
}

// Current returns the index a handler should serve this request from. Callers
// SHOULD load it exactly once per request and reuse that pointer, so a swap
// mid-request cannot make one response reflect two different trees (e.g. a
// sidenav built from the new index around a doc resolved against the old).
func (s *Store) Current() *Index {
	return s.current.Load()
}

// Swap installs a rebuilt index for subsequent requests. In-flight requests
// keep serving from whatever they already loaded.
func (s *Store) Swap(idx *Index) {
	s.current.Store(idx)
}
