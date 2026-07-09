package pr

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/quarantine"
	"github.com/cameronsjo/forgectl/internal/sandbox"
)

// Teardown discards the review session recorded at path: it removes the
// sandbox workspace, restores the quarantined instruction files, kills the
// tmux window, and deletes the breadcrumb.
//
// path MUST be an EXACT-MATCH member of the current breadcrumb set — a
// set-membership check, never a glob or a prefix match — so code under review
// cannot invoke teardown against an arbitrary path. Membership is resolved
// against the live session-state dir listing (symlink-resolved), and the
// breadcrumb is then loaded through the same location+content validation as
// every other consumer.
func (c *Client) Teardown(ctx context.Context, path string) error {
	if err := c.assertMember(path); err != nil {
		return err
	}
	sess, err := c.loadSession(path)
	if err != nil {
		return err
	}
	return c.discard(ctx, sess)
}

// assertMember rejects any path that is not an exact member of the known
// breadcrumb set. The comparison is on symlink-resolved absolute paths, so
// neither a symlink nor a "./" alias nor a glob can slip a non-member through.
func (c *Client) assertMember(path string) error {
	want := resolvePath(path)
	entries, err := os.ReadDir(c.sessionsDir)
	if err != nil {
		return fmt.Errorf("read pr sessions dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		if resolvePath(filepath.Join(c.sessionsDir, e.Name())) == want {
			return nil
		}
	}
	slog.Error("Teardown target is not a known session breadcrumb; refusing.", "path", path)
	return fmt.Errorf("%q is not a known pr session breadcrumb", path)
}

// resolvePath returns the symlink-resolved absolute form of path, falling back
// to a lexical Clean+Abs when resolution fails (a not-yet-created path).
func resolvePath(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	if abs, err := filepath.Abs(path); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(path)
}

// discard performs the actual teardown for an already-validated session: undo
// the quarantine (recomputed precisely from the sandbox's canonical
// scheme+targets), remove the workspace, kill the window, delete the
// breadcrumb.
func (c *Client) discard(ctx context.Context, sess Session) error {
	slog.Debug("Preparing to tear down review session.", "ref", sess.Ref.String(), "workspace", sess.Workspace)

	// Restore quarantined files first, while the workspace still exists.
	moves, err := quarantine.ComputeMoves(sess.Workspace, quarantine.SuffixQuarantined, quarantine.DefaultTargets)
	if err != nil {
		return fmt.Errorf("recompute quarantine moves: %w", err)
	}
	if err := quarantine.New(c.run).Restore(ctx, moves); err != nil {
		return fmt.Errorf("restore quarantined files: %w", err)
	}

	if err := sandbox.Teardown(ctx, c.run, sess.Workspace); err != nil {
		return fmt.Errorf("teardown workspace: %w", err)
	}

	// Best-effort: kill the review window if it is still open.
	if _, err := c.run.Run(ctx, "tmux", "kill-window", "-t", c.windowTarget(sess.Ref)); err != nil {
		slog.Debug("No review window to kill (already gone).", "target", c.windowTarget(sess.Ref), "error", err)
	}

	if sess.Path != "" {
		if err := os.Remove(sess.Path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove breadcrumb %s: %w", sess.Path, err)
		}
	}
	slog.Info("Successfully tore down review session.", "ref", sess.Ref.String())
	return nil
}

// Cleanup discards every session created on the given date (YYYY-MM-DD, UTC).
// It reuses the validated List, so only genuine breadcrumbs are touched, and
// each teardown goes through the same exact-match membership guard.
func (c *Client) Cleanup(ctx context.Context, date string) error {
	sessions, err := c.List()
	if err != nil {
		return err
	}
	var discarded int
	var firstErr error
	for _, sess := range sessions {
		if sess.CreatedAt.UTC().Format("2006-01-02") != date {
			continue
		}
		if err := c.Teardown(ctx, sess.Path); err != nil {
			slog.Error("Failed to tear down session during cleanup.", "path", sess.Path, "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		discarded++
	}
	slog.Info("Cleanup complete.", "date", date, "discarded", discarded)
	return firstErr
}
