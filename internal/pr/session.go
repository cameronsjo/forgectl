package pr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/cameronsjo/forgectl/internal/quarantine"
	"github.com/cameronsjo/forgectl/internal/sandbox"
)

// Session is one prepared (or planned, on dry-run) clean-room review. It
// carries the resolved head metadata, the sandbox workspace, and the
// breadcrumb path that anchors the later manage/teardown verbs.
type Session struct {
	Ref       Ref
	HeadRef   string // headRefName — the branch under review
	HeadOid   string // headRefOid — the exact commit
	HeadRepo  string // "owner/repo" of the head repository (may be a fork)
	Workspace string // "" on dry-run (nothing created)
	Agent     string
	Path      string // breadcrumb path; "" on dry-run
	CreatedAt time.Time
	DryRun    bool

	// FindingsDir is populated only in-process by PrepareLocal — never
	// persisted to Breadcrumb, so it is meaningful on a freshly prepared
	// Session and zero-valued after a reload via List/Attach/Teardown (same
	// pattern HeadRef/HeadOid/HeadRepo already follow).
	FindingsDir string // the one path outside Workspace the local review agent may write

	// Provenance is who WROTE the code under review, and unlike FindingsDir it
	// IS persisted — a reconstructed session must be able to answer the
	// question that gates CodexExec without re-entering preparation. It is
	// always the EFFECTIVE value (EffectiveProvenance applied), never the raw
	// declaration, so nothing downstream has to remember to normalize it.
	Provenance ReviewProvenance
}

// PrepareOpts are the knobs for one Prepare call.
type PrepareOpts struct {
	Agent    string
	DryRun   bool
	Headless bool
	// Provenance is the caller's authorship DECLARATION. Every route into
	// Prepare is a remote PR by construction, so the honest value here is
	// ReviewProvenanceThirdParty — and the zero value (unknown) refuses the
	// unconfined path just the same, which is why a caller that forgets is
	// safe rather than sorry.
	Provenance ReviewProvenance

	// RecordPath names an ALREADY-RESERVED `preparing` record to complete
	// rather than creating a fresh one: reserve wrote it under the lifecycle
	// lock to hold a slot, and Prepare moves that same file to `prepared` once
	// the workspace exists. Empty means Prepare writes its own record — the
	// shape every unreserved caller still takes.
	RecordPath string
}

// ghPRView is the subset of `gh pr view --json …` output the core consumes.
type ghPRView struct {
	HeadRefName         string `json:"headRefName"`
	HeadRefOid          string `json:"headRefOid"`
	HeadRepositoryOwner struct {
		Login string `json:"login"`
	} `json:"headRepositoryOwner"`
	HeadRepository struct {
		Name string `json:"name"`
	} `json:"headRepository"`
}

