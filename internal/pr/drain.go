package pr

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// DefaultDrainMaxAttempts is the number of failed launch attempts a queued
// record survives before the drainer parks it in needs-repair rather than
// handing it back to the queue for another pass to retry.
const DefaultDrainMaxAttempts = 3

// Drain outcomes. "launched" and "would-launch" mirror the launch/dry-run
// split every other verb in this package uses; "retry-queued" and
// "needs-repair" name the two ways a launch failure can land.
const (
	drainOutcomeLaunched     = "launched"
	drainOutcomeWouldLaunch  = "would-launch"
	drainOutcomeRetryQueued  = "retry-queued"
	drainOutcomeNeedsRepair  = "needs-repair"
	drainOutcomeClaimFailure = "claim-failed"
	drainOutcomeRefused      = "refused"
)

// errLocalNotDrainable is the reason a queued LOCAL record is refused at claim
// time. It mirrors Queue's writer-side refusal (session.go) in the reader, so
// a record that never went through Queue is stopped before it is claimed,
// cloned, or handed to an agent.
const errLocalNotDrainable = "local reviews cannot be drained: a local session's findings directory is never " +
	"persisted and a reloaded local session refuses to launch — review it now with 'forgectl pr local', not later"

// DrainOpts drives one `pr drain` pass.
//
// Once, Watch, and Interval are carried here to complete the shape the design
// names, but Drain itself always performs exactly ONE PASS — the watch loop,
// its per-pass stdout line, and its backoff-then-refuse policy live in the
// CLI (internal/cli/pr_drain.go), which calls Drain once per tick. That
// split keeps this package free of anything that prints or sleeps, matching
// every other verb here (Repair, Prune): the ops layer returns a report: the
// CLI decides how often to ask for one and what to do with it.
type DrainOpts struct {
	// Once is accepted for shape completeness; Drain does not read it — one
	// call is always one pass.
	Once bool
	// Watch is accepted for shape completeness; the CLI reads it to decide
	// whether to loop, not this function.
	Watch bool
	// Interval is accepted for shape completeness; the CLI reads it to time
	// the loop, not this function.
	Interval time.Duration
	// DryRun claims nothing and launches nothing: it reports which queued
	// records the pass would have claimed, in claim order, and touches no
	// record, workspace, or tmux window.
	DryRun bool
	// MaxAttempts is how many failed launches a queued record survives before
	// the drainer parks it in needs-repair instead of returning it to the
	// queue. Non-positive resolves to DefaultDrainMaxAttempts.
	MaxAttempts int
}

// DrainItem is one row of a drain pass report — the queued record claimed (or
// that would have been claimed on --dry-run) and what happened to it.
type DrainItem struct {
	Ref        string `json:"ref"`
	RecordPath string `json:"record_path"`
	FromPhase  string `json:"from_phase"`
	ToPhase    string `json:"to_phase,omitempty"`
	Outcome    string `json:"outcome"`
	Error      string `json:"error,omitempty"`
}

// DrainReport is what one `pr drain` pass returns and `--json` encodes.
//
// Refusal is set, and Items left empty, when the whole pass refused before
// claiming anything: an unreadable cap, an unreadable record, or a lock
// timeout. A partial pass (some items launched, some failed) is never a
// Refusal — it is reported item by item, exactly as `pr repair`'s inspect
// reports one unsettled record per row rather than a single "something is
// wrong".
type DrainReport struct {
	Pass      int         `json:"pass"`
	Free      int         `json:"free"`
	Queued    int         `json:"queued"`
	Launching int         `json:"launching"`
	Launched  int         `json:"launched"`
	Failed    int         `json:"failed"`
	Items     []DrainItem `json:"items"`
	Refusal   string      `json:"refusal,omitempty"`
}

