package sweep

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
)

const (
	// maxOpenPages bounds the open-PR read: openQuery asks for 100 PRs a
	// page. A sixth page is reported as truncation, never read.
	maxOpenPages = 5
	// maxMerged is the --limit of the merged-PR search.
	maxMerged = 300
	// maxFirstLine cuts a review body's first line before it is kept.
	maxFirstLine = 300
	maxGHOutput  = 16 << 20
)

// openQuery reads open PRs with what the state rule needs: the head commit,
// mergeability, the head's check rollup and the latest 20 review bodies by
// the authenticated login only. Anyone who can review a public repo's PR can
// post a marker, so reviews by other accounts are not read at all; asking for
// the viewer's own reviews also keeps outsiders from pushing the real marker
// out of the 20. A marker pushed past 20 of the viewer's own reviews reads as
// no marker, which fails safe.
const openQuery = `query($q: String!, $after: String, $author: String!) {
  search(query: $q, type: ISSUE, first: 100, after: $after) {
    pageInfo { hasNextPage endCursor }
    nodes {
      ... on PullRequest {
        number
        title
        isDraft
        headRefOid
        mergeable
        createdAt
        repository { nameWithOwner }
        commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }
        reviews(last: 20, author: $author) { nodes { body author { login } } }
      }
    }
  }
}`

