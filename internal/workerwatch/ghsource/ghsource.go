// Package ghsource reads a worker's PR from GitHub and its branch and repo
// from git, for internal/workerwatch. It is separate so internal/tui, which
// renders workerwatch snapshots, never links internal/pr.
package ghsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/cameronsjo/forgectl/internal/gitenv"
	"github.com/cameronsjo/forgectl/internal/pr"
	"github.com/cameronsjo/forgectl/internal/sweep"
	"github.com/cameronsjo/forgectl/internal/workerwatch"
)

const (
	maxGHOutput = 4 << 20
	// maxReviewLine cuts a review body's first line before it is parsed.
	maxReviewLine = 300
)

// prQuery reads the newest PRs from one head branch name, each PR's head
// repository, its head commit's check rollup, and the viewer's own last 20
// reviews. A fork can open a PR from a branch of the same name, so parsePR
// keeps only a PR whose head repository is the queried one. Only the
// viewer's reviews are read for markers, the rule sweep uses: anyone who can
// review a public PR can post a marker line. The author filter is checked
// again on the client, as sweep does.
const prQuery = `query($owner: String!, $name: String!, $head: String!, $author: String!) {
  repository(owner: $owner, name: $name) {
    pullRequests(headRefName: $head, last: 10, states: [OPEN, MERGED, CLOSED]) {
      nodes {
        number
        state
        isDraft
        headRefOid
        headRepository { nameWithOwner }
        commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }
        reviews(last: 20, author: $author) { nodes { body author { login } } }
      }
    }
  }
}`

var (
	sha40Re  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	branchRe = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	loginRe  = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}$`)
)

// GitHub finds a branch's PR through gh. Run should be pinned to the
// GitHub host (githubauth.Runner).
type GitHub struct {
	Run workerwatch.Runner

	mu     sync.Mutex
	viewer string
}

// PRFor returns the newest PR whose head is branch in repo (owner/name).
func (g *GitHub) PRFor(ctx context.Context, repo, branch string) (workerwatch.PR, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !pr.ValidOwnerRepoPart(owner) || !pr.ValidOwnerRepoPart(name) {
		return workerwatch.PR{}, fmt.Errorf("%q is not owner/repo", repo)
	}
	if !branchRe.MatchString(branch) || strings.HasPrefix(branch, "-") {
		return workerwatch.PR{}, errors.New("the branch name is not one gh can be asked about")
	}
	viewer, err := g.login(ctx)
	if err != nil {
		return workerwatch.PR{}, err
	}
	out, err := g.Run.Run(ctx, "gh", "api", "graphql",
		"-f", "query="+prQuery, "-f", "owner="+owner, "-f", "name="+name,
		"-f", "head="+branch, "-f", "author="+viewer)
	if err != nil {
		return workerwatch.PR{}, ghFailure(ctx, "gh api graphql")
	}
	return parsePR(repo, viewer, out)
}

type gqlPRs struct {
	Data struct {
		Repository *struct {
			PullRequests struct {
				Nodes []struct {
					Number     int    `json:"number"`
					State      string `json:"state"`
					IsDraft    bool   `json:"isDraft"`
					HeadRefOid string `json:"headRefOid"`
					HeadRepo   *struct {
						NameWithOwner string `json:"nameWithOwner"`
					} `json:"headRepository"`
					Commits struct {
						Nodes []struct {
							Commit struct {
								StatusCheckRollup *struct {
									State string `json:"state"`
								} `json:"statusCheckRollup"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits"`
					Reviews struct {
						Nodes []struct {
							Body   string `json:"body"`
							Author *struct {
								Login string `json:"login"`
							} `json:"author"`
						} `json:"nodes"`
					} `json:"reviews"`
				} `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	} `json:"data"`
	Errors []json.RawMessage `json:"errors"`
}

// parsePR turns a prQuery answer into a PR: the newest one whose head
// repository is repo. Enums are matched, the head must be a full SHA, and
// only well-formed marker lines from viewer's own reviews count.
func parsePR(repo, viewer, out string) (workerwatch.PR, error) {
	var resp gqlPRs
	if len(out) > maxGHOutput || json.Unmarshal([]byte(out), &resp) != nil || len(resp.Errors) > 0 || resp.Data.Repository == nil {
		return workerwatch.PR{}, errors.New("GitHub returned an unusable PR answer")
	}
	nodes := resp.Data.Repository.PullRequests.Nodes
	found := -1
	for i := len(nodes) - 1; i >= 0; i-- {
		if h := nodes[i].HeadRepo; h != nil && strings.EqualFold(h.NameWithOwner, repo) {
			found = i
			break
		}
	}
	if found < 0 {
		return workerwatch.PR{}, workerwatch.ErrNoPR
	}
	n := nodes[found]
	if n.Number <= 0 {
		return workerwatch.PR{}, errors.New("GitHub returned a PR with no number")
	}
	p := workerwatch.PR{Repo: repo, Number: n.Number, Draft: n.IsDraft}
	switch n.State {
	case "OPEN", "MERGED", "CLOSED":
		p.State = n.State
	}
	if sha40Re.MatchString(n.HeadRefOid) {
		p.Head = n.HeadRefOid
	}
	if len(n.Commits.Nodes) > 0 {
		if r := n.Commits.Nodes[0].Commit.StatusCheckRollup; r != nil {
			switch r.State {
			case "SUCCESS", "FAILURE", "ERROR", "PENDING", "EXPECTED":
				p.Checks = r.State
			}
		}
	}
	// Reviews come oldest first; the newest marker wins.
	for i := len(n.Reviews.Nodes) - 1; i >= 0; i-- {
		rv := n.Reviews.Nodes[i]
		if rv.Author == nil || !strings.EqualFold(rv.Author.Login, viewer) {
			continue
		}
		line, _, _ := strings.Cut(rv.Body, "\n")
		line = strings.TrimSpace(line)
		if len(line) > maxReviewLine {
			continue
		}
		m, ok := sweep.ParseMarker(line)
		if !ok {
			continue
		}
		p.Marker = &workerwatch.Marker{Reviewer: m.Reviewer, Head: m.Head, Crit: m.Crit, Imp: m.Imp, AtHead: p.Head != "" && m.Head == p.Head}
		break
	}
	return p, nil
}

// login is the authenticated gh login, read once.
func (g *GitHub) login(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.viewer != "" {
		return g.viewer, nil
	}
	out, err := g.Run.Run(ctx, "gh", "api", "graphql", "-f", "query=query { viewer { login } }")
	if err != nil {
		return "", ghFailure(ctx, "gh api graphql (viewer)")
	}
	var resp struct {
		Data struct {
			Viewer struct {
				Login string `json:"login"`
			} `json:"viewer"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(out), &resp) != nil || !loginRe.MatchString(resp.Data.Viewer.Login) {
		return "", errors.New("gh did not say which login it is using")
	}
	g.viewer = resp.Data.Viewer.Login
	return g.viewer, nil
}

