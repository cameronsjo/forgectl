// Package branch is the ops layer for `forgectl branch`: enumerate local and
// remote-tracking git branches, classify each against SERVER-SIDE PR truth,
// and prune the ones that are actually safe to delete. It knows nothing of
// Cobra — that decoupling is the house pattern (see internal/net,
// internal/docker).
//
// forgectl branch ships FLAT — no `forgectl git` parent command. If future
// git verbs (tag prune, stash sweep, …) show up, that's the moment to
// reconsider a parent group; one verb doesn't justify it yet.
//
// This package exists specifically to encode five subtle git/gh gotchas as
// enforced behavior, not just documentation. branch_test.go asserts each one
// against exec.FakeRunner.Calls:
//
//  1. `git branch --merged <default>` misses squash-merged branches — a
//     squash-merged commit is never a literal ancestor of the default branch.
//     Merged-ness is decided by the PR's server-side state (`gh pr list
//     --state merged`), never local ancestry alone. Info.MergedLocally is
//     reported for visibility but Classify never reads it.
//  2. A branch an OPEN PR is based on must never be deleted underneath it —
//     Prune refuses anything Classify didn't mark SafeToDelete, and Classify
//     marks any branch with an open PR Blocked before it even looks at merge
//     state.
//  3. `git worktree remove` MUST run (and succeed) BEFORE `git branch
//     -d`/`-D` for a worktree-attached branch — deleteLocal enforces that
//     order unconditionally; attempting the reverse fails git itself
//     ("used by worktree at …"), but getting the ORDER right belongs to the
//     tool, not the operator.
//  4. Remote-deletion verification uses the SINGULAR
//     repos/{owner}/{repo}/git/ref/heads/{branch} endpoint. The PLURAL
//     repos/{owner}/{repo}/git/refs/heads/{branch} form is a prefix-match LIST
//     endpoint that returns 200 with an empty array for a branch that no
//     longer exists — trusting it would silently report a failed delete as a
//     success.
//  5. A local `git branch -D` (force, not `-d`) runs ONLY once
//     Info.MergedOnServer is true (rule 1's server truth) — `-D` is
//     deliberate, not a shortcut: git's own `-d` ancestry check would refuse a
//     squash-merged branch for the exact reason `--merged` misses it.
package branch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/sandbox"
)

// defaultRemoteName and defaultBranchName are New's built-in defaults, both
// overridable via Option and per-call via EnumerateOptions/PruneOptions.
const (
	defaultRemoteName = "origin"
	defaultBranchName = "main"
)

// ghListLimit bounds every `gh pr list` query so a very active repo doesn't
// silently truncate at gh's default of 30.
const ghListLimit = "500"

// Client enumerates and prunes git branches. Every git/gh shell-out goes
// through the exec.Runner seam, never os/exec directly.
type Client struct {
	run exec.Runner

	remoteName    string
	defaultBranch string
}

// Option configures a Client at construction.
type Option func(*Client)

// WithRemoteName overrides the default remote New assumes ("origin") when a
// call doesn't specify its own.
func WithRemoteName(name string) Option {
	return func(c *Client) { c.remoteName = name }
}

// WithDefaultBranch overrides the default (protected) branch New assumes
// ("main") when a call doesn't specify its own.
func WithDefaultBranch(name string) Option {
	return func(c *Client) { c.defaultBranch = name }
}

