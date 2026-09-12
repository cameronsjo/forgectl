package pr

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/tmux"
)

// DefaultMaxConcurrentReviews is the review-window cap applied when
// [pr].max_concurrent is unset or non-positive in config.toml.
const DefaultMaxConcurrentReviews = 4

// reviewWindowPrefix is the tmux window-name prefix a review launch uses.
// ReviewWindowName() (launch.go) builds off this constant directly rather than a
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
// other than a proven absent default socket); a server with no matching windows is a
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
// CLOSED by forgectl#237, and recorded because this comment previously named
// it as an open residual: Launch used to pass a bare, unqualified session name
// to `tmux new-window -t`, so a sibling session that merely prefix-matched
// c.tmuxSession could absorb review windows indefinitely — invisibly to this
// exact-match count, regardless of how LiveReviews itself is implemented.
// Launch now targets the session's native id via newWindowTarget (launch.go),
// so no name reaches tmux's resolver and every window it creates is countable
// here. Do not reintroduce a name target on that path.
//
// One residual IS accepted here, deliberately conservative: a user-named
// "pr-*" window unrelated to a review gets counted too, which only makes
// admission MORE conservative (fewer slots granted), never less.
func (c *Client) LiveReviews(ctx context.Context) (n int, ok bool) {
	t := c.tmuxClient
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
// ReviewWindowName(ref) exactly. has-session and `display-message
// -t` both route through tmux's own `-t` resolution — exact, then fnmatch,
// then PREFIX — which reports a sibling session as a match; see the
// LiveReviews doc comment above for the full trace of why that fuzziness is
// unsafe here.
func (c *Client) WindowsLive(ctx context.Context, refs []Ref) (map[Ref]bool, bool) {
	t := c.tmuxClient
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
		name, err := ReviewWindowName(ref)
		if err != nil {
			// A ref with no derivable identity has no window to be live in.
			// Reporting it as not-live is correct AND safe: liveness only ever
			// widens what the UI shows and what teardown offers, never what it
			// destroys, and the ref will fail closed at the seam that acts on it.
			slog.Debug("Skipping liveness for a ref with no derivable session identity.",
				"ref", ref.String(), "error", err)
			out[ref] = false
			continue
		}
		out[ref] = inSession[name]
	}
	return out, true
}

