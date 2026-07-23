package projects

import (
	"context"
	"strings"
)

// PullStatus classifies the outcome of pulling one project.
type PullStatus int

const (
	// PullUpToDate means the pull ran and the repo was already current.
	PullUpToDate PullStatus = iota
	// PullUpdated means the pull ran and brought in new commits.
	PullUpdated
	// PullSkippedDirty means the pull did not run because the working tree
	// had modified or untracked files.
	PullSkippedDirty
	// PullFailed means the pull ran and git returned an error.
	PullFailed
)

// String renders a PullStatus for logging and test failure messages.
func (s PullStatus) String() string {
	switch s {
	case PullUpToDate:
		return "up-to-date"
	case PullUpdated:
		return "updated"
	case PullSkippedDirty:
		return "skipped-dirty"
	case PullFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// PullResult is the outcome of pulling one discovered project.
type PullResult struct {
	Name   string
	Dir    string
	Status PullStatus
	Err    error
}

// PullAll walks dir (c.Dir when dir is empty) the same way Discover does, then
// runs `git pull --rebase` in every discovered repo, sequentially. A repo with
// a dirty working tree (modified or untracked files) is skipped rather than
// pulled — Ahead alone doesn't count as dirty since a rebase pull handles
// unpushed local commits fine.
func (c *Client) PullAll(ctx context.Context, dir string) ([]PullResult, error) {
	if dir == "" {
		dir = c.Dir
	}
	projs, err := c.discoverDir(ctx, dir)
	if err != nil {
		return nil, err
	}
	results := make([]PullResult, 0, len(projs))
	for _, p := range projs {
		// discoverDir includes plain non-git directories too (list/pick display
		// them, and a non-git dir carries the same zero GitStatus as a clean
		// repo). They are not repos to pull, so skip them — shelling `git pull`
		// into a non-repo errors and would misreport as PullFailed.
		if !isGitRepo(p.Dir) {
			continue
		}
		if p.Status.Modified > 0 || p.Status.Untracked > 0 {
			results = append(results, PullResult{Name: p.Name, Dir: p.Dir, Status: PullSkippedDirty})
			continue
		}
		out, err := c.run.Run(ctx, "git", "-C", p.Dir, "pull", "--rebase")
		results = append(results, PullResult{Name: p.Name, Dir: p.Dir, Status: classifyPull(out, err), Err: err})
	}
	return results, nil
}

// classifyPull maps a `git pull --rebase` invocation's output/error to a
// PullStatus. Deliberately doesn't pass --quiet to the pull itself — that
// flag suppresses the "up to date" string this depends on.
func classifyPull(out string, err error) PullStatus {
	if err != nil {
		return PullFailed
	}
	if out == "" || strings.Contains(strings.ToLower(out), "up to date") {
		return PullUpToDate
	}
	return PullUpdated
}
