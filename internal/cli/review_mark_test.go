package cli

// Test plan for review_mark.go
//
// mark/unmark (Classification: state-store verbs)
//   [x] Happy: mark normalizes owner/repo#N and the URL form to the SAME
//       host-qualified key; unmark round-trips
//   [x] Unhappy: an invalid ref is an error before any store write
//
// sync (Classification: guarded prune)
//   [x] Happy: prunes marks for items absent from the open set
//   [x] Invariant: refuses on a partial (noted) set; skips on an empty set

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/review"
)

// execReview runs a review subcommand against src/store and returns stdout+stderr.
func execReview(t *testing.T, src review.Source, reviewedPath string, args ...string) (string, string, error) {
	t.Helper()
	cmd := newReviewCmdForSource(src, reviewedPath)
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

func TestReviewMark_NormalizesBothForms(t *testing.T) {
	reviewedPath := filepath.Join(t.TempDir(), "review-reviewed.json")
	src := fakeReviewSource{}

	if _, _, err := execReview(t, src, reviewedPath, "mark", "cameronsjo/forgectl#76"); err != nil {
		t.Fatalf("mark slug form: %v", err)
	}
	if _, _, err := execReview(t, src, reviewedPath, "mark", "https://github.com/cameronsjo/forgectl/pull/77"); err != nil {
		t.Fatalf("mark URL form: %v", err)
	}

	store := pr.LoadReviewed(reviewedPath)
	now := time.Now().Add(-time.Hour)
	if !store.IsReviewedKey("github.com/cameronsjo/forgectl#76", now) {
		t.Error("slug-form mark must land under the host-qualified key")
	}
	if !store.IsReviewedKey("github.com/cameronsjo/forgectl#77", now) {
		t.Error("URL-form mark must land under the host-qualified key")
	}

	if _, _, err := execReview(t, src, reviewedPath, "unmark", "cameronsjo/forgectl#76"); err != nil {
		t.Fatalf("unmark: %v", err)
	}
	if pr.LoadReviewed(reviewedPath).IsReviewedKey("github.com/cameronsjo/forgectl#76", now) {
		t.Error("unmark must clear the key")
	}
}

func TestReviewMark_RejectsInvalidRef(t *testing.T) {
	reviewedPath := filepath.Join(t.TempDir(), "review-reviewed.json")
	if _, _, err := execReview(t, fakeReviewSource{}, reviewedPath, "mark", "-owner/repo#1"); err == nil {
		t.Error("invalid ref must be an error")
	}
}

func TestReviewSync_PrunesClosedItems(t *testing.T) {
	reviewedPath := filepath.Join(t.TempDir(), "review-reviewed.json")
	seedReviewedKey(t, reviewedPath, "github.com/cameronsjo/alpha#1", reviewTestTime)
	seedReviewedKey(t, reviewedPath, "github.com/cameronsjo/alpha#2", reviewTestTime)

	src := fakeReviewSource{items: []review.Item{reviewItem(review.KindIssue, "alpha", 1)}}
	if _, _, err := execReview(t, src, reviewedPath, "sync"); err != nil {
		t.Fatalf("sync: %v", err)
	}

	store := pr.LoadReviewed(reviewedPath)
	if !store.IsReviewedKey("github.com/cameronsjo/alpha#1", reviewTestTime) {
		t.Error("open item's mark must survive sync")
	}
	if store.IsReviewedKey("github.com/cameronsjo/alpha#2", reviewTestTime) {
		t.Error("closed item's mark must be pruned")
	}
}

func TestReviewSync_RefusesPartialAndEmpty(t *testing.T) {
	reviewedPath := filepath.Join(t.TempDir(), "review-reviewed.json")
	seedReviewedKey(t, reviewedPath, "github.com/cameronsjo/alpha#1", reviewTestTime)

	// Partial: a note means a query degraded or truncated → refuse to prune.
	partial := fakeReviewSource{
		items: []review.Item{reviewItem(review.KindIssue, "bravo", 9)},
		notes: []string{"issues(cameronsjo): gh: rate limited"},
	}
	if _, stderr, err := execReview(t, partial, reviewedPath, "sync"); err != nil {
		t.Fatalf("sync (partial): %v", err)
	} else if !contains(stderr, "refusing to sync") {
		t.Errorf("want refusal on partial set, got %q", stderr)
	}
	if !pr.LoadReviewed(reviewedPath).IsReviewedKey("github.com/cameronsjo/alpha#1", reviewTestTime) {
		t.Error("partial sync must not prune")
	}

	// Empty: an empty open set skips the prune instead of wiping the store.
	if _, stderr, err := execReview(t, fakeReviewSource{}, reviewedPath, "sync"); err != nil {
		t.Fatalf("sync (empty): %v", err)
	} else if !contains(stderr, "skipping prune") {
		t.Errorf("want empty-set skip, got %q", stderr)
	}
	if !pr.LoadReviewed(reviewedPath).IsReviewedKey("github.com/cameronsjo/alpha#1", reviewTestTime) {
		t.Error("empty sync must not wipe the store")
	}
}

// contains is a tiny strings.Contains alias keeping the assertions readable.
func contains(s, substr string) bool {
	return bytes.Contains([]byte(s), []byte(substr))
}
