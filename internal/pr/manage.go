package pr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
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
		return SessionSummary{}, fmt.Errorf("breadcrumb %s: %w", path, err)
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
	return Session{
		Ref:       ref,
		Workspace: bc.Workspace,
		Agent:     bc.Agent,
		Path:      path,
		CreatedAt: bc.CreatedAt,
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
	target := c.windowTarget(sess.Ref)
	slog.Debug("Attaching to review window.", "target", target)
	if err := c.run.RunInteractive(ctx, "tmux", "select-window", "-t", target); err != nil {
		return fmt.Errorf("select review window %q: %w — the window may predate a "+
			"forgectl upgrade that renamed review windows; relaunch the review with `pr <ref>`", target, err)
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
	// exact "forgectl" session should already exist — ensureSession is a
	// cheap has-session check in the common case, and a safety net if it was
	// killed out from under a still-valid breadcrumb.
	if err := c.ensureSession(ctx); err != nil {
		return err
	}
	_, err = c.run.Run(ctx, "tmux", "new-window", "-t", c.exactSessionTarget(),
		"-n", windowName(sess.Ref)+"-shell", "-c", sess.Workspace)
	return err
}
