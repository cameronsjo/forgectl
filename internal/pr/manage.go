package pr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// List returns a presentation row for every review session recorded in the
// session-state dir whose workspace is either LIVE or cleanly MISSING, sorted
// newest first.
//
// Rows are SessionSummary, not Session, because a stale record has no workspace
// to act on and must still be listable — a row the user cannot see is a row
// they cannot tear down, which is the visibility half of #212.
//
// Each breadcrumb goes through the same location+record validation as every
// other consumer; anything that fails, or whose workspace classifies invalid,
// is skipped (logged), not fatal — one corrupt file must not blind the list.
func (c *Client) List() ([]SessionSummary, error) {
	entries, err := os.ReadDir(c.sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read pr sessions dir: %w", err)
	}
	var summaries []SessionSummary
	var live, missing, invalid int
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(c.sessionsDir, e.Name())
		sum, err := c.loadSummary(path)
		if err != nil {
			slog.Warn("Skipping invalid pr breadcrumb.", "path", path, "error", err)
			invalid++
			continue
		}
		switch {
		case sum.IsWorkspaceMissing():
			missing++
		case sum.IsWorkspaceLive():
			live++
		}
		summaries = append(summaries, sum)
	}
	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].createdAt.After(summaries[j].createdAt)
	})
	// One aggregate at debug altitude. Per-record warnings above keep their
	// existing altitude; no path or ref is added here, so listing stays quiet
	// and leaks nothing new.
	slog.Debug("Listed pr session breadcrumbs.", "live", live, "missing", missing, "invalid_skipped", invalid)
	return summaries, nil
}

// loadSummary builds one presentation row: record validation, then workspace
// classification. Live and missing both yield a row; invalid is an error, so
// an unclassifiable record can never be presented as if it were understood.
func (c *Client) loadSummary(path string) (SessionSummary, error) {
	bc, _, err := loadBreadcrumbRecord(path, c.sessionsDir)
	if err != nil {
		return SessionSummary{}, err
	}
	ref, err := refFromRecord(bc)
	if err != nil {
		return SessionSummary{}, err
	}
	// Live and missing are both presentable, so their errors are discarded on
	// purpose — classifyWorkspace returns the typed missing error alongside a
	// perfectly listable row, and only a consumer that ACTS on the workspace
	// (loadBreadcrumb) needs to surface it.
	avail, err := classifyWorkspace(bc.Workspace)
	switch avail {
	case workspaceAvailabilityLive, workspaceAvailabilityMissing:
	default:
		return SessionSummary{}, fmt.Errorf("breadcrumb %s: %w", termsafe.QuotePath(path), err)
	}
	return SessionSummary{ref: ref, path: path, createdAt: bc.CreatedAt, availability: avail}, nil
}

// refFromRecord restores the Ref a breadcrumb records. Locality cannot ride
// the ref string (owner "local" is only a display value); the breadcrumb's own
// flag is what restores it. Shared by the summary and session loaders so the
// two cannot drift on locality.
func refFromRecord(bc Breadcrumb) (Ref, error) {
	ref, err := ParseRef(bc.Ref)
	if err != nil {
		return Ref{}, fmt.Errorf("breadcrumb ref: %w", err)
	}
	if bc.Local {
		ref = ref.asLocal()
	}
	return ref, nil
}

