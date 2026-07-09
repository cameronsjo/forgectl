package pr

// Test plan for reviewed.go
//
// LoadReviewed (Classification: I/O boundary / corrupt-tolerant read)
//   [x] Happy: missing file → empty store, not fatal
//   [x] Unhappy: malformed JSON → empty store, not fatal
//   [x] Happy: a persisted store round-trips (Mark → Load → ReviewedAt)
//
// Mark / Unmark (Classification: I/O logic)
//   [x] Happy: Mark stamps now() and persists; ReviewedAt returns it
//   [x] Happy: Unmark clears the mark and persists
//   [x] Boundary: Unmark of an unmarked ref is a no-op (no file written)
//
// IsReviewed (Classification: pure comparison — the load-bearing invariant)
//   [x] Happy: reviewedAt == latestActivity → reviewed (>= is inclusive)
//   [x] Happy: reviewedAt after latestActivity → reviewed
//   [x] Boundary: latestActivity moves past reviewedAt → auto-un-dims
//   [x] Unhappy: never-marked ref → not reviewed
//
// Sync (Classification: data transformer + I/O)
//   [x] Happy: prunes entries not in the open set, keeps open ones
//   [x] Boundary: no change → no write

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testRef(n int) Ref { return Ref{Owner: "cameronsjo", Repo: "forgectl", Number: n} }

func TestLoadReviewed_MissingFile_EmptyNotFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	s := LoadReviewed(path)
	if _, ok := s.ReviewedAt(testRef(1)); ok {
		t.Errorf("missing file: want empty store, got a mark")
	}
}

func TestLoadReviewed_MalformedJSON_EmptyNotFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-reviewed.json")
	if err := os.WriteFile(path, []byte("this is { not json"), 0o600); err != nil {
		t.Fatalf("seed malformed file: %v", err)
	}
	s := LoadReviewed(path)
	if _, ok := s.ReviewedAt(testRef(1)); ok {
		t.Errorf("malformed file: want empty store, got a mark")
	}
	// The store must still be usable — a Mark should succeed and overwrite.
	if err := s.Mark(testRef(1)); err != nil {
		t.Errorf("Mark after malformed load: %v", err)
	}
}

func TestReviewedStore_MarkUnmarkRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-reviewed.json")
	stamp := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	s := LoadReviewed(path, WithNow(func() time.Time { return stamp }))

	if err := s.Mark(testRef(42)); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	// Reload from disk — the mark must survive.
	reloaded := LoadReviewed(path)
	at, ok := reloaded.ReviewedAt(testRef(42))
	if !ok {
		t.Fatalf("reloaded store missing the mark")
	}
	if !at.Equal(stamp) {
		t.Errorf("reloaded reviewedAt = %v, want %v", at, stamp)
	}

	// Unmark and reload — the mark must be gone.
	if err := reloaded.Unmark(testRef(42)); err != nil {
		t.Fatalf("Unmark: %v", err)
	}
	if _, ok := LoadReviewed(path).ReviewedAt(testRef(42)); ok {
		t.Errorf("after Unmark: mark still present on disk")
	}
}

func TestReviewedStore_UnmarkUnmarked_NoWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-reviewed.json")
	s := LoadReviewed(path)
	if err := s.Unmark(testRef(7)); err != nil {
		t.Fatalf("Unmark of unmarked ref: %v", err)
	}
	// No mark existed → nothing persisted → no file created.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("Unmark of unmarked ref wrote a file (err=%v)", err)
	}
}

func TestReviewedStore_IsReviewed_AutoUndimsOnFreshActivity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-reviewed.json")
	reviewedAt := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	s := LoadReviewed(path, WithNow(func() time.Time { return reviewedAt }))
	if err := s.Mark(testRef(42)); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	ref := testRef(42)
	cases := []struct {
		name     string
		activity time.Time
		want     bool
	}{
		{"activity before mark → reviewed", reviewedAt.Add(-time.Hour), true},
		{"activity equals mark → reviewed (inclusive)", reviewedAt, true},
		{"activity after mark → auto-un-dimmed", reviewedAt.Add(time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.IsReviewed(ref, tc.activity); got != tc.want {
				t.Errorf("IsReviewed(activity=%v) = %v, want %v", tc.activity, got, tc.want)
			}
		})
	}

	// A never-marked ref is never reviewed, regardless of activity.
	if s.IsReviewed(testRef(99), reviewedAt.Add(-time.Hour)) {
		t.Errorf("never-marked ref reported reviewed")
	}
}

func TestReviewedStore_Sync_PrunesClosedRefs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-reviewed.json")
	s := LoadReviewed(path, WithNow(func() time.Time { return time.Unix(0, 0) }))
	for _, n := range []int{1, 2, 3} {
		if err := s.Mark(testRef(n)); err != nil {
			t.Fatalf("Mark #%d: %v", n, err)
		}
	}

	// Only #2 remains open — #1 and #3 should be pruned.
	if err := s.Sync([]Ref{testRef(2)}); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	reloaded := LoadReviewed(path)
	if _, ok := reloaded.ReviewedAt(testRef(2)); !ok {
		t.Errorf("Sync pruned an open ref (#2)")
	}
	for _, n := range []int{1, 3} {
		if _, ok := reloaded.ReviewedAt(testRef(n)); ok {
			t.Errorf("Sync kept a closed ref (#%d)", n)
		}
	}
}

func TestReviewedStore_Sync_NoChange_NoWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-reviewed.json")
	s := LoadReviewed(path)
	// Empty store, empty open set → nothing to prune → no file written.
	if err := s.Sync(nil); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("no-op Sync wrote a file (err=%v)", err)
	}
}
