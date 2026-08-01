package pr

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	if err := c.rejectCleanRoomPath(absPath); err != nil {
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

	// A detached HEAD is the ordinary shape of `gh pr checkout` — the operator's
	// own tree sitting on a third party's commit. That is the one Codex path
	// this design deliberately does not gate (see CheckAgentForRef's ACCEPTED
	// RESIDUAL), because the directory is genuinely theirs. Warn rather than
	// refuse: silence here would read as coverage.
	if LaunchPathFor(opts.Agent) == CodexExec && headRef == "HEAD" {
		slog.Warn("Local review is on a detached HEAD, which is what `gh pr checkout` leaves behind. "+
			"If this commit came from someone else, the Codex reviewer is not confined against it — "+
			"use --agent claude instead.", "path", absPath, "head", headOid)
	}

	ref := newLocalRef(headOid)
	sess := Session{
		Ref:       ref,
		HeadRef:   headRef,
		HeadOid:   headOid,
		Agent:     opts.Agent,
		CreatedAt: time.Now().UTC(),
		DryRun:    opts.DryRun,
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

	// The escape-hatch dir lives under the client's durable findings dir
	// (config.PrFindingsDir by default) rather than as a sibling of workspace
	// under the OS temp root: findings are the deliverable of a local review
	// and must outlive the disposable workspace, so an OS tmp sweep (or the
	// same-run teardown path) must never be able to take them with it. The
	// name still carries a fresh, unpredictable random suffix rather than a
	// deterministic/reusable one, which would invite a symlink-pre-plant
	// race — 0700 ownership on the parent dir closes the rest of that gap
	// (a world-writable sticky tmp dir has no such protection).
	if err := os.MkdirAll(c.findingsDir, 0o700); err != nil {
		_ = sandbox.Teardown(ctx, c.run, workspace)
		slog.Error("Failed to create findings root dir.", "dir", c.findingsDir, "error", err)
		return Session{}, fmt.Errorf("create findings root dir: %w", err)
	}
	findingsDir, err := os.MkdirTemp(c.findingsDir, findingsDirPrefix)
	if err != nil {
		// best-effort: don't let cleanup's own error shadow the error already being returned
		_ = sandbox.Teardown(ctx, c.run, workspace)
		slog.Error("Failed to create findings directory.", "workspace", workspace, "prefix", findingsDirPrefix, "error", err)
		return Session{}, fmt.Errorf("create findings dir: %w", err)
	}
	sess.FindingsDir = findingsDir

	// The allowlist is a Claude Code control: it lands in
	// .claude/settings.local.json, which only Claude Code reads. Writing it for
	// a session that will dispatch `codex exec` leaves a file that looks like an
	// active control and enforces nothing — the next reader would reasonably
	// mistake it for the Codex reviewer's confinement. Skip it on that path
	// (LaunchPathFor is the same routing Launch uses, so the two cannot drift);
	// what does and does not confine the Codex reviewer is documented on
	// CodexExec in agent.go.
	if LaunchPathFor(opts.Agent) != CodexExec {
		if _, err := writeLocalAllowlist(workspace, findingsDir); err != nil {
			c.teardownLocalArtifacts(ctx, workspace, findingsDir)
			return Session{}, err
		}
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

// rejectCleanRoomPath refuses a `pr local` path that points into a forgectl
// clean-room workspace.
//
// `pr local` exists to review the operator's OWN tree, and that premise is what
// justifies keeping the unconfinable Codex reviewer available there. But the
// verb takes an arbitrary path with no provenance test, so without this guard
// the refusal of `--agent codex` on a remote head is a speed bump rather than a
// boundary: run `forgectl pr <ref>` (which is allowed — it dispatches Claude),
// then point `pr local --agent codex` at the workspace that now holds the
// third party's head. Same hostile diff, same unconfined reviewer, one extra
// command.
//
// Two independent checks, because each covers the other's blind spot:
//
//  1. The LIVE BREADCRUMB SET — every Workspace this client has recorded in
//     sessionsDir. This is the authoritative half and, critically, it is
//     $TMPDIR-independent: os.TempDir() reads $TMPDIR at CALL time, so a
//     workspace created under one $TMPDIR and attacked under another
//     (`TMPDIR=/tmp forgectl pr local /var/folders/…/forgectl-workflow-abc`)
//     slips past any temp-root comparison. The recorded absolute path does not
//     move.
//  2. The PREFIX SCAN — under the current temp root AND carrying the sandbox
//     prefix. This is the belt: it still catches a workspace whose breadcrumb
//     was deleted, or one left by a different forgectl invocation with its
//     own sessions dir. Unlike validateWorkspace (identity only, via the
//     sandbox prefix — see breadcrumb.go), this scan also bounds at the
//     current temp root; that's fine here, since this check only ever
//     REFUSES a path, and refusing less under a stale $TMPDIR is covered by
//     check 1 above.
//
// A breadcrumb that fails to load is skipped rather than fatal, matching List:
// one corrupt file must not make every local review unreviewable. The prefix
// scan still applies in that case.
func (c *Client) rejectCleanRoomPath(absPath string) error {
	real := absPath
	if r, err := filepath.EvalSymlinks(absPath); err == nil {
		real = r
	}
	real = filepath.Clean(real)

	if ws, ok := c.recordedWorkspaceFor(real); ok {
		return cleanRoomError(absPath, ws)
	}

	// Belt: prefix scan, bounded at the temp root so a $TMPDIR whose own path
	// happens to contain a "forgectl-*" component does not reject everything
	// under it.
	tempRoot := filepath.Clean(osTempDir())
	if r, err := filepath.EvalSymlinks(tempRoot); err == nil {
		tempRoot = r
	}
	if !sandbox.WithinWorkspace(tempRoot, real) {
		return nil
	}
	for dir := real; dir != tempRoot; dir = filepath.Dir(dir) {
		if strings.HasPrefix(filepath.Base(dir), sandboxPrefix) {
			return cleanRoomError(absPath, dir)
		}
		if parent := filepath.Dir(dir); parent == dir {
			break
		}
	}
	return nil
}

// recordedWorkspaceFor reports the recorded clean-room workspace that contains
// real, if any. Comparison is on resolved absolute paths, so it holds across a
// $TMPDIR change.
//
// It deliberately does NOT go through List/loadBreadcrumb. That path runs
// validateBreadcrumb, whose workspace check used to require the recorded
// directory to sit under osTempDir() — which reads $TMPDIR at call time — so
// under a $TMPDIR change List() would skip every breadcrumb and the
// authoritative half would see nothing. That dependency (which also blinded
// `pr list`, `pr teardown`, and `pr cleanup` after a $TMPDIR change) is gone:
// validateWorkspace no longer compares against the current temp root, only
// the recorded sandbox prefix. This function still bypasses List/loadBreadcrumb
// on its own merits — it needs the recorded string even when the workspace no
// longer exists on disk (loadBreadcrumb's Stat would reject it), and going
// straight to the breadcrumb set avoids paying content-schema validation for a
// comparison that only ever refuses, never acts.
//
// Reading with the LOCATION guard but not the workspace-schema guard is safe
// for this use: the recorded string is only ever compared against a candidate
// path in order to REFUSE. It never becomes an argv, and a broader view here
// can only refuse more, never act on more.
func (c *Client) recordedWorkspaceFor(real string) (string, bool) {
	entries, err := os.ReadDir(c.sessionsDir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("Could not read session breadcrumbs while vetting a local review path.", "error", err)
		}
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(c.sessionsDir, e.Name())
		if !sandbox.WithinWorkspace(c.sessionsDir, path) {
			continue // same location guard loadBreadcrumb applies first
		}
		data, err := os.ReadFile(path) //nolint:gosec // location-validated above
		if err != nil {
			continue
		}
		var bc Breadcrumb
		if err := json.Unmarshal(data, &bc); err != nil || bc.Workspace == "" {
			continue
		}
		ws := filepath.Clean(bc.Workspace)
		if r, err := filepath.EvalSymlinks(ws); err == nil {
			ws = r
		}
		if real == ws || sandbox.WithinWorkspace(ws, real) {
			return ws, true
		}
	}
	return "", false
}

func cleanRoomError(absPath, workspace string) error {
	return fmt.Errorf(
		"refusing to review %q: it is inside a forgectl clean-room workspace (%q), which holds content fetched from somewhere else. "+
			"`pr local` reviews your own working tree — point it at your repo, not at a review sandbox",
		absPath, workspace,
	)
}

// teardownLocalArtifacts is PrepareLocal's failure-path cleanup: best-effort
// removal of both workspace and findingsDir. Best-effort because a caller of
// this always already has a primary error to return — neither cleanup call's
// own error should shadow it.
func (c *Client) teardownLocalArtifacts(ctx context.Context, workspace, findingsDir string) {
	_ = sandbox.Teardown(ctx, c.run, workspace)
	_ = os.RemoveAll(findingsDir)
}

// unparseableHexSentinel is the fallback Number for newLocalRef when hexPart
// fails to parse or parses to zero. hexPart is at most 6 hex digits, so any
// successful parse is in [0, 0xFFFFFF]; this sentinel sits strictly above
// that range so it can never collide with a legitimately parsed value (e.g.
// an oid prefix "000001" parses to 1 — a fixed sentinel of 1 would collide
// with that).
const unparseableHexSentinel = 0x1000000

// localRef is the ONLY constructor that may produce a Ref carrying the
// reserved sentinel. It builds the identity in-process from a local git oid —
// nothing external reaches it — which is what makes locality unforgeable: the
// exported routes (ParseRef, RefFromParts, ResolveRef) all refuse the
// sentinel, so no gh response, config value, or user-typed string can spell it.
//
// It builds a synthetic Ref identity from a local HEAD oid: Owner is
// localOwnerSentinel (reserved — see its doc in ref.go), Repo is a 7-char
// short oid, and Number is derived from the oid's first 6 hex chars (always
// positive — parseNumber rejects Number<=0, so a fixed 0 sentinel would fail
// breadcrumb reload). Every component stays inside Ref's existing validated
// charset, so ref.String() round-trips through parseRefAllowingLocal exactly
// like a real PR ref does through ParseRef. Deriving Number from the oid also keeps concurrent-session tmux
// window names (pr-<owner>-<N>) distinct per commit under review.
func newLocalRef(oid string) Ref {
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
