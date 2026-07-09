package pr

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cameronsjo/forgectl/internal/sandbox"
)

// findingsDirPrefix is the os.MkdirTemp prefix for the local review's
// escape-hatch dir — the ONE path outside the reviewed worktree the agent may
// write to.
const findingsDirPrefix = "forgectl-findings-"

// PrepareLocalOpts are the knobs for one PrepareLocal call. There is no
// Headless field: a local session has no PostReview path to gate, so it would
// be a no-op flag.
type PrepareLocalOpts struct {
	Agent  string
	DryRun bool
}

// PrepareLocal resolves the local HEAD of the repo at path, sandboxes it into
// a throwaway worktree, applies the same reversible clean-room controls as
// Prepare (quarantine + deny-by-default allowlist), and writes a breadcrumb —
// returning the Session. Unlike Prepare, there is no GitHub round-trip
// anywhere in this path: PrepareLocal never shells out to gh.
//
// On DryRun it resolves and returns the plan and creates NOTHING: no
// worktree, no window, no breadcrumb. The only Runner calls a dry-run makes
// are the two read-only, local-only `git rev-parse` calls.
func (c *Client) PrepareLocal(ctx context.Context, path string, opts PrepareLocalOpts) (Session, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return Session{}, fmt.Errorf("resolve path %q: %w", path, err)
	}
	if err := sandbox.RejectOptionLike("path", absPath); err != nil {
		return Session{}, err
	}
	slog.Debug("Preparing local clean-room review.", "path", absPath, "dryRun", opts.DryRun)

	headRef, err := c.run.Run(ctx, "git", "-C", absPath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return Session{}, fmt.Errorf("resolve local HEAD branch: %w", err)
	}
	headOid, err := c.run.Run(ctx, "git", "-C", absPath, "rev-parse", "HEAD")
	if err != nil {
		return Session{}, fmt.Errorf("resolve local HEAD commit: %w", err)
	}

	ref := localRef(headOid)
	sess := Session{
		Ref:       ref,
		HeadRef:   headRef,
		HeadOid:   headOid,
		Agent:     opts.Agent,
		CreatedAt: time.Now().UTC(),
		DryRun:    opts.DryRun,
		Local:     true,
	}

	if opts.DryRun {
		slog.Info("Dry-run: resolved local plan, creating nothing.", "ref", ref.String(), "head", headRef)
		return sess, nil
	}

	// headOid reaches git as a positional (worktree ref); guard before sandbox
	// does its own check, mirroring Prepare's ref guard.
	if err := sandbox.RejectOptionLike("ref", headOid); err != nil {
		return Session{}, err
	}

	// alwaysClone=false: a real local directory always takes Sandbox's worktree
	// path. Pinning to the resolved headOid (not "HEAD") snapshots exactly what
	// was measured above and deliberately excludes any uncommitted/staged
	// changes — this reviews committed changes only.
	workspace, err := c.sandboxAndQuarantine(ctx, absPath, headOid, false)
	if err != nil {
		return Session{}, err
	}
	sess.Workspace = workspace

	// The escape-hatch dir is a fresh, unpredictable sibling of workspace under
	// the OS temp root — not a deterministic/reusable name, which would invite
	// a symlink-pre-plant race.
	findingsDir, err := os.MkdirTemp(filepath.Dir(workspace), findingsDirPrefix)
	if err != nil {
		// best-effort: don't let cleanup's own error shadow the error already being returned
		_ = sandbox.Teardown(ctx, c.run, workspace)
		slog.Error("Failed to create findings directory.", "workspace", workspace, "prefix", findingsDirPrefix, "error", err)
		return Session{}, fmt.Errorf("create findings dir: %w", err)
	}
	sess.FindingsDir = findingsDir

	if _, err := writeLocalAllowlist(workspace, findingsDir); err != nil {
		c.teardownLocalArtifacts(ctx, workspace, findingsDir)
		return Session{}, err
	}

	bc := Breadcrumb{
		Workspace: workspace,
		Ref:       ref.String(),
		Agent:     opts.Agent,
		CreatedAt: sess.CreatedAt,
	}
	bcPath, err := writeBreadcrumb(c.sessionsDir, ref, bc)
	if err != nil {
		c.teardownLocalArtifacts(ctx, workspace, findingsDir)
		return Session{}, err
	}
	sess.Path = bcPath

	slog.Info("Successfully prepared local clean-room review.", "ref", ref.String(), "workspace", workspace, "findings", findingsDir)
	return sess, nil
}

// teardownLocalArtifacts is PrepareLocal's failure-path cleanup: best-effort
// removal of both workspace and findingsDir. Best-effort because a caller of
// this always already has a primary error to return — neither cleanup call's
// own error should shadow it.
func (c *Client) teardownLocalArtifacts(ctx context.Context, workspace, findingsDir string) {
	_ = sandbox.Teardown(ctx, c.run, workspace)
	_ = os.RemoveAll(findingsDir)
}

// unparseableHexSentinel is the fallback Number for localRef when hexPart
// fails to parse or parses to zero. hexPart is at most 6 hex digits, so any
// successful parse is in [0, 0xFFFFFF]; this sentinel sits strictly above
// that range so it can never collide with a legitimately parsed value (e.g.
// an oid prefix "000001" parses to 1 — a fixed sentinel of 1 would collide
// with that).
const unparseableHexSentinel = 0x1000000

// localRef builds a synthetic Ref identity from a local HEAD oid: Owner is
// localOwnerSentinel (reserved — see its doc in ref.go), Repo is a 7-char
// short oid, and Number is derived from the oid's first 6 hex chars (always
// positive — parseNumber rejects Number<=0, so a fixed 0 sentinel would fail
// breadcrumb reload). Every component stays inside Ref's existing validated
// charset, so ref.String() round-trips through ParseRef exactly like a real
// PR ref. Deriving Number from the oid also keeps concurrent-session tmux
// window names (pr-<owner>-<N>) distinct per commit under review.
func localRef(oid string) Ref {
	hexPart := truncate(oid, 6)
	n, err := strconv.ParseInt(hexPart, 16, 64)
	if err != nil || n <= 0 {
		n = unparseableHexSentinel
	}
	return Ref{Owner: localOwnerSentinel, Repo: truncate(oid, 7), Number: int(n)}
}

// truncate returns s cut to at most n bytes.
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