var (
	cursorRe = regexp.MustCompile(`^[A-Za-z0-9=_-]{1,200}$`)
	sha40Re  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// GitHub reads open and merged PRs through gh. Run should be pinned to the
// deployment's GitHub host (githubauth.Runner). Everything it returns is
// untrusted: repo names and numbers are validated, enums are matched, and text
// is cut here and cleaned again when it enters the ledger.
type GitHub struct {
	Run exec.Runner
}

type gqlResponse struct {
	Data struct {
		Search struct {
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
			Nodes []gqlPR `json:"nodes"`
		} `json:"search"`
	} `json:"data"`
	Errors []json.RawMessage `json:"errors"`
}

type gqlPR struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	IsDraft    bool   `json:"isDraft"`
	HeadRefOid string `json:"headRefOid"`
	Mergeable  string `json:"mergeable"`
	CreatedAt  string `json:"createdAt"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
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
}

// OpenPRs lists the owner's open PRs. truncated is true when more remained
// than maxOpenPages pages hold.
func (g GitHub) OpenPRs(ctx context.Context, owner string) (facts []PRFacts, truncated bool, err error) {
	if !pr.ValidOwnerRepoPart(owner) {
		return nil, false, errors.New("sweep: owner is not a GitHub account name")
	}
	query := "user:" + owner + " is:pr is:open archived:false"
	viewer, err := g.viewer(ctx)
	if err != nil {
		return nil, false, err
	}
	after := ""
	for page := 0; page < maxOpenPages; page++ {
		args := []string{"api", "graphql", "-f", "query=" + openQuery, "-f", "q=" + query, "-f", "author=" + viewer}
		if after != "" {
			args = append(args, "-f", "after="+after)
		}
		out, err := g.Run.Run(ctx, "gh", args...)
		if err != nil {
			return nil, false, ghFailure(ctx, "gh api graphql")
		}
		var resp gqlResponse
		if len(out) > maxGHOutput || json.NewDecoder(bytes.NewReader([]byte(out))).Decode(&resp) != nil || len(resp.Errors) > 0 {
			return nil, false, errors.New("sweep: GitHub returned an unusable open-PR answer")
		}
		for _, n := range resp.Data.Search.Nodes {
			if f, ok := n.facts(viewer); ok {
				facts = append(facts, f)
			}
		}
		pi := resp.Data.Search.PageInfo
		if !pi.HasNextPage {
			return facts, false, nil
		}
		if !cursorRe.MatchString(pi.EndCursor) {
			return facts, true, nil
		}
		after = pi.EndCursor
	}
	return facts, true, nil
}

func (n gqlPR) facts(viewer string) (PRFacts, bool) {
	if n.Number <= 0 || !validRepoName(n.Repository.NameWithOwner) {
		return PRFacts{}, false
	}
	f := PRFacts{Repo: n.Repository.NameWithOwner, Number: n.Number, Title: n.Title, Draft: n.IsDraft, CreatedAt: n.CreatedAt, Mergeable: "UNKNOWN"}
	if sha40Re.MatchString(n.HeadRefOid) {
		f.Head = n.HeadRefOid
	}
	switch n.Mergeable {
	case "MERGEABLE", "CONFLICTING":
		f.Mergeable = n.Mergeable
	}
	if len(n.Commits.Nodes) > 0 {
		if r := n.Commits.Nodes[0].Commit.StatusCheckRollup; r != nil {
			switch r.State {
			case "SUCCESS", "FAILURE", "ERROR", "PENDING", "EXPECTED":
				f.Checks = r.State
			}
		}
	}
	for _, rv := range n.Reviews.Nodes {
		if rv.Author == nil || !strings.EqualFold(rv.Author.Login, viewer) {
			continue
		}
		line, _, _ := strings.Cut(rv.Body, "\n")
		line = strings.TrimSpace(line)
		if len(line) > maxFirstLine {
			line = line[:maxFirstLine]
		}
		if line != "" {
			f.Reviews = append(f.Reviews, line)
		}
	}
	return f, true
}

// viewer is the authenticated login: the only account whose reviews are read
// for markers.
func (g GitHub) viewer(ctx context.Context) (string, error) {
	out, err := g.Run.Run(ctx, "gh", "api", "graphql", "-f", "query=query { viewer { login } }")
	if err != nil {
		return "", ghFailure(ctx, "gh api graphql")
	}
	var resp struct {
		Data struct {
			Viewer struct {
				Login string `json:"login"`
			} `json:"viewer"`
		} `json:"data"`
	}
	if json.NewDecoder(bytes.NewReader([]byte(out))).Decode(&resp) != nil || !pr.ValidOwnerRepoPart(resp.Data.Viewer.Login) {
		return "", errors.New("sweep: GitHub did not name the authenticated login")
	}
	return resp.Data.Viewer.Login, nil
}

type ghMerged struct {
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Number   int    `json:"number"`
	Title    string `json:"title"`
	ClosedAt string `json:"closedAt"`
	Author   struct {
		Login string `json:"login"`
		IsBot bool   `json:"is_bot"`
	} `json:"author"`
}

// MergedPRs lists the owner's PRs merged on or after since.
func (g GitHub) MergedPRs(ctx context.Context, owner string, since time.Time) (merged []MergedPR, truncated bool, err error) {
	if !pr.ValidOwnerRepoPart(owner) {
		return nil, false, errors.New("sweep: owner is not a GitHub account name")
	}
	out, err := g.Run.Run(ctx, "gh", "search", "prs", "--owner", owner,
		"--merged-at", ">="+since.UTC().Format(time.DateOnly),
		"--limit", fmt.Sprint(maxMerged), "--json", "repository,number,title,closedAt,author")
	if err != nil {
		return nil, false, ghFailure(ctx, "gh search prs")
	}
	var rows []ghMerged
	if len(out) > maxGHOutput || json.NewDecoder(bytes.NewReader([]byte(out))).Decode(&rows) != nil {
		return nil, false, errors.New("sweep: GitHub returned an unusable merged-PR answer")
	}
	for _, r := range rows {
		closed, perr := time.Parse(time.RFC3339, r.ClosedAt)
		if r.Number <= 0 || !validRepoName(r.Repository.NameWithOwner) || perr != nil {
			continue
		}
		merged = append(merged, MergedPR{
			Repo: r.Repository.NameWithOwner, Number: r.Number, Title: r.Title, ClosedAt: closed.UTC(),
			Bot: r.Author.IsBot || strings.HasSuffix(r.Author.Login, "[bot]"),
		})
	}
	return merged, len(rows) >= maxMerged, nil
}

// ghFailure is a categorical error: gh's own message can carry terminal
// bytes or tokens, so it is never echoed. A cancelled context is reported as
// such.
func ghFailure(ctx context.Context, what string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("sweep: %s: %w", what, err)
	}
	return fmt.Errorf("sweep: %s failed (run it by hand to see why)", what)
}

func validRepoName(s string) bool {
	owner, repo, ok := strings.Cut(s, "/")
	return ok && pr.ValidOwnerRepoPart(owner) && pr.ValidOwnerRepoPart(repo) && repo != "." && !strings.Contains(repo, "/")
}
