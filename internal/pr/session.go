package pr

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
}

// PrepareOpts are the knobs for one Prepare call.
type PrepareOpts struct {
	Agent    string
	DryRun   bool
	Headless bool
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
	slog.Debug("Preparing to set up clean-room review.", "ref", ref.String(), "dryRun", opts.DryRun)

	view, err := c.viewPR(ctx, ref)
	if err != nil {
		return Session{}, err
	}

	headOwner := firstNonEmpty(view.HeadRepositoryOwner.Login, ref.Owner)
	headName := firstNonEmpty(view.HeadRepository.Name, ref.Repo)
	sess := Session{
		Ref:       ref,
		HeadRef:   view.HeadRefName,
		HeadOid:   view.HeadRefOid,
		HeadRepo:  headOwner + "/" + headName,
		Agent:     opts.Agent,
		CreatedAt: time.Now().UTC(),
		DryRun:    opts.DryRun,
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
		Workspace: workspace,
		Ref:       ref.String(),
		Agent:     opts.Agent,
		CreatedAt: sess.CreatedAt,
	}
	path, err := writeBreadcrumb(c.sessionsDir, ref, bc)
	if err != nil {
		_ = sandbox.Teardown(ctx, c.run, workspace)
		return Session{}, err
	}
	sess.Path = path

	slog.Info("Successfully prepared clean-room review.", "ref", ref.String(), "workspace", workspace)
	return sess, nil
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
	if _, err := quarantine.New(c.run).Hide(ctx, workspace, quarantine.SuffixQuarantined, quarantine.DefaultTargets, false); err != nil {
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