// loadSession validates+loads the breadcrumb at path (using the client's
// session-state dir) and reconstitutes an ACTIONABLE Session from it. It goes
// through the strict live loader, so a stale or invalid workspace is an error
// here by design — every caller of this function is about to act on Workspace.
func (c *Client) loadSession(path string) (Session, error) {
	bc, err := loadBreadcrumb(path, c.sessionsDir)
	if err != nil {
		return Session{}, err
	}
	ref, err := refFromRecord(bc)
	if err != nil {
		return Session{}, err
	}
	// Provenance is resolved through provenanceFromRecord (joint shape
	// validation) and then through EffectiveProvenance against the
	// RECONSTRUCTED ref.
	//
	// The second pass CANNOT UPGRADE a loaded record today, and is deliberately
	// kept. It does change value on some records — a non-local ref carrying an
	// unknown or absent provenance is normalized down to third-party — but no
	// record reaches operator-authored that did not already declare it, and none
	// yields operator-authored with a non-local reloaded ref. That holds because
	// provenanceFromRecord only returns that value when bc.Local is set,
	// refFromRecord makes exactly those refs local, and validateBreadcrumbRecord
	// already rejects a Local record whose owner is not the sentinel. Three
	// invariants have to hold for the pass to stay upgrade-proof, and all three
	// live in other functions. This costs one comparison and means a change to
	// any of them degrades to a refusal rather than to an unconfined shell.
	//
	// Neither pass is what stops the hostile breadcrumb end-to-end — Launch's
	// own re-check does, and a mutation probe confirms it fires alone. These are
	// depth, and each is pinned by its own unit test so it cannot be mistaken
	// for dead code.
	return Session{
		Ref:        ref,
		Workspace:  bc.Workspace,
		Agent:      bc.Agent,
		Path:       path,
		CreatedAt:  bc.CreatedAt,
		Provenance: EffectiveProvenance(ref, provenanceFromRecord(bc)),
	}, nil
}

// remediateMissingWorkspace appends teardown guidance to err, but ONLY when
// the failure is a cleanly missing workspace — the one state `pr teardown` can
// actually resolve.
//
// The gate is errors.As on the typed missing error, never
// errors.Is(err, fs.ErrNotExist). A dangling final symlink, an unresolvable
// parent, and a resolution race all wrap the same OS cause while being
// invalid; telling a user to run teardown for those would be advising a
// deletion the teardown path is going to refuse anyway.
func remediateMissingWorkspace(path string, err error) error {
	var missing *workspaceMissingError
	if !errors.As(err, &missing) {
		return err
	}
	return fmt.Errorf("%w — its workspace is gone, so there is nothing to attach to; "+
		"discard the leftover breadcrumb with `forgectl pr teardown %q`", err, path)
}

// Attach jumps to the review window for the breadcrumb at path. It validates
// the breadcrumb (location + content) BEFORE any tmux argv is built, so a
// hostile path cannot steer tmux at an arbitrary target.
func (c *Client) Attach(ctx context.Context, path string) error {
	sess, err := c.loadSession(path)
	if err != nil {
		return remediateMissingWorkspace(path, err)
	}
	name, err := ReviewWindowName(sess.Ref)
	if err != nil {
		return err
	}
	window, err := c.resolveReviewWindow(ctx, sess.Ref)
	if err != nil {
		return fmt.Errorf("select review window %q: %w — the window may predate a "+
			"forgectl upgrade that renamed review windows; relaunch the review with `pr <ref>`", name, err)
	}
	slog.Debug("Attaching to review window.", "window_id", window.ID, "session_id", window.SessionID, "name", name)
	if err := c.tmuxClient.SelectWindow(ctx, window); err != nil {
		return fmt.Errorf("select review window %q: %w", name, err)
	}
	return nil
}

// Open opens a fresh shell window rooted at the review workspace — a way into
// the clean room without disturbing the review agent's window. It validates
// the breadcrumb before touching tmux.
func (c *Client) Open(ctx context.Context, path string) error {
	sess, err := c.loadSession(path)
	if err != nil {
		return remediateMissingWorkspace(path, err)
	}
	slog.Debug("Opening workspace window.", "workspace", sess.Workspace)
	// A review session was already dispatched to reach this breadcrumb, so the
	// review session should already exist — ensureSession is a cheap exact
	// lookup in the common case, and a safety net if it was killed out from
	// under a still-valid breadcrumb.
	session, err := c.ensureSession(ctx)
	if err != nil {
		return err
	}
	// A shell window is NOT the review — it is a second, non-authoritative
	// window under the same key, distinguished by its role in the digest rather
	// than by a "-shell" suffix on the review's name. A suffix was both losable
	// to the name bound and indistinguishable from a review window whose own
	// label happened to end that way.
	name, err := shellWindowName(sess.Ref)
	if err != nil {
		return err
	}
	_, err = c.tmuxClient.NewWindow(ctx, session, name, sess.Workspace)
	return err
}
