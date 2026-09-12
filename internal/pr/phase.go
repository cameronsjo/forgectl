package pr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// Phase is the durable lifecycle state of one review-session record. It is
// what the record SAYS; whether a tmux window actually exists is observed
// separately (WindowsLive) and rendered beside it, never folded into it.
//
// A legacy record (no version field) has no phase. Every consumer that needs
// one treats it as active with an unknown window — conservative, and it
// costs nothing because a legacy record never occupies a slot.
type Phase string

const (
	// PhaseQueued is intent only: no workspace, no slot, waiting for a drainer.
	PhaseQueued Phase = "queued"
	// PhasePreparing is a reserved slot with the clone in flight.
	PhasePreparing Phase = "preparing"
	// PhasePrepared has a live workspace and no launch yet.
	PhasePrepared Phase = "prepared"
	// PhaseLaunching is fsynced BEFORE tmux new-window, so a crash between the
	// two leaves a record that says exactly where it died.
	PhaseLaunching Phase = "launching"
	// PhaseActive carries a validated native window identity.
	PhaseActive Phase = "active"
	// PhaseNeedsRepair is written when a transition could not prove its own
	// outcome; it always carries a reason.
	PhaseNeedsRepair Phase = "needs-repair"
)

// valid reports whether p is one of the six phases this build knows. An
// unknown phase on a version-2 record is refused at validation rather than
// mapped to anything, because a phase this build cannot name is one it cannot
// reason about.
func (p Phase) valid() bool {
	switch p {
	case PhaseQueued, PhasePreparing, PhasePrepared, PhaseLaunching, PhaseActive, PhaseNeedsRepair:
		return true
	}
	return false
}

// allowsEmptyWorkspace reports whether a record in phase p may carry no
// workspace: nothing has been cloned yet for a queued or preparing session.
func (p Phase) allowsEmptyWorkspace() bool {
	return p == PhaseQueued || p == PhasePreparing
}

// anyPhase is the wildcard `from` for a transition that must land regardless
// of where the record currently sits — only needs-repair uses it, because the
// whole point of that phase is that the writer could not prove which side of a
// mutation it died on.
const anyPhase Phase = ""

// errLegacyRecordNoTransition refuses to move a record that predates phases.
// A legacy record carries no revision, so there is nothing to compare and
// write against; the two ways out are `pr teardown` and
// `pr repair --adopt-window`, which converts it.
var errLegacyRecordNoTransition = errors.New(
	"this is a legacy session record with no phase; it accepts no transition — " +
		"settle it with 'forgectl pr repair <breadcrumb> --apply --adopt-window' or discard it with 'forgectl pr teardown <breadcrumb>'")

// transition moves the record at path from one phase to the next under the
// lifecycle lock.
//
// THE CRASH-SAFETY CONTRACT: it re-reads the record under the lock (what a
// caller read earlier is a claim, not a fact), refuses when the on-disk phase
// is not `from`, applies mut, increments Revision, and writes with the
// revision it just read as the compare-and-write expectation. A mismatch means
// a peer forgectl moved the record between the read and the write: it re-reads
// and retries ONCE, and a second mismatch surfaces to the caller as a skip.
//
// It is the locked shell; every composite verb that already holds the lock
// calls transitionLocked instead, because the lock is non-reentrant.
func (c *Client) transition(ctx context.Context, path string, from, to Phase, mut func(*Breadcrumb) error) error {
	return c.withLifecycleLock(ctx, "transition", func() error {
		return c.transitionLocked(path, from, to, mut)
	})
}

// transitionLocked is transition's core for a caller already holding the lock.
func (c *Client) transitionLocked(path string, from, to Phase, mut func(*Breadcrumb) error) error {
	err := c.transitionOnce(path, from, to, mut)
	if !errors.Is(err, errRecordRevisionMismatch) {
		return err
	}
	slog.Warn("A session record changed underneath a phase transition; re-reading and retrying once.",
		"path", path, "from", string(from), "to", string(to))
	return c.transitionOnce(path, from, to, mut)
}

// transitionOnce is one read-decide-write attempt. It performs no retry, so
// the retry policy lives in exactly one place above it.
func (c *Client) transitionOnce(path string, from, to Phase, mut func(*Breadcrumb) error) error {
	if !to.valid() {
		return fmt.Errorf("refusing to write unknown phase %q to %s", string(to), termsafe.QuotePath(path))
	}
	bc, _, err := loadBreadcrumbRecord(path, c.sessionsDir)
	if err != nil {
		return err
	}
	if bc.Version != breadcrumbVersion {
		slog.Error("Refusing a phase transition on a record with no version.",
			"path", path, "to", string(to))
		return fmt.Errorf("%s: %w", termsafe.QuotePath(path), errLegacyRecordNoTransition)
	}
	if from != anyPhase && bc.Phase != from {
		slog.Error("Refusing a phase transition: the record is not in the expected phase.",
			"path", path, "found", string(bc.Phase), "expected", string(from), "to", string(to))
		return fmt.Errorf("session record %s is in phase %q, not %q; nothing was changed — see 'forgectl pr repair'",
			termsafe.QuotePath(path), string(bc.Phase), string(from))
	}
	expect := bc.Revision
	next := bc
	if mut != nil {
		if err := mut(&next); err != nil {
			return err
		}
	}
	next.Phase = to
	next.Revision = expect + 1
	// The record is validated on the way OUT here — unlike writeBreadcrumbFS,
	// which deliberately does not, so tests can stage forged files. A
	// transition composes a record from one this build already accepted plus a
	// mutation it wrote itself, so a rejection is a bug in the caller and must
	// never reach disk.
	if err := validateBreadcrumbRecord(next); err != nil {
		return fmt.Errorf("refusing to write an invalid %s record for %s: %w",
			string(to), termsafe.QuotePath(path), err)
	}
	data, err := encodeBreadcrumb(next)
	if err != nil {
		return err
	}
	if err := writeRecordAtomic(c.fs, c.sessionsDir, filepath.Base(path), data, expect); err != nil {
		return err
	}
	slog.Debug("Moved a session record to its next phase.",
		"path", path, "from", string(bc.Phase), "to", string(to), "revision", next.Revision)
	return nil
}

// markNeedsRepair parks the record at path in needs-repair with the reason
// written at the throw site — the state a transition enters when it could not
// prove its own outcome. It accepts any current phase by design.
func (c *Client) markNeedsRepair(ctx context.Context, path, reason string) error {
	return c.withLifecycleLock(ctx, "repair-mark", func() error {
		return c.markNeedsRepairLocked(path, reason)
	})
}

// markNeedsRepairLocked is markNeedsRepair's core for a lock holder.
func (c *Client) markNeedsRepairLocked(path, reason string) error {
	return c.transitionLocked(path, anyPhase, PhaseNeedsRepair, func(bc *Breadcrumb) error {
		bc.RepairReason = termsafe.SafeLine(reason)
		bc.WindowID = ""
		return nil
	})
}
