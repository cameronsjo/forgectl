package pr

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
)

// List returns every valid review session recorded in the session-state dir.
// Each breadcrumb is loaded through the same location+content validation as
// every other consumer; a breadcrumb that fails validation is skipped (logged),
// not fatal — one corrupt file must not blind the whole list.
func (c *Client) List() ([]Session, error) {
	entries, err := os.ReadDir(c.sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read pr sessions dir: %w", err)
	}
	var sessions []Session
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(c.sessionsDir, e.Name())
		sess, err := c.loadSession(path)
		if err != nil {
			slog.Warn("Skipping invalid pr breadcrumb.", "path", path, "error", err)
			continue
		}
		sessions = append(sessions, sess)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].CreatedAt.After(sessions[j].CreatedAt)
	})
	return sessions, nil
}

// loadSession validates+loads the breadcrumb at path (using the client's
// session-state dir) and reconstitutes a Session from it.
func (c *Client) loadSession(path string) (Session, error) {
	bc, err := loadBreadcrumb(path, c.sessionsDir)
	if err != nil {
		return Session{}, err
	}
	// Permissive variant: a local session's breadcrumb legitimately carries the
	// reserved owner (see localOwnerSentinel).
	ref, err := parseRefAllowingLocal(bc.Ref)
	if err != nil {
		return Session{}, fmt.Errorf("breadcrumb ref: %w", err)
	}
	return Session{
		Ref:       ref,
		Workspace: bc.Workspace,
		Agent:     bc.Agent,
		Path:      path,
		CreatedAt: bc.CreatedAt,
	}, nil
}

// Attach jumps to the review window for the breadcrumb at path. It validates
// the breadcrumb (location + content) BEFORE any tmux argv is built, so a
// hostile path cannot steer tmux at an arbitrary target.
func (c *Client) Attach(ctx context.Context, path string) error {
	sess, err := c.loadSession(path)
	if err != nil {
		return err
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
		return err
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