// Drain performs one drain pass: it takes the lifecycle lock, counts
// occupancy, refuses the WHOLE pass if any record could not be read, and
// otherwise claims the oldest queued records — by createdAt, FIFO — up to
// however many slots are free, transitioning each to `preparing` under that
// SAME lock hold. The lock is released before any claimed record is prepared
// or launched: the clone and the dispatch both run outside it, against slots
// already claimed, exactly as every other launch path in this package does.
//
// Each claimed record is then prepared and launched through the identical
// Prepare -> Launch path `pr <ref>` uses. A launch failure increments the
// record's Attempts, records LastError/LastAttempt, and returns it to
// `queued` for the next pass to retry — unless Attempts has reached
// opts.MaxAttempts, in which case it is parked in `needs-repair` with a
// reason naming the attempt count and the last error, and no further pass
// will pick it up (queued records are the only ones drain claims).
//
// A retry is offered ONLY when the failure landed before dispatch. A record
// Launch already parked, or a ref whose review window is resolvable (or whose
// tmux is unreadable), is left parked with its workspace intact and is never
// retried — see settleDrainFailure.
func (c *Client) Drain(ctx context.Context, cfg config.Config, opts DrainOpts) (DrainReport, error) {
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = DefaultDrainMaxAttempts
	}
	// Dispatch capability is checked ONCE, before anything is claimed, exactly
	// as the human path checks it before reserving (internal/cli/pr.go). A tmux
	// that cannot dispatch fails every launch in the pass, so asking it here
	// costs one probe and refuses the whole pass; asking it per record instead
	// means N clones and N teardowns before N records park in needs-repair.
	if err := c.CheckDispatchCapability(ctx); err != nil {
		slog.Error("Refusing a drain pass: this host cannot dispatch a review window.", "error", err)
		return DrainReport{Items: []DrainItem{}, Refusal: termsafe.SafeLine(err.Error())}, nil
	}
	report, claimed := c.claimQueuedPass(ctx, cfg, opts)
	if report.Refusal != "" {
		return report, nil
	}
	if opts.DryRun {
		for _, s := range claimed {
			report.Items = append(report.Items, DrainItem{
				Ref:        s.Ref().String(),
				RecordPath: s.Path(),
				FromPhase:  string(PhaseQueued),
				Outcome:    drainOutcomeWouldLaunch,
			})
		}
		return report, nil
	}
	for _, s := range claimed {
		item := c.drainItem(ctx, cfg, s, opts.MaxAttempts)
		report.Items = append(report.Items, item)
		if item.Outcome == drainOutcomeLaunched {
			report.Launched++
		} else {
			report.Failed++
		}
	}
	return report, nil
}

// claimQueuedPass takes the lifecycle lock once, counts occupancy, and — off
// --dry-run — transitions the oldest free-slot's-worth of queued records to
// `preparing`. On --dry-run it claims nothing and returns the records that
// WOULD have been claimed, so the caller's would-launch rows name exactly the
// same set a real pass would have started on.
//
// It refuses the whole pass, before claiming anything, when List reports an
// unreadable record or when the occupancy count could not be read (tmux
// unreadable) — the same fail-closed rule every other counting arm in this
// package follows (admission.go's occupiedLocked, reserve's openReservation).
func (c *Client) claimQueuedPass(ctx context.Context, cfg config.Config, opts DrainOpts) (DrainReport, []SessionSummary) {
	report := DrainReport{Items: []DrainItem{}}
	var claimed []SessionSummary
	err := c.withLifecycleLock(ctx, "drain", func() error {
		summaries, unreadable, lerr := c.listLocked()
		if lerr != nil {
			return lerr
		}
		if len(unreadable) > 0 {
			return fmt.Errorf("%d session record(s) could not be read, so the free-slot count would be wrong — "+
				"settle them with 'forgectl pr repair' before draining", len(unreadable))
		}
		occupied, lerr := c.occupancyFrom(ctx, summaries)
		if lerr != nil {
			return lerr
		}
		maxN := MaxConcurrentReviews(cfg.Pr.MaxConcurrent)
		free := maxN - occupied
		if free < 0 {
			free = 0
		}
		report.Free = free

		var queued []SessionSummary
		refused := 0
		for _, s := range summaries {
			switch s.Phase() {
			case PhaseQueued:
				// READER-SIDE MIRROR of Queue's local-ref refusal (session.go).
				// Queue refuses to WRITE a local queued record; nothing refused
				// to READ one, so a hand-written or forged record with
				// `local: true` was claimed, cloned, and carried all the way to
				// Launch's own local refusal — which that function's comment
				// calls an incidental second barrier. The refusal belongs at the
				// claim, where the record is still untouched.
				if s.Ref().IsLocal() {
					slog.Error("Refusing to claim a queued review: local reviews cannot be drained.",
						"ref", s.Ref().String(), "path", s.Path())
					report.Items = append(report.Items, DrainItem{
						Ref:        s.Ref().String(),
						RecordPath: s.Path(),
						FromPhase:  string(PhaseQueued),
						ToPhase:    string(PhaseQueued),
						Outcome:    drainOutcomeRefused,
						Error:      errLocalNotDrainable,
					})
					refused++
					continue
				}
				queued = append(queued, s)
			case PhasePreparing, PhasePrepared, PhaseLaunching:
				report.Launching++
			}
		}
		// Refused records are still queued on disk, so they count toward the
		// queue depth the report names — they are simply never claimed.
		report.Queued = len(queued) + refused
		report.Failed += refused
		sort.Slice(queued, func(i, j int) bool {
			return queued[i].CreatedAt().Before(queued[j].CreatedAt())
		})

		n := free
		if n > len(queued) {
			n = len(queued)
		}
		for i := 0; i < n; i++ {
			s := queued[i]
			if opts.DryRun {
				claimed = append(claimed, s)
				continue
			}
			if terr := c.transitionLocked(s.Path(), PhaseQueued, PhasePreparing, nil); terr != nil {
				slog.Error("Refusing to claim a queued review: the record could not be moved to preparing.",
					"ref", s.Ref().String(), "path", s.Path(), "error", terr)
				continue
			}
			claimed = append(claimed, s)
		}
		return nil
	})
	if err != nil {
		report.Refusal = err.Error()
	}
	return report, claimed
}

