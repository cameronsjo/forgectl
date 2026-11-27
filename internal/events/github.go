// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cameronsjo/forgectl/internal/sweep"
	"github.com/cameronsjo/forgectl/internal/workerwatch"
)

// citlLabel is the label cloud sessions put on a PR whose last step needs
// the operator's hands.
const citlLabel = "CITL"

// maxGHOutput bounds one gh answer; maxReviewLine cuts a review body's
// first line before the marker parse, as ghsource does.
const (
	maxGHOutput   = 4 << 20
	maxReviewLine = 300
)

var loginRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}$`)

// prQuery reads one PR by number: its state, head, the head's check rollup,
// its labels, the viewer's own last 20 reviews (only the viewer's markers
// count, the rule sweep uses), and the merge methods its repository allows.
const prQuery = `query($owner: String!, $name: String!, $number: Int!, $author: String!) {
  repository(owner: $owner, name: $name) {
    mergeCommitAllowed
    squashMergeAllowed
    rebaseMergeAllowed
    pullRequest(number: $number) {
      number
      state
      isDraft
      headRefOid
      labels(first: 30) { nodes { name } }
      commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }
      reviews(last: 20, author: $author) { nodes { body author { login } } }
    }
  }
}`

// Fetcher reads one PR's snapshot.
type Fetcher interface {
	Fetch(ctx context.Context, ref Ref) (PRState, error)
}

// GitHub is a Fetcher over gh. Run should be pinned to the GitHub host
// (githubauth.Runner). Reviewers, when set, limits which marker names count,
// as [sweep] reviewers does.
type GitHub struct {
	Run       workerwatch.Runner
	Reviewers []string
	Now       func() time.Time

	mu     sync.Mutex
	viewer string
}

type gqlPR struct {
	Data struct {
		Repository *struct {
			MergeCommitAllowed bool `json:"mergeCommitAllowed"`
			SquashMergeAllowed bool `json:"squashMergeAllowed"`
			RebaseMergeAllowed bool `json:"rebaseMergeAllowed"`
			PullRequest        *struct {
				Number     int    `json:"number"`
				State      string `json:"state"`
				IsDraft    bool   `json:"isDraft"`
				HeadRefOid string `json:"headRefOid"`
				Labels     struct {
					Nodes []struct {
						Name string `json:"name"`
					} `json:"nodes"`
				} `json:"labels"`
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
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []json.RawMessage `json:"errors"`
}

// Fetch implements Fetcher.
func (g *GitHub) Fetch(ctx context.Context, ref Ref) (PRState, error) {
	owner, name, ok := strings.Cut(ref.Repo, "/")
	if !ok {
		return PRState{}, ErrBadRef
	}
	viewer, err := g.login(ctx)
	if err != nil {
		return PRState{}, err
	}
	out, err := g.Run.Run(ctx, "gh", "api", "graphql",
		"-f", "query="+prQuery, "-f", "owner="+owner, "-f", "name="+name,
		"-F", "number="+strconv.Itoa(ref.Number), "-f", "author="+viewer)
	if err != nil {
		return PRState{}, ghFailure(ctx, "gh api graphql")
	}
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	return parsePR(ref, viewer, g.Reviewers, out, now())
}

// parsePR turns a prQuery answer into a snapshot. Enums are matched, the
// head must be a full SHA, and only well-formed markers from the viewer's
// own reviews count.
func parsePR(ref Ref, viewer string, reviewers []string, out string, now time.Time) (PRState, error) {
	var resp gqlPR
	if len(out) > maxGHOutput || json.Unmarshal([]byte(out), &resp) != nil || len(resp.Errors) > 0 || resp.Data.Repository == nil {
		return PRState{}, errors.New("GitHub returned an unusable PR answer")
	}
	repo := resp.Data.Repository
	n := repo.PullRequest
	if n == nil || n.Number != ref.Number {
		return PRState{}, errors.New("GitHub has no such PR")
	}
	s := PRState{Repo: ref.Repo, Number: ref.Number, Draft: n.IsDraft, PolledAt: now.UTC().Format(time.RFC3339)}
	if !prStates[n.State] {
		return PRState{}, errors.New("GitHub returned an unknown PR state")
	}
	s.State = n.State
	if sha40Re.MatchString(n.HeadRefOid) {
		s.Head = n.HeadRefOid
	}
	if len(n.Commits.Nodes) > 0 {
		if r := n.Commits.Nodes[0].Commit.StatusCheckRollup; r != nil && checkStates[r.State] {
			s.Checks = r.State
		}
	}
	for _, l := range n.Labels.Nodes {
		if l.Name == citlLabel {
			s.CITL = true
		}
	}
	if repo.MergeCommitAllowed {
		s.MergeMethods = append(s.MergeMethods, "merge")
	}
	if repo.RebaseMergeAllowed {
		s.MergeMethods = append(s.MergeMethods, "rebase")
	}
	if repo.SquashMergeAllowed {
		s.MergeMethods = append(s.MergeMethods, "squash")
	}
	var ms []workerwatch.Marker
	nodes := n.Reviews.Nodes
	for i := len(nodes) - 1; i >= 0; i-- {
		rv := nodes[i]
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
		ms = append(ms, workerwatch.Marker{Reviewer: m.Reviewer, Head: m.Head, Crit: m.Crit, Imp: m.Imp, AtHead: s.Head != "" && m.Head == s.Head})
	}
	if r := workerwatch.SummarizeMarkers(ms, reviewers); r != nil {
		s.Review = &Review{AtHead: r.AtHead, Stale: r.Stale, Crit: min(r.Crit, maxCount), Imp: min(r.Imp, maxCount)}
	}
	slices.Sort(s.MergeMethods)
	return s, s.Validate()
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