// Prepare resolves the PR head, sandboxes it into a throwaway workspace,
// applies the reversible clean-room controls (quarantine + deny-by-default
// allowlist), and writes a breadcrumb — returning the Session.
//
// On DryRun it resolves and returns the plan and creates NOTHING: no
// worktree, no window, no breadcrumb. The only Runner call a dry-run makes is
// the read-only `gh pr view`.
func (c *Client) Prepare(ctx context.Context, ref Ref, opts PrepareOpts) (Session, error) {
	if !ref.Complete() {
		return Session{}, fmt.Errorf("PR reference %+v is missing owner/repo; resolve it first", ref)
	}
	// Refuse an unconfinable agent BEFORE the gh round-trip: a remote head is a
	// third party's content, and there is no reason to fetch and check it out
	// only to refuse at dispatch. Launch re-checks — this one is the fast fail,
	// not the authoritative gate. It precedes the DryRun branch deliberately,
	// so `--dry-run` reports the refusal rather than printing a plan that
	// cannot run.
	//
	// EffectiveProvenance is what makes a declaration unforgeable here: this is
	// the remote route, so a ref reaching it is never local, and any
	// operator-authored claim — a caller bug, a copied literal, a future route
	// that declares wrongly — is downgraded to third-party before the check
	// rather than trusted.
	provenance := EffectiveProvenance(ref, opts.Provenance)
	if err := CheckAgentForReview(opts.Agent, provenance); err != nil {
		return Session{}, err
	}
	slog.Debug("Preparing to set up clean-room review.", "ref", ref.String(), "dryRun", opts.DryRun)

	view, err := c.viewPR(ctx, ref)
	if err != nil {
		return Session{}, err
	}

	headOwner := firstNonEmpty(view.HeadRepositoryOwner.Login, ref.Owner)
	headName := firstNonEmpty(view.HeadRepository.Name, ref.Repo)
	sess := Session{
		Ref:        ref,
		HeadRef:    view.HeadRefName,
		HeadOid:    view.HeadRefOid,
		HeadRepo:   headOwner + "/" + headName,
		Agent:      opts.Agent,
		CreatedAt:  time.Now().UTC(),
		DryRun:     opts.DryRun,
		Provenance: provenance,
	}

	if opts.DryRun {
		slog.Info("Dry-run: resolved plan, creating nothing.", "ref", ref.String(), "head", sess.HeadRef)
		return sess, nil
	}

	// The head repo owner/name and branch come from gh JSON — hostile input.
	// The owner/name become path segments of a URL that reaches git as a
	// positional, so validate each against the same anchored owner/repo charset
	// guard the ref path uses (a RejectOptionLike on the assembled https:// URL
	// is dead — it always begins with "https", never "-"). The branch reaches
	// git as its own positional, so it still gets the option-like guard.
	if !ValidOwnerRepoPart(headOwner) || !ValidOwnerRepoPart(headName) {
		return Session{}, fmt.Errorf("PR head repo %q/%q outside allowed owner/repo charset", headOwner, headName)
	}
	repoURL := "https://github.com/" + headOwner + "/" + headName
	if err := sandbox.RejectOptionLike("ref", view.HeadRefName); err != nil {
		return Session{}, err
	}

	workspace, err := c.sandboxAndQuarantine(ctx, repoURL, view.HeadRefName, true)
	if err != nil {
		return Session{}, err
	}
	sess.Workspace = workspace

	if _, err := writeAllowlist(workspace); err != nil {
		_ = sandbox.Teardown(ctx, c.run, workspace)
		return Session{}, err
	}

	bc := Breadcrumb{
		Workspace:  workspace,
		Ref:        ref.String(),
		Agent:      opts.Agent,
		CreatedAt:  sess.CreatedAt,
		Provenance: provenance.persisted(),
		// Local stays false: this is a remote PR. The zero value is the
		// deliberate answer here, not an omission.
		Version:  breadcrumbVersion,
		Phase:    PhasePrepared,
		Revision: 1,
	}
	path, createdAt, err := c.recordPrepared(ctx, ref, bc, opts.RecordPath)
	if err != nil {
		_ = sandbox.Teardown(ctx, c.run, workspace)
		return Session{}, err
	}
	sess.Path = path
	sess.CreatedAt = createdAt

	slog.Info("Successfully prepared clean-room review.", "ref", ref.String(), "workspace", workspace)
	return sess, nil
}