// drainItem prepares and launches one already-claimed (`preparing`) record
// through the same Prepare -> Launch path a human's `pr <ref>` uses, and
// settles a launch failure per the attempt-count policy above.
func (c *Client) drainItem(ctx context.Context, cfg config.Config, s SessionSummary, maxAttempts int) DrainItem {
	ref := s.Ref()
	path := s.Path()
	item := DrainItem{Ref: ref.String(), RecordPath: path, FromPhase: string(PhaseQueued)}

	bc, _, err := loadBreadcrumbRecord(path, c.sessionsDir)
	if err != nil {
		item.Outcome = drainOutcomeClaimFailure
		item.Error = termsafe.SafeLine(err.Error())
		return item
	}

	// provenanceFromRecord, not ParseReviewProvenance: it applies the joint
	// shape check (breadcrumb.go), so a record claiming authorship without the
	// canonical local shape warns here too. The outcome is identical either way
	// — EffectiveProvenance downgrades a remote ref regardless — but the
	// unattended path is the one with nobody watching, so it is the last place
	// that warning should be missing.
	prepOpts := PrepareOpts{
		Agent:      bc.Agent,
		Provenance: provenanceFromRecord(bc),
		RecordPath: path,
	}
	sess, err := c.Prepare(ctx, ref, prepOpts)
	if err == nil {
		_, err = c.Launch(ctx, sess, cfg)
	}
	if err == nil {
		item.ToPhase = string(PhaseActive)
		item.Outcome = drainOutcomeLaunched
		return item
	}

	item.Error = termsafe.SafeLine(err.Error())
	outcome, toPhase := c.settleDrainFailure(ctx, ref, path, bc.Attempts, maxAttempts, err)
	item.Outcome = outcome
	item.ToPhase = toPhase
	return item
}

