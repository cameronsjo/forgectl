package pr

import (
	"context"
	"testing"
)

// TestLiveReviews_DottedSessionMatchesStoredName is forgectl#815 for the
// review session: tmux stores a session created as "x.y" as "x_y", and every
// window row names "x_y". WithTmuxSession stores the configured name the same
// way, so the review windows under it are counted.
//
// Mutation that turns it red: store the raw name in WithTmuxSession (the count
// reads 0).
func TestLiveReviews_DottedSessionMatchesStoredName(t *testing.T) {
	fake := listWindowsFake(winRow("x_y", "pr-a-b-1"), winRow("x_y", "pr-c-d-2"))
	c := New(fake, WithTmuxSession("x.y"))
	n, ok := c.LiveReviews(context.Background())
	if !ok || n != 2 {
		t.Fatalf("LiveReviews() = (%d, %v), want (2, true)", n, ok)
	}
}
