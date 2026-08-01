package pr

import (
	"context"
	"strings"

	"github.com/cameronsjo/forgectl/internal/tmux"
)

// DefaultMaxConcurrentReviews is the review-window cap applied when
// [pr].max_concurrent is unset or non-positive in config.toml.
const DefaultMaxConcurrentReviews = 4

// reviewWindowPrefix is the tmux window-name prefix a review launch uses —
// must match windowName() in launch.go.
const reviewWindowPrefix = "pr-"

// MaxConcurrentReviews resolves the configured cap: any non-positive value
// (unset, zero, or negative) falls back to DefaultMaxConcurrentReviews.
func MaxConcurrentReviews(cfgMax int) int {
	if cfgMax <= 0 {
		return DefaultMaxConcurrentReviews
	}
	return cfgMax
}

// LiveReviews counts tmux windows under the client's session whose name
// starts with reviewWindowPrefix — the live count of in-flight review
// launches. ok is false only when the window count genuinely could not be
// read (list-windows erroring); an absent tmux session is a legitimate zero,
// not a failure.
//
// Two residuals are deliberately left open, both fail-safe:
//   - a broken tmux binary reads as genuine-zero via HasSession's exit-code
//     check and fails closed one step later, at Launch's own tmux new-window
//     call — no session is ever killed by an admission miscount.
//   - a user-named "pr-*" window unrelated to a review gets counted too;
//     that only makes admission MORE conservative (fewer slots granted),
//     never less.
func (c *Client) LiveReviews(ctx context.Context) (n int, ok bool) {
	t := tmux.New(c.run)
	if !t.HasSession(ctx, c.tmuxSession) {
		return 0, true
	}
	wins, err := t.ListWindows(ctx)
	if err != nil {
		return 0, false
	}
	count := 0
	for _, w := range wins {
		if w.Session == c.tmuxSession && strings.HasPrefix(w.Name, reviewWindowPrefix) {
			count++
		}
	}
	return count, true
}

// Admit resolves the concurrency cap (via MaxConcurrentReviews) and reports
// how many more reviews may launch right now. ok is false when the live
// count could not be read — callers MUST treat that as fail-closed and
// refuse to launch anything rather than assume free capacity.
func (c *Client) Admit(ctx context.Context, max int) (free int, ok bool) {
	max = MaxConcurrentReviews(max)
	live, ok := c.LiveReviews(ctx)
	if !ok {
		return 0, false
	}
	free = max - live
	if free < 0 {
		free = 0
	}
	return free, true
}
