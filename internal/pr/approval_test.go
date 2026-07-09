package pr

// Test plan for approval.go
//
// ApprovalState (Classification: pure logic delegating to the store)
//   [x] Happy: reviewed mark >= UpdatedAt → StateReviewed
//   [x] Happy: no mark → StateNeedsReview
//   [x] Boundary: mark before a fresh UpdatedAt → StateNeedsReview (un-dim)
//   [x] Unhappy: nil store → StateNeedsReview
//
// Dimmed / ApprovalState agreement (the launcher/picker invariant)
//   [x] Across a mixed PR table, Dimmed(pr) == (ApprovalState(pr) == StateReviewed)
//       for every row — the picker's skip predicate and the dashboard's dim
//       predicate can never disagree because both call Dimmed.

import (
	"path/filepath"
	"testing"
	"time"
)

func TestApprovalState_NilStore_NeedsReview(t *testing.T) {
	pr := PR{Ref: testRef(1), UpdatedAt: time.Unix(1000, 0)}
	if got := ApprovalState(pr, nil); got != StateNeedsReview {
		t.Errorf("nil store: ApprovalState = %v, want StateNeedsReview", got)
	}
	if Dimmed(pr, nil) {
		t.Errorf("nil store: Dimmed = true, want false")
	}
}

func TestApprovalState_MarkVsActivity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-reviewed.json")
	markAt := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	store := LoadReviewed(path, WithNow(func() time.Time { return markAt }))
	if err := store.Mark(testRef(1)); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	cases := []struct {
		name      string
		updatedAt time.Time
		want      State
	}{
		{"activity before mark → reviewed", markAt.Add(-time.Hour), StateReviewed},
		{"activity equals mark → reviewed", markAt, StateReviewed},
		{"fresh activity after mark → needs review", markAt.Add(time.Hour), StateNeedsReview},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := PR{Ref: testRef(1), UpdatedAt: tc.updatedAt}
			if got := ApprovalState(pr, store); got != tc.want {
				t.Errorf("ApprovalState = %v, want %v", got, tc.want)
			}
		})
	}

	// An unmarked ref is never reviewed.
	if got := ApprovalState(PR{Ref: testRef(2), UpdatedAt: markAt}, store); got != StateNeedsReview {
		t.Errorf("unmarked ref: ApprovalState = %v, want StateNeedsReview", got)
	}
}

// TestDimmed_AgreesWithApprovalState is the invariant guard: for every PR in a
// mixed table, the picker's skip predicate (Dimmed) and the dashboard's dim
// predicate (also Dimmed, via ApprovalState) return the same verdict. They
// share one call site, so this can only fail if that seam is broken.
func TestDimmed_AgreesWithApprovalState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pr-reviewed.json")
	markAt := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	store := LoadReviewed(path, WithNow(func() time.Time { return markAt }))
	// Mark #1 and #3; leave #2 and #4 unmarked.
	for _, n := range []int{1, 3} {
		if err := store.Mark(testRef(n)); err != nil {
			t.Fatalf("Mark #%d: %v", n, err)
		}
	}

	table := []PR{
		{Ref: testRef(1), UpdatedAt: markAt.Add(-time.Hour)}, // reviewed
		{Ref: testRef(2), UpdatedAt: markAt.Add(-time.Hour)}, // unmarked
		{Ref: testRef(3), UpdatedAt: markAt.Add(time.Hour)},  // marked but fresh push → un-dimmed
		{Ref: testRef(4), UpdatedAt: markAt},                 // unmarked
	}
	for _, pr := range table {
		wantReviewed := ApprovalState(pr, store) == StateReviewed
		if Dimmed(pr, store) != wantReviewed {
			t.Errorf("%s: Dimmed=%v disagrees with ApprovalState==StateReviewed=%v",
				pr.Ref, Dimmed(pr, store), wantReviewed)
		}
	}
}