// recordPrepared lands the `prepared` record for a freshly built clean room —
// either by completing the reservation at recordPath, or by writing a new
// record when the caller did not reserve one.
//
// Completing a reservation is a TRANSITION, not a rewrite: the reserved file
// already holds the slot and its name encodes the reservation's timestamp, so
// the workspace is written INTO it rather than beside it. That is what keeps
// the slot accounted for exactly once from reserve through teardown. It
// returns the record's own createdAt so the Session and the file on disk agree
// about when the session began.
func (c *Client) recordPrepared(ctx context.Context, ref Ref, bc Breadcrumb, recordPath string) (string, time.Time, error) {
	if recordPath == "" {
		path, err := c.writeBreadcrumb(ctx, ref, bc)
		return path, bc.CreatedAt, err
	}
	var createdAt time.Time
	err := c.transition(ctx, recordPath, PhasePreparing, PhasePrepared, func(rec *Breadcrumb) error {
		rec.Workspace = bc.Workspace
		rec.Agent = bc.Agent
		rec.Provenance = bc.Provenance
		rec.Local = bc.Local
		createdAt = rec.CreatedAt
		return nil
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return recordPath, createdAt, nil
}

// sandboxAndQuarantine creates the workspace via sandbox.Sandbox and
// quarantines any AI-instruction files it may carry — the shared head of
// Prepare's and PrepareLocal's clean-room pipeline, so the security-critical
// sequence (and its teardown-on-failure discipline) has exactly one owner. On
// failure it tears down whatever it created before returning.
func (c *Client) sandboxAndQuarantine(ctx context.Context, repo, ref string, alwaysClone bool) (string, error) {
	workspace, err := sandbox.Sandbox(ctx, c.run, repo, ref, alwaysClone)
	if err != nil {
		return "", fmt.Errorf("sandbox: %w", err)
	}
	// Expand the nestable basenames so a PR head carrying `src/AGENTS.md` is
	// quarantined too, not just the root pair. Teardown recomputes through the
	// same ExpandTargets call, which keeps the stable logical target and move
	// graph reversible across the sandbox's lifetime.
	targets, err := quarantine.ExpandTargets(workspace, quarantine.SuffixQuarantined, quarantine.DefaultTargets)
	if err != nil {
		// best-effort: don't let cleanup's own error shadow the error already being returned
		_ = sandbox.Teardown(ctx, c.run, workspace)
		return "", fmt.Errorf("expand quarantine targets: %w", err)
	}
	if _, err := quarantine.New(c.run).Hide(ctx, workspace, quarantine.SuffixQuarantined, targets, false); err != nil {
		// best-effort: don't let cleanup's own error shadow the error already being returned
		_ = sandbox.Teardown(ctx, c.run, workspace)
		return "", fmt.Errorf("quarantine workspace: %w", err)
	}
	return workspace, nil
}

// viewPR fetches the head metadata for ref via gh. The PR number is a positional
// int and the repo slug is charset-validated, so neither can smuggle a flag.
func (c *Client) viewPR(ctx context.Context, ref Ref) (ghPRView, error) {
	out, err := c.run.Run(ctx, "gh", "pr", "view", fmt.Sprintf("%d", ref.Number),
		"--repo", ref.Slug(),
		"--json", "headRefName,headRefOid,headRepositoryOwner,headRepository")
	if err != nil {
		return ghPRView{}, fmt.Errorf("gh pr view %s: %w", ref.String(), err)
	}
	var view ghPRView
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		return ghPRView{}, fmt.Errorf("parse gh pr view output: %w", err)
	}
	if view.HeadRefName == "" {
		return ghPRView{}, fmt.Errorf("gh pr view %s returned no head ref", ref.String())
	}
	return view, nil
}

// firstNonEmpty returns a if non-empty, else b.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Reserve claims one review slot for ref and writes the `preparing` record
// that holds it — reserve (admission.go) exported for callers outside this
// package. Every launch path but the bulk PrepareMany fan-out (which shares
// one reservation across a batch under a single lock hold) reserves through
// here: `pr <ref>` and `pr local` each claim exactly one slot per call, before
// Prepare/PrepareLocal completes the same record with PrepareOpts.RecordPath.
//
// A cap refusal surfaces as *errReviewCapReached, unexported; callers outside
// this package read it through ReviewCapReached rather than a type assertion.
func (c *Client) Reserve(ctx context.Context, ref Ref, cfgMax int, opts PrepareOpts) (string, error) {
	return c.reserve(ctx, ref, cfgMax, opts)
}

// ReviewCapReached reports whether err is (or wraps) the admission refusal
// Reserve returns at the concurrency cap, and if so, the numbers the
// three-line CLI refusal renders.
func ReviewCapReached(err error) (maxN, liveN int, ok bool) {
	var capErr *errReviewCapReached
	if errors.As(err, &capErr) {
		return capErr.Max, capErr.Live, true
	}
	return 0, 0, false
}

// ResolveLocalHead resolves path's local HEAD commit and returns both the
// synthetic local Ref PrepareLocal would derive for it and the full oid that
// Ref was derived from — the read-only half of PrepareLocal's opening git
// rev-parse pair (headOid only; the branch name is not needed to derive a
// Ref), exposed so a caller can Reserve a slot BEFORE entering PrepareLocal,
// exactly as `pr <ref>` reserves against a Ref it already has from ResolveRef.
//
// The oid comes back alongside the Ref because a Ref cannot carry it: a local
// Ref stores only the first 7 hex characters (newLocalRef, local.go). The
// caller threads the full oid into PrepareLocalOpts.HeadOid so HEAD is read
// exactly ONCE per `pr local` run — a HEAD that moved between two reads would
// key the reservation and the record to one commit while the workspace,
// window name, and reviewed tree were pinned to another.
//
// It runs the same location guards PrepareLocal runs (RejectOptionLike,
// rejectCleanRoomPath) so a caller cannot reserve against a path PrepareLocal
// would then refuse — the reservation and the prepare must agree on legality,
// not just on identity.
func (c *Client) ResolveLocalHead(ctx context.Context, path string) (Ref, string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return Ref{}, "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	if err := sandbox.RejectOptionLike("path", absPath); err != nil {
		return Ref{}, "", err
	}
	if err := c.rejectCleanRoomPath(absPath); err != nil {
		return Ref{}, "", err
	}
	headOid, err := c.run.Run(ctx, "git", "-C", absPath, "rev-parse", "HEAD")
	if err != nil {
		return Ref{}, "", fmt.Errorf("resolve local HEAD commit: %w", err)
	}
	return newLocalRef(headOid), headOid, nil
}