func ghFailure(ctx context.Context, what string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%s failed (run it by hand to see why)", what)
}

// RepoFromRemote turns a remote URL into owner/name, for github.com only.
func RepoFromRemote(raw string) (string, bool) {
	host, owner, name, ok := pr.ParseRemoteURL(raw)
	if !ok || !strings.EqualFold(host, "github.com") || !pr.ValidOwnerRepoPart(owner) || !pr.ValidOwnerRepoPart(name) {
		return "", false
	}
	return owner + "/" + name, true
}

// GitCLI is Git over the git command line, under gitenv's Local posture (no
// fetch, no hooks, no prompts).
type GitCLI struct {
	Run gitenv.Runner
}

// Branch implements workerwatch.Git.
func (g GitCLI) Branch(ctx context.Context, worktree string) (string, error) {
	out, err := gitenv.Run(ctx, g.Run, gitenv.Local, "-C", worktree, "branch", "--show-current")
	if err != nil {
		return "", errors.New("git branch --show-current failed")
	}
	branch := strings.TrimSpace(out)
	if branch != "" && !branchRe.MatchString(branch) {
		return "", errors.New("the branch name holds characters the watch does not show")
	}
	return branch, nil
}

// Repo implements workerwatch.Git.
func (g GitCLI) Repo(ctx context.Context, worktree string) (string, error) {
	out, err := gitenv.Run(ctx, g.Run, gitenv.Local, "-C", worktree, "remote", "get-url", "origin")
	if err != nil {
		return "", errors.New("git remote get-url origin failed")
	}
	repo, ok := RepoFromRemote(strings.TrimSpace(out))
	if !ok {
		return "", workerwatch.ErrNotGitHub
	}
	return repo, nil
}
