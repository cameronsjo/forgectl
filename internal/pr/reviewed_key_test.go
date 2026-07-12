package pr

// Test plan for the string-key ReviewedStore methods (reviewed.go)
//
// MarkKey/IsReviewedKey/UnmarkKey/SyncKeys (Classification: state store)
//   [x] Happy: MarkKey → IsReviewedKey true at/before the mark, false for
//       later activity (the auto-un-dim comparison, identical to the Ref path)
//   [x] Happy: Ref methods and Key methods share one map (Mark via Ref is
//       visible through IsReviewedKey with ref.String())
//   [x] Happy: SyncKeys prunes keys absent from the open set, keeps the rest

import (
	"path/filepath"
	"testing"
	"time"
)

func TestReviewedStore_KeyMethods(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review-reviewed.json")
	markAt := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := LoadReviewed(path, WithNow(func() time.Time { return markAt }))

	key := "github.com/cameronsjo/forgectl#76"
	if err := store.MarkKey(key); err != nil {
		t.Fatalf("MarkKey: %v", err)
	}
	if !store.IsReviewedKey(key, markAt) {
		t.Error("key marked at activity time must read reviewed")
	}
	if store.IsReviewedKey(key, markAt.Add(time.Hour)) {
		t.Error("later activity must auto-un-dim the key")
	}
	if store.IsReviewedKey("github.com/cameronsjo/forgectl#77", markAt) {
		t.Error("unmarked key must read unreviewed")
	}

	if err := store.UnmarkKey(key); err != nil {
		t.Fatalf("UnmarkKey: %v", err)
	}
	if store.IsReviewedKey(key, markAt) {
		t.Error("unmarked key must read unreviewed after UnmarkKey")
	}
}

func TestReviewedStore_RefAndKeyShareOneMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-reviewed.json")
	markAt := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := LoadReviewed(path, WithNow(func() time.Time { return markAt }))

	ref := Ref{Owner: "cameronsjo", Repo: "forgectl", Number: 42}
	if err := store.Mark(ref); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if !store.IsReviewedKey(ref.String(), markAt) {
		t.Error("a Ref mark must be visible through the key path (one map)")
	}
}

func TestReviewedStore_SyncKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review-reviewed.json")
	store := LoadReviewed(path)

	keep := "github.com/cameronsjo/forgectl#1"
	drop := "github.com/cameronsjo/forgectl#2"
	if err := store.MarkKey(keep); err != nil {
		t.Fatalf("MarkKey: %v", err)
	}
	if err := store.MarkKey(drop); err != nil {
		t.Fatalf("MarkKey: %v", err)
	}
	if err := store.SyncKeys([]string{keep}); err != nil {
		t.Fatalf("SyncKeys: %v", err)
	}

	reloaded := LoadReviewed(path)
	if _, ok := reloaded.at[keep]; !ok {
		t.Error("open key must survive SyncKeys")
	}
	if _, ok := reloaded.at[drop]; ok {
		t.Error("closed key must be pruned by SyncKeys")
	}
}