// settleDrainFailure records the failed attempt on the record and decides
// whether the ref may be retried at all.
//
// A RETRY IS ONLY SAFE WHEN THE FAILURE LANDED BEFORE DISPATCH. Launch returns
// an error on two branches where the tmux window ALREADY EXISTS and the review
// agent is running — a windowId that is not generation-qualified, and a failed
// `launching -> active` transition — and both park the record in needs-repair
// with a reason naming the window, which is the only pointer
// `pr repair --adopt-window` has left (launch.go's completeLaunch). Requeuing
// one of those would delete the clean room under a live agent, erase that
// pointer, and let a later pass launch a SECOND agent for the same ref. So the
// settlement reads the two signals that distinguish the cases and refuses to
// retry on either: the record arriving already parked, and a window resolvable
// by the ref's derived name. An unreadable tmux counts as "a window may exist"
// — the fail-closed direction, matching WindowLive's own contract that
// unreadable is not "gone".
//
// Only a failure with NEITHER signal — a clone or `gh` failure before dispatch
// — returns the record to `queued` for another pass, or parks it once attempts
// are exhausted.
func (c *Client) settleDrainFailure(ctx context.Context, ref Ref, path string, priorAttempts, maxAttempts int, cause error) (outcome, toPhase string) {
	attempts := priorAttempts + 1
	lastError := termsafe.SafeLine(cause.Error())

	bc, _, rerr := loadBreadcrumbRecord(path, c.sessionsDir)
	if rerr != nil {
		slog.Error("Failed to re-read a session record after a drain launch failure; it was left as the failure found it.",
			"ref", ref.String(), "path", path, "error", rerr)
		return drainOutcomeClaimFailure, ""
	}
	if bc.Phase == PhaseNeedsRepair {
		// Launch already parked it with a reason naming the window. Record the
		// attempt WITHOUT touching RepairReason, Workspace, or WindowID: this
		// record is now `pr repair`'s to settle, not the drainer's to retry.
		return c.recordParkedAttempt(ctx, ref, path, attempts, lastError,
			"a review window may already exist for this ref")
	}
	if live, ok := c.WindowLive(ctx, ref); !ok || live {
		reason := fmt.Sprintf("drain: launch failed with a review window present (or tmux unreadable) for %s; "+
			"settle it with 'forgectl pr repair --adopt-window' — last: %s", ref.String(), lastError)
		slog.Error("Refusing to retry a drained review: a window for this ref may be live, so its clean room stays.",
			"ref", ref.String(), "path", path, "windowReadable", ok, "error", cause)
		if terr := c.transition(ctx, path, anyPhase, PhaseNeedsRepair, func(rec *Breadcrumb) error {
			rec.Attempts = attempts
			rec.LastError = lastError
			rec.LastAttempt = time.Now().UTC()
			rec.RepairReason = termsafe.SafeLine(reason)
			return nil
		}); terr != nil {
			slog.Error("Failed to park a drained review whose window may be live.",
				"ref", ref.String(), "path", path, "error", terr)
			return drainOutcomeClaimFailure, ""
		}
		return drainOutcomeNeedsRepair, string(PhaseNeedsRepair)
	}

	// No park, no window: the failure happened before anything was dispatched,
	// so the workspace this attempt may have cloned is nobody's and the ref is
	// safe to retry.
	exhausted := attempts >= maxAttempts
	target := PhaseQueued
	outcome = drainOutcomeRetryQueued
	if exhausted {
		target = PhaseNeedsRepair
		outcome = drainOutcomeNeedsRepair
	}

	// The mutator is side-effect-free and idempotent BY CONTRACT:
	// transitionLocked re-runs it once on a revision mismatch, and it holds the
	// lifecycle lock while it does. The workspace removal therefore happens
	// after this returns — a recursive delete of a full clone must never stall
	// every other lifecycle-lock user (another drainer, `pr <ref>`, `pr pick`,
	// `pr repair`) for its duration.
	var cleared string
	err := c.transition(ctx, path, anyPhase, target, func(rec *Breadcrumb) error {
		rec.Attempts = attempts
		rec.LastError = lastError
		rec.LastAttempt = time.Now().UTC()
		if exhausted {
			rec.RepairReason = fmt.Sprintf("drain: %d attempts, last: %s", attempts, lastError)
			// A retry that got as far as a workspace leaves it behind for
			// `pr repair` to inspect; needs-repair does not require an empty
			// workspace.
			return nil
		}
		rec.RepairReason = ""
		// queued must not carry a workspace or a window: a retried Prepare
		// clones a fresh one, and a stale windowId on a queued record names a
		// window this ref no longer has.
		cleared = rec.Workspace
		rec.Workspace = ""
		rec.WindowID = ""
		return nil
	})
	if err != nil {
		slog.Error("Failed to settle a drain launch failure; the record's attempt count was not recorded.",
			"path", path, "target", string(target), "error", err)
		return drainOutcomeClaimFailure, ""
	}
	// Best-effort, outside the lock; its own failure must not shadow the launch
	// error already being reported. sandboxTeardown carries the prefix and
	// symlink checks that bound every removal in this package.
	if cleared != "" {
		if terr := sandboxTeardown(ctx, c.run, cleared); terr != nil {
			slog.Error("Failed to tear down the workspace from a failed drain attempt; it may need manual removal.",
				"path", path, "workspace", cleared, "error", terr)
		}
	}
	return outcome, string(target)
}

// recordParkedAttempt records one more failed attempt on a record Launch
// ALREADY parked in needs-repair, preserving the repair reason, the workspace,
// and the window id it wrote. The phase does not move (needs-repair to
// needs-repair) — the write exists so the attempt count and last error stay
// truthful for `pr repair`.
func (c *Client) recordParkedAttempt(ctx context.Context, ref Ref, path string, attempts int, lastError, why string) (outcome, toPhase string) {
	slog.Error("Refusing to retry a drained review: its record is already parked in needs-repair.",
		"ref", ref.String(), "path", path, "why", why)
	if err := c.transition(ctx, path, PhaseNeedsRepair, PhaseNeedsRepair, func(rec *Breadcrumb) error {
		rec.Attempts = attempts
		rec.LastError = lastError
		rec.LastAttempt = time.Now().UTC()
		return nil
	}); err != nil {
		slog.Error("Failed to record a drain attempt on an already-parked record.",
			"ref", ref.String(), "path", path, "error", err)
		return drainOutcomeClaimFailure, ""
	}
	return drainOutcomeNeedsRepair, string(PhaseNeedsRepair)
}
