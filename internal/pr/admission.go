package pr

import (
	"context"
	"strings"

	"github.com/cameronsjo/forgectl/internal/tmux"
)

// DefaultMaxConcurrentReviews is the review-window cap applied when
// [pr].max_concurrent is unset or non-positive in config.toml.
const DefaultMaxConcurrentReviews = 4

// reviewWindowPrefix is the tmux window-name prefix a review launch uses.
// windowName() (launch.go) builds off this constant directly rather than a
// re-hardcoded literal, so the two can never drift apart.
const reviewWindowPrefix = "pr-"

// MaxConcurrentReviews resolves the configured cap: any non-positive value
// (unset, zero, or negative) falls back to DefaultMaxConcurrentReviews.
func MaxConcurrentReviews(cfgMax int) int {
	if cfgMax <= 0 {
		return DefaultMaxConcurrentReviews
	}
	return cfgMax
}

// LiveReviews counts tmux windows across ALL sessions whose session matches
// the client's exactly and whose name starts with reviewWindowPrefix — the
// live count of in-flight review launches. ok is false only when the window
// count genuinely could not be read (list-windows erroring for a reason
// other than "no server running"); a server with no matching windows is a
// legitimate zero, not a failure.
//
// ListWindows is the SOLE discriminator, deliberately — it lists every
// window's session verbatim (no `-t` targeting involved), so the count above
// is an exact string comparison against ground truth. An earlier version of
// this function gated the count behind tmux.Client.HasSession(c.tmuxSession)
// first ("skip ListWindows if the session doesn't exist"). That call goes
// through tmux's OWN `-t` resolution, which matches exact name, then
// fnmatch, then PREFIX — so has-session -t forgectl reports true against an
// unrelated session merely named forgectl-review. That fuzziness gated
// nothing useful (the exact-match count below runs the same either way,
// since a window that landed in the wrongly-resolved session was never
// countable under c.tmuxSession regardless of whether HasSession's check
// ran), but it DID mean any transport-level tmux failure — a broken or
// missing binary — surfaced through HasSession's swallowed error as "session
// absent, zero windows" (ok=true) instead of "unreadable" (ok=false),
// letting the cap fail OPEN and grant a full batch on every invocation. Now
// that path runs through ListWindows directly, so the same failure surfaces
// as ok=false and the caller refuses the batch before any clone.
//
// Known residual, NOT fixed here: a review window that lands in the wrong
// session because Launch's own `tmux new-window -t c.tmuxSession` target is
// a bare, unqualified session name — subject to that same tmux fuzzy `-t`
// resolution — is invisible to this exact-match count regardless of how
// LiveReviews itself is implemented. If a sibling session already exists
// with a name that happens to prefix-match c.tmuxSession (e.g. a worktree
// session named after its directory, per internal/projects/projects.go's
// filepath.Base(dir) naming), new-window can keep depositing review windows
// there indefinitely, and this count will never see them. Closing that
// requires qualifying Launch's own tmux target (or verifying exact session
// existence before it dispatches) — out of scope for this admission-gate
// change and tracked separately.
//
// One residual IS accepted here, deliberately conservative: a user-named
// "pr-*" window unrelated to a review gets counted too, which only makes
// admission MORE conservative (fewer slots granted), never less.
func (c *Client) LiveReviews(ctx context.Context) (n int, ok bool) {
	t := tmux.New(c.run)
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

// WindowLive reports whether ref's review window still exists in the client's
// tmux session. ok is false when the window list could not be read at all —
// callers MUST render that as "unknown" rather than "gone", because a
// transport-level tmux failure is indistinguishable from a vanished window if
// the two are collapsed, and reporting "gone" on an unreadable tmux would cry
// wolf on every healthy launch.
//
// Why this exists: `tmux new-window` returns exit 0 the instant the window is
// created — it never observes the child. A review agent that dies seconds
// later (a rejected model, a bad flag, a missing binary) takes the window with
// it, since remain-on-exit is off by default, and the error message dies with
// the pane. Launch has already returned nil and the breadcrumb is on disk, so
// `pr list` — which reads breadcrumbs only — reports the session as active
// forever. This is the read that tells the truth.
func (c *Client) WindowLive(ctx context.Context, ref Ref) (live bool, ok bool) {
	m, ok := c.WindowsLive(ctx, []Ref{ref})
	if !ok {
		return false, false
	}
	return m[ref], true
}

// WindowsLive answers WindowLive for a whole set of refs from ONE ListWindows
// call — the shape `pr list` needs, where a per-ref probe would fork a tmux
// process per session. ok is false when the window list could not be read;
// the returned map is nil in that case, never a map of falses.
//
// What is shared with LiveReviews is the exact SESSION comparison
// (w.Session == c.tmuxSession) and ListWindows as the sole discriminator —
// the load-bearing part. The window test differs by design: LiveReviews
// counts the whole review family with a HasPrefix on reviewWindowPrefix,
// where this needs one specific window, so it compares w.Name against
// windowName(ref) exactly. has-session and `display-message
// -t` both route through tmux's own `-t` resolution — exact, then fnmatch,
// then PREFIX — which reports a sibling session as a match; see the
// LiveReviews doc comment above for the full trace of why that fuzziness is
// unsafe here.
func (c *Client) WindowsLive(ctx context.Context, refs []Ref) (map[Ref]bool, bool) {
	t := tmux.New(c.run)
	wins, err := t.ListWindows(ctx)
	if err != nil {
		return nil, false
	}
	inSession := make(map[string]bool, len(wins))
	for _, w := range wins {
		if w.Session == c.tmuxSession {
			inSession[w.Name] = true
		}
	}
	out := make(map[Ref]bool, len(refs))
	for _, ref := range refs {
		out[ref] = inSession[windowName(ref)]
	}
	return out, true
}

// Admit resolves the concurrency cap from cfgMax (via MaxConcurrentReviews)
// and reports the live count and how many more reviews may launch right now.
// It is the sole place that resolves cfgMax — callers pass the raw
// [pr].max_concurrent value through unresolved rather than pre-resolving it
// themselves, so there is exactly one interpretation of "unset or
// non-positive" in the whole call path.
//
// ok is false when the live count could not be read — callers MUST treat
// that as fail-closed and refuse to launch anything rather than assume free
// capacity.
func (c *Client) Admit(ctx context.Context, cfgMax int) (max, live, free int, ok bool) {
	max = MaxConcurrentReviews(cfgMax)
	live, ok = c.LiveReviews(ctx)
	if !ok {
		return max, 0, 0, false
	}
	free = max - live
	if free < 0 {
		free = 0
	}
	return max, live, free, true
}