// ParkFailedReservation parks the reservation at recordPath in `needs-repair`
// with reason, for a caller whose Prepare/PrepareLocal failed AFTER Reserve
// succeeded — markNeedsRepair (phase.go) exported for callers outside this
// package.
//
// Without it the reservation stays a `preparing` record with no clean room
// behind it: it holds its slot against the cap forever and blocks every retry
// of the same ref. PrepareMany already does this inline at its own throw site
// (discover.go); the single-ref launch paths in internal/cli reach it through
// here, so a transient `gh` or clone failure costs a retry rather than a slot.
func (c *Client) ParkFailedReservation(ctx context.Context, recordPath, reason string) error {
	return c.markNeedsRepair(ctx, recordPath, reason)
}

// Queue writes a `queued` v2 record for ref under the lifecycle lock: intent
// only, no workspace, no slot, waiting for `forgectl pr drain` (#473). It
// dedups against every phase but needs-repair — naming the existing record,
// exactly as reserve does — and it refuses a local ref: PrepareLocal never
// persists FindingsDir (session.go's own Session.FindingsDir doc) and Launch
// refuses a reloaded local session, so a queued local review could never
// launch; queuing one would be a promise this build cannot keep.
func (c *Client) Queue(ctx context.Context, ref Ref, opts PrepareOpts) (string, error) {
	if ref.IsLocal() {
		return "", errors.New("local reviews cannot be queued: a local session's findings directory is never " +
			"persisted and a reloaded local session refuses to launch — review it now with 'forgectl pr local', not later")
	}
	var path string
	err := c.withLifecycleLock(ctx, "queue", func() error {
		p, err := c.queueLocked(ref, opts)
		path = p
		return err
	})
	return path, err
}

// queueLocked is Queue's core for a caller already holding the lifecycle
// lock.
func (c *Client) queueLocked(ref Ref, opts PrepareOpts) (string, error) {
	summaries, unreadable, err := c.listLocked()
	if err != nil {
		return "", err
	}
	if len(unreadable) > 0 {
		return "", fmt.Errorf("%d session record(s) could not be read, so a duplicate queue entry could not be ruled out — "+
			"settle them with 'forgectl pr repair' before queuing", len(unreadable))
	}
	if existing, ok := recordForRef(summaries, ref); ok {
		slog.Error("Refusing to queue a review: the ref already has a session record.",
			"ref", ref.String(), "path", existing.Path(), "phase", string(existing.Phase()))
		return "", duplicateRecordRefusal(ref, existing)
	}
	bc := Breadcrumb{
		Ref:        ref.String(),
		Agent:      opts.Agent,
		CreatedAt:  time.Now().UTC(),
		Local:      ref.IsLocal(),
		Provenance: EffectiveProvenance(ref, opts.Provenance).persisted(),
		Version:    breadcrumbVersion,
		Phase:      PhaseQueued,
		Revision:   1,
	}
	path, err := writeBreadcrumbFS(c.fs, c.sessionsDir, ref, bc)
	if err != nil {
		return "", err
	}
	slog.Info("Successfully queued a review for the drainer.", "ref", ref.String(), "path", path)
	return path, nil
}
