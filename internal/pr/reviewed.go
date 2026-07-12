package pr

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// ReviewedStore is the local, offline reviewed-state authority for `forgectl
// pr`: it maps a PR's canonical "owner/repo#N" form (Ref.String) to the
// timestamp it was last marked reviewed.
//
// Timestamp-not-boolean is load-bearing: dimming and the picker's skip both
// derive from a single comparison — reviewedAt >= the PR's latest activity —
// so any newer activity (a push, comment, label, or review — whatever bumps
// the PR's updatedAt) auto-un-dims the PR with no separate un-dim logic. The
// on-disk shape is a plain JSON object mapping the
// breadcrumb form to an RFC3339 time; a missing, unreadable, or malformed file
// loads as an empty store, never an error (mirrors internal/net's cache).
type ReviewedStore struct {
	path string
	at   map[string]time.Time
	now  func() time.Time
}

// ReviewedOption configures a ReviewedStore at load time.
type ReviewedOption func(*ReviewedStore)

// WithNow overrides the clock used to stamp Mark — used in tests so the
// reviewedAt timestamp is deterministic (mirrors net.WithNow).
func WithNow(fn func() time.Time) ReviewedOption {
	return func(s *ReviewedStore) { s.now = fn }
}

// LoadReviewed reads the reviewed-state store at path. A missing, unreadable,
// or malformed file yields an empty store rather than an error — the same
// corrupt-tolerant model internal/net uses for its cache, so a hand-mangled
// file degrades to "nothing reviewed yet" instead of blinding the dashboards.
func LoadReviewed(path string, opts ...ReviewedOption) *ReviewedStore {
	s := &ReviewedStore{
		path: path,
		at:   make(map[string]time.Time),
		now:  time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s // missing / unreadable → empty
	}
	var at map[string]time.Time
	if err := json.Unmarshal(data, &at); err != nil {
		slog.Warn("Ignoring unreadable pr-reviewed store; starting empty.", "path", path, "error", err)
		return s // malformed → empty
	}
	if at != nil {
		s.at = at
	}
	slog.Debug("Successfully loaded reviewed store.", "path", path, "count", len(s.at))
	return s
}

// Mark stamps ref as reviewed at the current clock and persists the store.
func (s *ReviewedStore) Mark(ref Ref) error { return s.MarkKey(ref.String()) }

// MarkKey is Mark for a caller that keys entries by an arbitrary canonical
// string (internal/review's host-qualified "host/owner/repo#N" keys) rather
// than a Ref. The Ref methods delegate here — one write path, two key shapes.
func (s *ReviewedStore) MarkKey(key string) error {
	slog.Debug("Preparing to mark reviewed.", "key", key)
	s.at[key] = s.now()
	if err := s.persist(); err != nil {
		slog.Error("Failed to mark reviewed.", "key", key, "error", err)
		return err
	}
	slog.Info("Successfully marked reviewed.", "key", key)
	return nil
}

// Unmark clears ref's reviewed mark and persists. A ref that was never marked
// is a no-op — no write fires.
func (s *ReviewedStore) Unmark(ref Ref) error { return s.UnmarkKey(ref.String()) }

// UnmarkKey is Unmark for string-keyed callers (see MarkKey).
func (s *ReviewedStore) UnmarkKey(key string) error {
	slog.Debug("Preparing to unmark reviewed.", "key", key)
	if _, ok := s.at[key]; !ok {
		slog.Debug("Skipping unmark: entry was not marked reviewed.", "key", key)
		return nil
	}
	delete(s.at, key)
	if err := s.persist(); err != nil {
		slog.Error("Failed to unmark reviewed.", "key", key, "error", err)
		return err
	}
	slog.Info("Successfully unmarked reviewed.", "key", key)
	return nil
}

// IsReviewed reports whether ref was marked reviewed at or after its latest
// activity. A later latestActivity (any new activity that bumps the PR's
// updatedAt) makes a previously-marked PR read as unreviewed again — the
// auto-un-dim falls out of this comparison, so there is no separate un-dim path.
func (s *ReviewedStore) IsReviewed(ref Ref, latestActivity time.Time) bool {
	return s.IsReviewedKey(ref.String(), latestActivity)
}

// IsReviewedKey is IsReviewed for string-keyed callers (see MarkKey) — the
// identical timestamp comparison, so issues and PRs share the auto-un-dim
// semantics exactly.
func (s *ReviewedStore) IsReviewedKey(key string, latestActivity time.Time) bool {
	at, ok := s.at[key]
	if !ok {
		return false
	}
	return !at.Before(latestActivity)
}

// ReviewedAt returns the stored reviewed timestamp for ref, if any. It backs
// the CLI-layer "previously marked reviewed" note on an explicit `pr <ref>`
// launch — a note only, never a skip.
func (s *ReviewedStore) ReviewedAt(ref Ref) (time.Time, bool) {
	at, ok := s.at[ref.String()]
	return at, ok
}

// Sync prunes any stored entry whose ref is not in openRefs and persists when
// anything changed. It keeps the store from growing without bound as PRs close
// and merge — `pr reviewed sync` feeds it the current open set.
func (s *ReviewedStore) Sync(openRefs []Ref) error {
	keys := make([]string, len(openRefs))
	for i, r := range openRefs {
		keys[i] = r.String()
	}
	return s.SyncKeys(keys)
}

// SyncKeys is Sync for string-keyed callers (see MarkKey).
func (s *ReviewedStore) SyncKeys(openKeys []string) error {
	slog.Debug("Preparing to sync reviewed store.", "storeSize", len(s.at), "openCount", len(openKeys))
	open := make(map[string]bool, len(openKeys))
	for _, k := range openKeys {
		open[k] = true
	}
	changed := false
	pruned := 0
	for key := range s.at {
		if !open[key] {
			delete(s.at, key)
			changed = true
			pruned++
		}
	}
	if !changed {
		slog.Debug("Skipping sync: no changes to store.", "openCount", len(openKeys))
		return nil
	}
	if err := s.persist(); err != nil {
		slog.Error("Failed to sync reviewed store.", "openCount", len(openKeys), "pruned", pruned, "error", err)
		return err
	}
	slog.Info("Successfully synced reviewed store.", "openCount", len(openKeys), "pruned", pruned)
	return nil
}

// persist writes the store to disk, creating the parent dir as needed — the
// exact write shape internal/net uses (MkdirAll 0o700 → WriteFile 0o600).
func (s *ReviewedStore) persist() error {
	if s.path == "" {
		return errors.New("pr: reviewed store path unset")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(s.at)
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}