// VerifyDispatched performs one delayed window snapshot for a whole launch
// batch and returns dispatches whose generation, session, and name no longer
// match. Errors leave liveness unknown and never fabricate gone reviews.
func (c *Client) VerifyDispatched(ctx context.Context, dispatches []Dispatch) ([]Dispatch, error) {
	if len(dispatches) == 0 {
		return nil, nil
	}
	if err := c.dispatchWait(ctx); err != nil {
		return nil, fmt.Errorf("wait to verify review dispatches: %w", err)
	}
	windows, err := c.tmuxClient.ListWindows(ctx)
	if err != nil {
		return nil, fmt.Errorf("verify review dispatches: %w", err)
	}
	type liveKey struct{ id, session, name string }
	live := make(map[liveKey]bool, len(windows))
	for _, window := range windows {
		// tmux.FieldSep, not a re-hardcoded "\x1f": this key is rebuilt from a
		// list-windows row and compared against an identity Launch captured with
		// tmux.IdentityFormat. A drift between the two joins is silent — every
		// live dispatch would miss and be reported gone.
		live[liveKey{
			id:      strings.Join([]string{window.ServerPID, window.ServerStart, window.ID}, tmux.FieldSep),
			session: window.Session,
			name:    window.Name,
		}] = true
	}
	var gone []Dispatch
	for _, dispatch := range dispatches {
		// Every ref here was keyable at launch, so a failure now is a real fault,
		// not a gone review. Erroring the batch leaves liveness unknown, which the
		// contract above requires; reporting it gone would fabricate one.
		name, err := ReviewWindowName(dispatch.Ref)
		if err != nil {
			return nil, fmt.Errorf("verify review dispatches: %w", err)
		}
		key := liveKey{id: dispatch.WindowID, session: c.tmuxSession, name: name}
		if !live[key] {
			gone = append(gone, dispatch)
		}
	}
	return gone, nil
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
// It counts RECORDS as well as windows: a session in preparing, prepared, or
// launching has reserved a slot whose window does not exist yet (or died
// before anyone observed it), and counting only tmux would hand that slot out
// twice. `queued` and `needs-repair` do not occupy — the first has reserved
// nothing, the second is the operator's call through `pr repair`.
func (c *Client) Admit(ctx context.Context, cfgMax int) (max, live, free int, ok bool) {
	max = MaxConcurrentReviews(cfgMax)
	err := c.withLifecycleLock(ctx, "admit", func() error {
		var lerr error
		live, lerr = c.occupiedLocked(ctx)
		return lerr
	})
	if err != nil {
		slog.Warn("Refusing to report free review slots: the occupancy count could not be read.", "error", err)
		return max, 0, 0, false
	}
	free = max - live
	if free < 0 {
		free = 0
	}
	return max, live, free, true
}

// occupiedLocked counts the review slots in use: live `pr-` windows plus every
// record in preparing/prepared/launching whose derived window is NOT already
// among them (counting both would double-count a healthy in-flight launch).
//
// It refuses — rather than returning a short count — when any record could not
// be read or when tmux could not be read at all. A count that silently omits
// what it could not see is the shape that hands out an occupied slot.
func (c *Client) occupiedLocked(ctx context.Context) (int, error) {
	summaries, unreadable, err := c.listLocked()
	if err != nil {
		return 0, err
	}
	if len(unreadable) > 0 {
		return 0, fmt.Errorf("%d session record(s) could not be read, so the review count would be short — "+
			"settle them with 'forgectl pr repair' before launching", len(unreadable))
	}
	return c.occupancyFrom(ctx, summaries)
}

// occupiesASlot reports whether a recorded phase holds a review slot.
func occupiesASlot(p Phase) bool {
	return p == PhasePreparing || p == PhasePrepared || p == PhaseLaunching
}

// errReviewCapReached is the admission refusal. It carries the numbers the
// CLI's three-line message renders, so the wording lives at the surface and
// the decision lives here.
type errReviewCapReached struct {
	Max  int
	Live int
}

func (e *errReviewCapReached) Error() string {
	return fmt.Sprintf("review cap reached (max %d, %d running) — nothing prepared", e.Max, e.Live)
}

// reserve claims one review slot for ref and writes the `preparing` record
// that holds it, all under ONE lock hold.
//
// The hold covers the count and the write and nothing else: the clone that
// follows runs outside the lock, against a slot already reserved. That is the
// whole point of the phase record — it bridges the long operations the lock
// must never span.
//
// It refuses at the cap, on any unreadable record, and on a ref that already
// has a record in any phase but needs-repair (naming that record). A refusal
// writes nothing.
func (c *Client) reserve(ctx context.Context, ref Ref, cfgMax int, opts PrepareOpts) (string, error) {
	var path string
	err := c.withLifecycleLock(ctx, "reserve", func() error {
		p, err := c.reserveLocked(ctx, ref, cfgMax, opts)
		path = p
		return err
	})
	return path, err
}

// reservation is one lock hold's view of the world: the records that existed
// when the hold began, and how many slots they and tmux account for. A BATCH
// of reservations shares one — reading the directory and forking tmux once per
// ref would be N times the cost and, worse, would let the two disagree with
// each other partway through a batch.
type reservation struct {
	summaries []SessionSummary
	occupied  int
}

// openReservation takes the one snapshot a lock hold reserves against.
func (c *Client) openReservation(ctx context.Context) (*reservation, error) {
	summaries, unreadable, err := c.listLocked()
	if err != nil {
		return nil, err
	}
	if len(unreadable) > 0 {
		slog.Error("Refusing to reserve a review slot: some session records could not be read.",
			"unreadable", len(unreadable), "first", unreadable[0].path)
		return nil, fmt.Errorf("%d session record(s) could not be read, so the review count would be short — "+
			"settle them with 'forgectl pr repair' before launching", len(unreadable))
	}
	occupied, err := c.occupancyFrom(ctx, summaries)
	if err != nil {
		return nil, err
	}
	return &reservation{summaries: summaries, occupied: occupied}, nil
}

// reserveLocked is reserve's core for a caller already holding the lock.
func (c *Client) reserveLocked(ctx context.Context, ref Ref, cfgMax int, opts PrepareOpts) (string, error) {
	res, err := c.openReservation(ctx)
	if err != nil {
		return "", err
	}
	return c.reserveFrom(res, ref, cfgMax, opts)
}

// reserveFrom claims one slot against an open reservation, accounting for it
// so the next ref in the same batch sees it.
func (c *Client) reserveFrom(res *reservation, ref Ref, cfgMax int, opts PrepareOpts) (string, error) {
	maxN := MaxConcurrentReviews(cfgMax)
	summaries := res.summaries
	if existing, ok := recordForRef(summaries, ref); ok {
		slog.Error("Refusing to reserve a review slot: the ref already has a session record.",
			"ref", ref.String(), "path", existing.Path(), "phase", string(existing.Phase()))
		return "", fmt.Errorf("%s already has a session record at %s (phase %s); "+
			"discard it with 'forgectl pr teardown %s' or settle it with 'forgectl pr repair'",
			ref.String(), existing.Path(), phaseOrLegacy(existing.Phase()), existing.Path())
	}
	if res.occupied >= maxN {
		slog.Warn("Refusing to reserve a review slot: the concurrency cap is reached.",
			"max", maxN, "occupied", res.occupied, "ref", ref.String())
		return "", &errReviewCapReached{Max: maxN, Live: res.occupied}
	}
	bc := Breadcrumb{
		Ref:        ref.String(),
		Agent:      opts.Agent,
		CreatedAt:  time.Now().UTC(),
		Local:      ref.IsLocal(),
		Provenance: EffectiveProvenance(ref, opts.Provenance).persisted(),
		Version:    breadcrumbVersion,
		Phase:      PhasePreparing,
		Revision:   1,
	}
	path, err := writeBreadcrumbFS(c.fs, c.sessionsDir, ref, bc)
	if err != nil {
		return "", err
	}
	// Account for the slot this call just took, so the next ref in the same
	// batch cannot be granted it too.
	res.occupied++
	res.summaries = append(res.summaries, SessionSummary{
		ref: ref, path: path, createdAt: bc.CreatedAt,
		availability: workspaceAvailabilityNone, phase: PhasePreparing,
	})
	slog.Info("Successfully reserved a review slot.", "ref", ref.String(), "path", path, "max", maxN, "occupied", res.occupied)
	return path, nil
}

// occupancyFrom counts slots from an already-taken listing, so a caller that
// has one does not pay for a second directory walk (and cannot disagree with
// itself about what was on disk).
func (c *Client) occupancyFrom(ctx context.Context, summaries []SessionSummary) (int, error) {
	liveWindows, names, ok := c.reviewWindowSnapshot(ctx)
	if !ok {
		return 0, fmt.Errorf("the tmux review window count could not be read; " +
			"check `tmux list-windows -a`, then retry")
	}
	return occupancyFromSnapshot(summaries, liveWindows, names), nil
}

// reviewWindowSnapshot reads the window list ONCE and answers both questions
// admission asks of it: how many review windows are live, and which review
// window names exist. Two separate reads (LiveReviews then WindowsLive) would
// fork tmux twice per decision and — worse — could disagree with each other,
// since the server moves between them.
func (c *Client) reviewWindowSnapshot(ctx context.Context) (live int, names map[string]bool, ok bool) {
	wins, err := c.tmuxClient.ListWindows(ctx)
	if err != nil {
		return 0, nil, false
	}
	names = make(map[string]bool, len(wins))
	for _, w := range wins {
		if w.Session != c.tmuxSession {
			continue
		}
		names[w.Name] = true
		if strings.HasPrefix(w.Name, reviewWindowPrefix) {
			live++
		}
	}
	return live, names, true
}

// occupancyFromSnapshot adds the RECORD term to the live-window count: a
// session in preparing/prepared/launching holds a slot whose window does not
// exist yet, and one whose window DOES exist is already counted by the live
// tally — so only the windowless ones are added.
//
// A ref with no derivable window name counts as occupied. That is the
// conservative direction on purpose: it can only grant fewer slots, never more.
func occupancyFromSnapshot(summaries []SessionSummary, liveWindows int, names map[string]bool) int {
	reserved := 0
	for _, s := range summaries {
		if !occupiesASlot(s.Phase()) {
			continue
		}
		name, err := ReviewWindowName(s.Ref())
		if err != nil || !names[name] {
			reserved++
		}
	}
	return liveWindows + reserved
}

// recordForRef finds an existing record for ref that BLOCKS a new reservation.
// A needs-repair record does not block: it is the state an operator is
// expected to settle, and refusing a fresh launch on it would make a crashed
// session permanently un-relaunchable.
func recordForRef(summaries []SessionSummary, ref Ref) (SessionSummary, bool) {
	for _, s := range summaries {
		if s.Ref() != ref {
			continue
		}
		if s.Phase() == PhaseNeedsRepair {
			continue
		}
		return s, true
	}
	return SessionSummary{}, false
}

// phaseOrLegacy renders a phase for an operator-facing message, naming the one
// state that has no phase rather than printing an empty string at them.
func phaseOrLegacy(p Phase) string {
	if p == "" {
		return "legacy, no phase"
	}
	return string(p)
}
