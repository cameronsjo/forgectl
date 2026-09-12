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
)

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
func (c *Client) Drain(ctx context.Context, cfg config.Config, opts DrainOpts) (DrainReport, error) {
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = DefaultDrainMaxAttempts
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
		for _, s := range summaries {
			switch s.Phase() {
			case PhaseQueued:
				queued = append(queued, s)
			case PhasePreparing, PhasePrepared, PhaseLaunching:
				report.Launching++
			}
		}
		report.Queued = len(queued)
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

	prepOpts := PrepareOpts{
		Agent:      bc.Agent,
		Provenance: ParseReviewProvenance(bc.Provenance),
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
	outcome, toPhase := c.settleDrainFailure(ctx, path, bc.Attempts, maxAttempts, err)
	item.Outcome = outcome
	item.ToPhase = toPhase
	return item
}

// settleDrainFailure records the failed attempt on the record — regardless of
// what phase Prepare/Launch left it in (queued's own `preparing`, or a
// needs-repair Launch itself already wrote on a dispatch failure) — and moves
// it to `queued` for a future pass to retry, or to `needs-repair` once
// attempts is exhausted. It uses the wildcard `from` (anyPhase) for the same
// reason markNeedsRepair does: the caller does not know, and must not have to
// know, which phase the failure left the record in.
func (c *Client) settleDrainFailure(ctx context.Context, path string, priorAttempts, maxAttempts int, cause error) (outcome, toPhase string) {
	attempts := priorAttempts + 1
	lastError := termsafe.SafeLine(cause.Error())
	exhausted := attempts >= maxAttempts

	target := PhaseQueued
	outcome = drainOutcomeRetryQueued
	if exhausted {
		target = PhaseNeedsRepair
		outcome = drainOutcomeNeedsRepair
	}

	err := c.transition(ctx, path, anyPhase, target, func(bc *Breadcrumb) error {
		bc.Attempts = attempts
		bc.LastError = lastError
		bc.LastAttempt = time.Now().UTC()
		if exhausted {
			bc.RepairReason = fmt.Sprintf("drain: %d attempts, last: %s", attempts, lastError)
			// A retry that got as far as a workspace leaves it behind for
			// `pr repair` to inspect; needs-repair does not require an empty
			// workspace.
		} else {
			bc.RepairReason = ""
			// queued must not carry a workspace: a retried Prepare clones a
			// fresh one, so any workspace this failed attempt created would
			// otherwise leak. Best-effort teardown; its own failure must not
			// shadow the launch error already being reported.
			if bc.Workspace != "" {
				if terr := sandboxTeardown(ctx, c.run, bc.Workspace); terr != nil {
					slog.Error("Failed to tear down the workspace from a failed drain attempt; it may need manual removal.",
						"path", path, "workspace", bc.Workspace, "error", terr)
				}
				bc.Workspace = ""
			}
		}
		return nil
	})
	if err != nil {
		slog.Error("Failed to settle a drain launch failure; the record's attempt count was not recorded.",
			"path", path, "target", string(target), "error", err)
		return drainOutcomeClaimFailure, ""
	}
	return outcome, string(target)
}