// New builds a Client over the given Runner.
func New(run exec.Runner, opts ...Option) *Client {
	c := &Client{
		run:           run,
		remoteName:    defaultRemoteName,
		defaultBranch: defaultBranchName,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// EnumerateOptions configures Enumerate.
type EnumerateOptions struct {
	// Local includes branches from refs/heads.
	Local bool
	// Remote includes branches from refs/remotes/<RemoteName>.
	Remote bool
	// RemoteName overrides the Client's configured remote for this call.
	RemoteName string
	// DefaultBranch overrides the Client's configured default (protected)
	// branch for this call.
	DefaultBranch string
	// IncludeGone additionally surfaces upstream-gone branches that have no
	// server-confirmed merge (NeedsAttention). Without it, a branch that is
	// merely "gone" with no PR evidence either way is omitted from the report
	// entirely rather than adding noise for every stale feature branch.
	IncludeGone bool
}

// Enumerate gathers local/remote branches, worktree attachments, and
// open/merged PR state from git and gh, classifies every branch, and returns
// the grouped Report — `forgectl branch`'s dry-run output.
func (c *Client) Enumerate(ctx context.Context, opts EnumerateOptions) (Report, error) {
	remoteName := firstNonEmpty(opts.RemoteName, c.remoteName)
	defaultBranch := firstNonEmpty(opts.DefaultBranch, c.defaultBranch)
	if err := sandbox.RejectOptionLike("remote", remoteName); err != nil {
		return Report{}, err
	}
	if err := sandbox.RejectOptionLike("default-branch", defaultBranch); err != nil {
		return Report{}, err
	}

	slog.Debug("Preparing to enumerate branches.", "local", opts.Local, "remote", opts.Remote, "remoteName", remoteName, "defaultBranch", defaultBranch, "includeGone", opts.IncludeGone)

	worktreeByBranch, err := c.worktrees(ctx)
	if err != nil {
		return Report{}, err
	}

	openByHead, err := c.prHeadsByState(ctx, "open")
	if err != nil {
		slog.Warn("Failed to list open PRs; open-PR blocking may be incomplete.", "error", err)
		openByHead = nil
	}
	mergedByHead, err := c.prHeadsByState(ctx, "merged")
	if err != nil {
		slog.Warn("Failed to list merged PRs; server-confirmed-merge detection may be incomplete.", "error", err)
		mergedByHead = nil
	}

	infos := make(map[string]*Info)

	if opts.Local {
		rows, err := c.localBranches(ctx)
		if err != nil {
			return Report{}, err
		}
		for _, row := range rows {
			infos[row.name] = &Info{
				Name:         row.name,
				LocalExists:  true,
				UpstreamGone: strings.Contains(row.track, "[gone]"),
			}
		}
	}

	if opts.Remote {
		names, err := c.remoteBranches(ctx, remoteName)
		if err != nil {
			return Report{}, err
		}
		for _, name := range names {
			info, ok := infos[name]
			if !ok {
				info = &Info{Name: name}
				infos[name] = info
			}
			info.RemoteExists = true
		}
	}

	// git branch --merged <default>: the LOCAL-only signal (see gotcha #1).
	// Reported on Info for visibility; Classify never reads it as a delete
	// signal.
	mergedLocally, err := c.mergedLocally(ctx, defaultBranch)
	if err != nil {
		slog.Warn("Failed to compute git branch --merged; MergedLocally will read false for every branch.", "error", err)
		mergedLocally = nil
	}

	names := make([]string, 0, len(infos))
	for name := range infos {
		names = append(names, name)
	}
	sort.Strings(names)

	toClassify := make([]Info, 0, len(names))
	for _, name := range names {
		info := infos[name]
		info.Protected = name == defaultBranch
		info.MergedLocally = mergedLocally[name]
		info.MergedOnServer = mergedByHead[name] != 0
		info.OpenPRNumber = openByHead[name]
		info.WorktreePath = worktreeByBranch[name]

		if info.UpstreamGone && !info.MergedOnServer && !opts.IncludeGone {
			slog.Debug("Skipping gone-but-unconfirmed branch (pass --include-gone to surface it).", "branch", name)
			continue
		}
		toClassify = append(toClassify, *info)
	}

	report := ClassifyAll(toClassify)
	slog.Info("Successfully enumerated branches.", "total", len(toClassify), "safeToDelete", len(report.SafeToDelete), "blocked", len(report.Blocked), "needsAttention", len(report.NeedsAttention))
	return report, nil
}

// PruneOptions configures Prune.
type PruneOptions struct {
	// RemoteName overrides the Client's configured remote for this call.
	RemoteName string
	// Local deletes the local branch (git branch -D) when Info.LocalExists.
	Local bool
	// Remote deletes the remote-tracking branch (git push --delete) when
	// Info.RemoteExists, then verifies the deletion server-side.
	Remote bool
}

// PruneResult is one branch's outcome from Prune.
type PruneResult struct {
	Name    string
	Deleted bool
	Skipped bool
	Reason  string
	Err     error
}

// Prune deletes every item Classify marked SafeToDelete, per opts. Anything
// NOT SafeToDelete is refused and recorded as Skipped — this is a second,
// defense-in-depth gate on top of the caller only passing report.SafeToDelete
// (gotcha #2): even if a caller mistakenly hands Prune a Blocked or
// NeedsAttention item, it never reaches git/gh argv.
func (c *Client) Prune(ctx context.Context, items []Classification, opts PruneOptions) []PruneResult {
	remoteName := firstNonEmpty(opts.RemoteName, c.remoteName)

	results := make([]PruneResult, 0, len(items))
	for _, item := range items {
		if item.Group != SafeToDelete {
			slog.Warn("Refusing to prune a non-safe-to-delete branch.", "branch", item.Info.Name, "group", item.Group, "reason", item.Reason)
			results = append(results, PruneResult{Name: item.Info.Name, Skipped: true, Reason: item.Reason})
			continue
		}

		if err := sandbox.RejectOptionLike("branch", item.Info.Name); err != nil {
			results = append(results, PruneResult{Name: item.Info.Name, Err: err})
			continue
		}

		result := PruneResult{Name: item.Info.Name}
		if opts.Local && item.Info.LocalExists {
			if err := c.deleteLocal(ctx, item.Info); err != nil {
				result.Err = err
				results = append(results, result)
				continue
			}
		}
		if opts.Remote && item.Info.RemoteExists {
			if err := c.deleteRemote(ctx, remoteName, item.Info); err != nil {
				result.Err = err
				results = append(results, result)
				continue
			}
		}
		result.Deleted = true
		results = append(results, result)
	}
	return results
}

// deleteLocal removes info's worktree (if attached) BEFORE deleting the local
// branch — gotcha #3. `git branch -d` would refuse a worktree-attached branch
// ("used by worktree at …") regardless, but getting the two calls in the
// right ORDER, unconditionally, is this function's entire job.
func (c *Client) deleteLocal(ctx context.Context, info Info) error {
	if info.WorktreePath != "" {
		slog.Debug("Preparing to remove worktree before deleting branch.", "branch", info.Name, "worktree", info.WorktreePath)
		if _, err := c.run.Run(ctx, "git", "worktree", "remove", "--", info.WorktreePath); err != nil {
			slog.Error("Failed to remove worktree.", "branch", info.Name, "worktree", info.WorktreePath, "error", err)
			return fmt.Errorf("remove worktree %s before deleting branch %s: %w", info.WorktreePath, info.Name, err)
		}
		slog.Debug("Successfully removed worktree.", "branch", info.Name, "worktree", info.WorktreePath)
	}

	// -D (force), not -d: this Info only reaches deleteLocal via Prune's
	// SafeToDelete gate, i.e. info.MergedOnServer is already true (gotcha
	// #1/#5). git's own `-d` runs a LOCAL ancestry check identical in spirit
	// to `--merged` and would refuse a squash-merged branch for the exact
	// reason gotcha #1 exists — using `-D` here is deliberate, not a shortcut
	// around a safety check we've already performed correctly, server-side.
	slog.Debug("Preparing to delete local branch.", "branch", info.Name)
	if _, err := c.run.Run(ctx, "git", "branch", "-D", "--", info.Name); err != nil {
		slog.Error("Failed to delete local branch.", "branch", info.Name, "error", err)
		return fmt.Errorf("delete local branch %s: %w", info.Name, err)
	}
	slog.Info("Successfully deleted local branch.", "branch", info.Name)
	return nil
}

// deleteRemote deletes the remote-tracking branch and verifies the deletion
// server-side via the SINGULAR ref endpoint — gotcha #4.
func (c *Client) deleteRemote(ctx context.Context, remoteName string, info Info) error {
	slog.Debug("Preparing to delete remote branch.", "remote", remoteName, "branch", info.Name)
	if _, err := c.run.Run(ctx, "git", "push", remoteName, "--delete", "--", info.Name); err != nil {
		slog.Error("Failed to delete remote branch.", "remote", remoteName, "branch", info.Name, "error", err)
		return fmt.Errorf("delete remote branch %s/%s: %w", remoteName, info.Name, err)
	}

	owner, repo, err := c.resolveOwnerRepo(ctx)
	if err != nil {
		return fmt.Errorf("resolve owner/repo to verify remote delete of %s: %w", info.Name, err)
	}
	if err := c.verifyRemoteDeleted(ctx, owner, repo, info.Name); err != nil {
		slog.Error("Failed to verify remote branch deletion.", "remote", remoteName, "branch", info.Name, "error", err)
		return err
	}
	slog.Info("Successfully deleted and verified remote branch.", "remote", remoteName, "branch", info.Name)
	return nil
}

// ownerRepoClass mirrors internal/pr's charset: gh output is hostile input,
// re-validated before it reaches a shell-out (here, a gh api URL path).
const ownerRepoClass = `[A-Za-z0-9._-]+`

var reOwnerRepo = regexp.MustCompile(`^` + ownerRepoClass + `$`)

// resolveOwnerRepo asks gh for the cwd repo's owner/name, re-validating the
// result against the same anchored charset internal/pr uses for the same
// reason: gh's own output is hostile input before it's safe to interpolate
// into another shell-out.
func (c *Client) resolveOwnerRepo(ctx context.Context) (owner, repo string, err error) {
	out, err := c.run.Run(ctx, "gh", "repo", "view", "--json", "owner,name", "-q", ".owner.login + \"/\" + .name")
	if err != nil {
		return "", "", fmt.Errorf("gh repo view: %w", err)
	}
	o, r, ok := strings.Cut(strings.TrimSpace(out), "/")
	if !ok || o == "" || r == "" {
		return "", "", fmt.Errorf("could not parse owner/repo from gh repo view output %q", out)
	}
	if !reOwnerRepo.MatchString(o) || !reOwnerRepo.MatchString(r) {
		return "", "", fmt.Errorf("origin owner/repo %q/%q outside allowed charset", o, r)
	}
	return o, r, nil
}

// verifyRemoteDeleted confirms a remote branch is actually gone by querying
// the SINGULAR repos/{owner}/{repo}/git/ref/heads/{branch} endpoint — gotcha
// #4. That endpoint 404s cleanly once the ref is gone. The PLURAL
// repos/{owner}/{repo}/git/refs/heads/{branch} form is deliberately never
// used here: it is a prefix-match LIST endpoint that returns 200 with an
// empty JSON array for a branch that no longer exists, which would mask a
// failed delete as a success.
func (c *Client) verifyRemoteDeleted(ctx context.Context, owner, repo, name string) error {
	path := fmt.Sprintf("repos/%s/%s/git/ref/heads/%s", owner, repo, name)
	out, err := c.run.Run(ctx, "gh", "api", path)
	if err == nil {
		return fmt.Errorf("remote branch %q still exists after delete (GET %s succeeded: %s)", name, path, out)
	}
	if !strings.Contains(err.Error(), "404") {
		return fmt.Errorf("verify remote branch %q deletion: %w", name, err)
	}
	return nil
}

// localRow is one parsed `git for-each-ref refs/heads` row.
type localRow struct {
	name     string
	upstream string
	track    string
}

// localBranches lists every local branch and its upstream tracking state.
func (c *Client) localBranches(ctx context.Context) ([]localRow, error) {
	out, err := c.run.Run(ctx, "git", "for-each-ref",
		"--format=%(refname:short)\t%(upstream:short)\t%(upstream:track)",
		"--", "refs/heads")
	if err != nil {
		return nil, fmt.Errorf("list local branches: %w", err)
	}
	var rows []localRow
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		row := localRow{name: parts[0]}
		if len(parts) > 1 {
			row.upstream = parts[1]
		}
		if len(parts) > 2 {
			row.track = parts[2]
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// remoteBranches lists every branch under refs/remotes/<remoteName>, strips
// the "<remoteName>/" prefix, and skips the remote's synthetic HEAD ref.
//
// refs/remotes/<remote>/HEAD is a symref (to refs/remotes/<remote>/<default>)
// rather than a real branch — but %(refname:short) renders it as the bare
// remote name ("origin"), NOT "origin/HEAD" as its full refname would
// suggest. Filtering by full refname (not the abbreviated short form) is what
// actually catches it; a naive `name == "HEAD"` check after stripping the
// "<remote>/" prefix misses this entirely, since "origin" never carries that
// prefix to strip. Caught via a manual dry-run against this repo's own
// origin remote.
func (c *Client) remoteBranches(ctx context.Context, remoteName string) ([]string, error) {
	out, err := c.run.Run(ctx, "git", "for-each-ref",
		"--format=%(refname)%09%(refname:short)",
		"--", "refs/remotes/"+remoteName)
	if err != nil {
		return nil, fmt.Errorf("list remote branches: %w", err)
	}
	prefix := remoteName + "/"
	headRef := "refs/remotes/" + remoteName + "/HEAD"
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		full := parts[0]
		if full == headRef {
			continue
		}
		short := full
		if len(parts) > 1 {
			short = parts[1]
		}
		names = append(names, strings.TrimPrefix(short, prefix))
	}
	return names, nil
}

// worktrees maps each worktree-checked-out branch name to its worktree path,
// parsed from `git worktree list --porcelain`.
func (c *Client) worktrees(ctx context.Context) (map[string]string, error) {
	out, err := c.run.Run(ctx, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", err)
	}
	result := make(map[string]string)
	var curPath string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			curPath = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch "):
			ref := strings.TrimPrefix(line, "branch ")
			name := strings.TrimPrefix(ref, "refs/heads/")
			if curPath != "" && name != "" {
				result[name] = curPath
			}
		case line == "":
			curPath = ""
		}
	}
	return result, nil
}

// mergedLocally returns the set of branch names `git branch --merged
// <defaultBranch>` reports — the LOCAL-ancestry-only signal gotcha #1 warns
// against trusting alone. Note deliberately no "--" separator before
// defaultBranch: unlike a bare positional, `git branch --merged --
// <commit-ish>` is rejected by git itself ("malformed object name --"), since
// --merged's argument is a single commit-ish, not a pathspec list.
// defaultBranch is still guarded by sandbox.RejectOptionLike in Enumerate
// before this is ever called.
func (c *Client) mergedLocally(ctx context.Context, defaultBranch string) (map[string]bool, error) {
	out, err := c.run.Run(ctx, "git", "branch", "--merged", defaultBranch, "--format=%(refname:short)")
	if err != nil {
		return nil, fmt.Errorf("list locally-merged branches: %w", err)
	}
	set := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		set[line] = true
	}
	return set, nil
}

// ghPRRow is the on-the-wire shape of one `gh pr list --json` row this
// package needs.
type ghPRRow struct {
	Number      int    `json:"number"`
	HeadRefName string `json:"headRefName"`
}

// prHeadsByState runs `gh pr list --state <state>` and returns a
// headRefName -> PR number map. state is a package-controlled literal
// ("open" or "merged"), never user input.
func (c *Client) prHeadsByState(ctx context.Context, state string) (map[string]int, error) {
	out, err := c.run.Run(ctx, "gh", "pr", "list",
		"--state", state,
		"--json", "number,headRefName",
		"--limit", ghListLimit)
	if err != nil {
		return nil, fmt.Errorf("gh pr list --state %s: %w", state, err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	var rows []ghPRRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, fmt.Errorf("parse gh pr list --state %s output: %w", state, err)
	}
	byHead := make(map[string]int, len(rows))
	for _, r := range rows {
		byHead[r.HeadRefName] = r.Number
	}
	return byHead, nil
}

// firstNonEmpty returns override if it's non-empty, else fallback.
func firstNonEmpty(override, fallback string) string {
	if override != "" {
		return override
	}
	return fallback
}
