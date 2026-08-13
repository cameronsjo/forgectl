package pr

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/quarantine"
	"github.com/cameronsjo/forgectl/internal/sandbox"
)

// sandboxTeardown is the workspace-removal seam. Production wires the real
// call; tests inject a mid-teardown failure, which is the only portable way to
// stage a LIVE branch that fails after mutation has begun — FakeRunner cannot
// force it, since sandbox.Teardown's failures come from the filesystem rather
// than from a Runner call. Tests must restore it and must not run in parallel
// while overriding it. Mirrors the classifier seams in workspace_state.go.
var sandboxTeardown = sandbox.Teardown

// Teardown discards the review session recorded at path.
//
// path MUST resolve to a member of the current breadcrumb set — a
// set-membership check, never a glob or a prefix match — so code under review
// cannot invoke teardown against an arbitrary path. Membership yields the
// AUTHORITATIVE file (see resolveBreadcrumbMember); nothing downstream acts on
// the caller's operand.
//
// Two branches, decided by the recorded workspace's state, and the order is
// load-bearing:
//
//	membership -> record validation -> workspace classification
//	  missing -> identity/byte/field recheck -> breadcrumb-ONLY unlink
//	  live    -> strict live reload -> quarantine restore -> sandbox teardown
//	          -> best-effort tmux kill -> breadcrumb unlink
//	  invalid -> refusal
//
// The stale decision happens BEFORE any live teardown facility, and the live
// branch never falls back to the stale unlink once quarantine restore, sandbox
// teardown, or tmux action has begun. A live-to-missing race can fail and leak
// through the existing error handling, but it cannot cross branches after
// mutation has started.
func (c *Client) Teardown(ctx context.Context, path string) error {
	// Held across membership, classification, and the final unlink so this
	// Client cannot race itself. Cleanup deliberately does NOT hold it around
	// its Teardown calls; each candidate reacquires it here.
	c.sessionsMu.Lock()
	defer c.sessionsMu.Unlock()

	member, err := c.resolveBreadcrumbMember(path)
	if err != nil {
		return err
	}

	avail, availErr := classifyWorkspace(member.breadcrumb.Workspace)
	switch avail {
	case workspaceAvailabilityMissing:
		return c.discardStale(member)
	case workspaceAvailabilityLive:
		// Strict live reload through the authoritative path — never the
		// operand — so the session acted on is the record just verified.
		sess, err := c.loadSession(member.path)
		if err != nil {
			return err
		}
		return c.discard(ctx, sess)
	default:
		return fmt.Errorf("cannot tear down breadcrumb %s: %w", member.path, availErr)
	}
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

// discardStale removes a breadcrumb whose workspace is gone — and removes
// NOTHING else.
//
// This is the one path in forgectl that deletes a file on the strength of a
// record rather than a live sandbox, so it re-proves every fact it is acting
// on immediately before acting. Everything membership captured is compared
// against the filesystem again, and every step runs through ONE pinned
// directory handle opened at the top:
//
//	os.OpenRoot(sessionsDir)             -> pin the directory for every step below
//	Lstat "." through the handle         -> SameFile as at check time
//	Lstat the member's base name         -> SameFile as at check time
//	re-read that name through the handle -> byte-identical
//	re-decode and re-validate            -> every security field identical
//	re-classify the recorded workspace   -> still cleanly missing
//	Remove that same base name
//
// PINNED HANDLE, UNRESOLVED NAME — and the pin is what makes the directory
// identity check load-bearing. The earlier form Lstat'd
// EvalSymlinks(c.sessionsDir) but unlinked through the UNRESOLVED
// c.sessionsDir: the object checked and the object acted through were
// different by construction, not merely racing, so a directory swap could
// pass the check and still be deleted from. os.Root closes that: every check
// and the unlink traverse the same file descriptor, and the unlink names only
// the member's base name.
//
// This does NOT conflict with the VALIDATE RESOLVED, ACT UNRESOLVED convention
// spelled out on validateWorkspace. That warning is about following a symlink
// to its target; os.Root REFUSES an escaping symlink rather than following it,
// so acting through the handle can never widen into a deletion elsewhere.
// OpenRoot resolves c.sessionsDir itself in the ordinary way, so a symlinked
// session directory remains supported.
//
// Any drift refuses: an identity mismatch, a symlink swapped in, a byte or
// field change, the record disappearing or being recreated, the parent
// directory swapped, an invalid decode, the workspace REAPPEARING, or any
// permission or I/O error. A refusal leaks a breadcrumb; deleting on
// uncertainty loses a record that may still describe a real review. Leaking is
// recoverable, so leaking is the failure mode chosen.
//
// It issues ZERO Runner calls and performs no quarantine restore, sandbox
// teardown, git, tmux, workspace write, or workspace removal. There is nothing
// to restore — the workspace is already gone — and a stale record must never
// be able to reach a facility that deletes directories.
//
// HONEST RESIDUAL: the pinned handle removes the parent-rename race, but Go
// still has no compare-and-unlink for the FILE itself. A same-uid actor can
// replace the breadcrumb between the final Lstat and Remove, and the unlink
// would take the replacement. That is accepted, not overlooked: the session
// directory is 0700, so such an actor can already unlink or rewrite the
// breadcrumb directly without going through forgectl. This protocol prevents
// benign in-process races and refuses observed drift; it does not claim to
// defeat a hostile same-uid concurrent writer.
func (c *Client) discardStale(member breadcrumbMember) error {
	slog.Debug("Preparing to discard a stale review breadcrumb.",
		"ref", member.breadcrumb.Ref, "path", member.path)

	root, err := os.OpenRoot(c.sessionsDir)
	if err != nil {
		return fmt.Errorf("pin pr sessions dir %s: %w", c.sessionsDir, err)
	}
	defer func() {
		if cerr := root.Close(); cerr != nil {
			slog.Debug("Failed to close the pinned pr sessions dir handle.", "error", cerr)
		}
	}()

	dirInfo, err := root.Lstat(".")
	if err != nil {
		return fmt.Errorf("re-stat pr sessions dir: %w", err)
	}
	if !os.SameFile(dirInfo, member.dirInfo) {
		return fmt.Errorf("pr sessions dir %s changed identity during teardown; refusing to remove %s",
			c.sessionsDir, member.path)
	}

	// Membership already proved this entry sits directly in the canonical
	// session directory, so its base name is the exact name to operate on
	// through the pinned handle — and a base name cannot escape it.
	name := filepath.Base(member.path)

	// A member that vanished before this check is refused rather than treated
	// as an already-successful removal: a concurrently completed teardown can
	// report its own success, and silently succeeding here would hide a
	// replacement race.
	info, err := root.Lstat(name)
	if err != nil {
		return fmt.Errorf("re-stat breadcrumb %s: %w", member.path, err)
	}
	if !os.SameFile(info, member.info) {
		return fmt.Errorf("breadcrumb %s changed identity during teardown; refusing to remove it", member.path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("breadcrumb %s is no longer a regular file; refusing to remove it", member.path)
	}

	data, err := root.ReadFile(name)
	if err != nil {
		return fmt.Errorf("re-read breadcrumb %s: %w", member.path, err)
	}
	if !bytes.Equal(data, member.bytes) {
		return fmt.Errorf("breadcrumb %s changed on disk during teardown; refusing to remove it", member.path)
	}
	bc, err := decodeBreadcrumbRecord(data, member.path)
	if err != nil {
		return fmt.Errorf("re-validate breadcrumb before removal: %w", err)
	}
	if !sameBreadcrumbRecord(bc, member.breadcrumb) {
		return fmt.Errorf("breadcrumb %s decoded differently during teardown; refusing to remove it", member.path)
	}

	// The authority for this deletion is the workspace's absence, so re-prove
	// it last, closest to the unlink. A workspace that came back means the
	// record is live again and must not be discarded as stale.
	avail, availErr := classifyWorkspace(bc.Workspace)
	if avail != workspaceAvailabilityMissing {
		return fmt.Errorf("workspace for breadcrumb %s is no longer cleanly absent; refusing to remove it: %w",
			member.path, availErr)
	}

	if err := root.Remove(name); err != nil {
		return fmt.Errorf("remove breadcrumb %s: %w", member.path, err)
	}
	slog.Info("Successfully discarded a stale review breadcrumb.", "ref", bc.Ref)
	return nil
}

// sameBreadcrumbRecord reports whether two decoded records agree on every
// field that carries security meaning. Byte equality already implies this;
// checking both means a future decoder change cannot quietly widen what
// "unchanged" means.
func sameBreadcrumbRecord(a, b Breadcrumb) bool {
	return a.Workspace == b.Workspace &&
		a.Ref == b.Ref &&
		a.Agent == b.Agent &&
		a.Local == b.Local &&
		a.CreatedAt.Equal(b.CreatedAt)
}

// discard performs the actual teardown for an already-validated session: undo
// the quarantine (recomputed precisely from the sandbox's canonical
// scheme+targets), remove the workspace, kill the window, delete the
// breadcrumb.
func (c *Client) discard(ctx context.Context, sess Session) error {
	slog.Debug("Preparing to tear down review session.", "ref", sess.Ref.String(), "workspace", sess.Workspace)

	// Restore quarantined files first, while the workspace still exists.
	// ExpandTargets is the same call sandboxAndQuarantine made, and it finds a
	// nested target by its RENAMED form as readily as its original — so the
	// stable logical target list and move graph recomputed here match the ones
	// Hide worked from. Quarantine does not promise a multi-path atomic rename
	// against concurrent writers; discard operates after that review boundary.
	targets, err := quarantine.ExpandTargets(sess.Workspace, quarantine.SuffixQuarantined, quarantine.DefaultTargets)
	if err != nil {
		return fmt.Errorf("expand quarantine targets: %w", err)
	}
	moves, err := quarantine.ComputeMoves(sess.Workspace, quarantine.SuffixQuarantined, targets)
	if err != nil {
		return fmt.Errorf("recompute quarantine moves: %w", err)
	}
	if err := quarantine.New(c.run).Restore(ctx, moves); err != nil {
		return fmt.Errorf("restore quarantined files: %w", err)
	}

	if err := sandboxTeardown(ctx, c.run, sess.Workspace); err != nil {
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

// Cleanup discards every session created on the given date (YYYY-MM-DD, UTC),
// stale records included — a day's sweep that silently skipped the leftovers
// would be the very accumulation #212 exists to end.
//
// List supplies only the CANDIDATE set and its recorded dates; it is not
// trusted for anything else. Every candidate re-enters Teardown, which
// re-resolves membership, re-validates the record, and re-classifies the
// workspace from scratch — so a record that changed between the listing and
// the teardown is judged on what it is NOW, not on what List saw.
//
// One failure is retained as the first error while later candidates continue,
// matching the existing cleanup contract.
func (c *Client) Cleanup(ctx context.Context, date string) error {
	summaries, err := c.List()
	if err != nil {
		return err
	}
	var discarded int
	var firstErr error
	for _, sum := range summaries {
		if sum.CreatedAt().UTC().Format("2006-01-02") != date {
			continue
		}
		if err := c.Teardown(ctx, sum.Path()); err != nil {
			slog.Error("Failed to tear down session during cleanup.", "path", sum.Path(), "error", err)
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
