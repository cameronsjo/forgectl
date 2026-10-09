package merge

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// The reads behind Facts. Every GraphQL connection is read in one page and
// the read refuses when GitHub says another page remains, so no verdict is
// made on a partial list. All reads go through the host-pinned gh runner.

// actorFields is an Actor selection: a user's or bot's numeric id.
const actorFields = `__typename login ... on User { databaseId } ... on Bot { databaseId }`

// DiscoverQuery lists the PRs whose head branch is $head, with the viewer
// (the operator gh is authenticated as) and the repository's identity.
const DiscoverQuery = `query($owner: String!, $name: String!, $head: String!) {
  viewer { login databaseId }
  repository(owner: $owner, name: $name) {
    databaseId
    nameWithOwner
    defaultBranchRef { name }
    pullRequests(headRefName: $head, first: 50, orderBy: {field: CREATED_AT, direction: DESC}) {
      pageInfo { hasNextPage }
      nodes {
        number
        state
        isCrossRepository
        createdAt
        headRefName
        headRepository { databaseId }
        author { __typename login ... on User { databaseId } }
      }
    }
  }
}`

// PRQuery reads one PR: its state, its reviews, review threads and
// conversation comments.
const PRQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    databaseId
    nameWithOwner
    defaultBranchRef { name }
    pullRequest(number: $number) {
      number
      url
      title
      state
      isDraft
      isCrossRepository
      baseRefName
      baseRefOid
      headRefName
      headRefOid
      mergeable
      mergeStateStatus
      changedFiles
      author { ` + actorFields + ` }
      headRepository { databaseId }
      baseRepository { databaseId }
      reviews(first: 100) {
        pageInfo { hasNextPage }
        nodes {
          author { ` + actorFields + ` }
          state
          submittedAt
          url
          body
          commit { oid }
        }
      }
      reviewThreads(first: 100) {
        pageInfo { hasNextPage }
        nodes {
          isResolved
          resolvedBy { __typename login databaseId }
          comments(first: 100) {
            pageInfo { hasNextPage }
            nodes { author { ` + actorFields + ` } body }
          }
        }
      }
      comments(first: 100) {
        pageInfo { hasNextPage }
        nodes { author { ` + actorFields + ` } body }
      }
    }
  }
}`

// ChecksQuery reads the check runs at commit $head, each with its suite's
// app and workflow run, and the PR's head and base again, so a push between
// the two queries is seen.
const ChecksQuery = `query($owner: String!, $name: String!, $number: Int!, $head: GitObjectID!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) { headRefOid baseRefOid }
    object(oid: $head) {
      __typename
      ... on Commit {
        oid
        checkSuites(first: 50) {
          pageInfo { hasNextPage }
          nodes {
            app { databaseId slug }
            workflowRun { databaseId event workflow { resourcePath } }
            checkRuns(first: 100) {
              pageInfo { hasNextPage }
              nodes { databaseId name status conclusion startedAt completedAt }
            }
          }
        }
        status { contexts { context state description } }
      }
    }
  }
}`

// ErrResponse reports a GitHub response the merge reads will not use.
var ErrResponse = errors.New("merge: unusable GitHub response")

type pageInfo struct {
	HasNextPage bool `json:"hasNextPage"`
}

// graphQLEnvelope decodes data into v after refusing any GraphQL error.
func graphQLEnvelope(data []byte, v any) error {
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphQLError  `json:"errors"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("%w: not JSON: %w", ErrResponse, err)
	}
	if len(resp.Errors) > 0 {
		return fmt.Errorf("%w: %s", ErrResponse, firstError(resp.Errors))
	}
	if len(resp.Data) == 0 || string(resp.Data) == "null" {
		return fmt.Errorf("%w: no data in the response", ErrResponse)
	}
	if err := json.Unmarshal(resp.Data, v); err != nil {
		return fmt.Errorf("%w: %w", ErrResponse, err)
	}
	return nil
}

func remaining(what string, p pageInfo) error {
	if p.HasNextPage {
		return fmt.Errorf("%w: %s has more than one page; the read refuses rather than judge part of it", ErrResponse, what)
	}
	return nil
}

// Candidate is one PR on the head branch, as discovery lists it.
type Candidate struct {
	Number            int    `json:"number"`
	State             string `json:"state"`
	IsCrossRepository bool   `json:"isCrossRepository"`
	CreatedAt         string `json:"createdAt"`
	HeadRefName       string `json:"headRefName"`
	HeadRepository    *struct {
		DatabaseID int64 `json:"databaseId"`
	} `json:"headRepository"`
	Author *Actor `json:"author"`
}

// Discovery is a DiscoverQuery response.
type Discovery struct {
	ViewerLogin string
	ViewerID    int64
	Repository  Repository
	Candidates  []Candidate
}

// DecodeDiscovery reads a DiscoverQuery response.
func DecodeDiscovery(data []byte) (Discovery, error) {
	var d struct {
		Viewer *struct {
			Login      string `json:"login"`
			DatabaseID int64  `json:"databaseId"`
		} `json:"viewer"`
		Repository *struct {
			repoNode
			PullRequests *struct {
				PageInfo pageInfo    `json:"pageInfo"`
				Nodes    []Candidate `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	}
	if err := graphQLEnvelope(data, &d); err != nil {
		return Discovery{}, err
	}
	if d.Viewer == nil || d.Viewer.DatabaseID <= 0 {
		return Discovery{}, fmt.Errorf("%w: no viewer id", ErrResponse)
	}
	if d.Repository == nil {
		return Discovery{}, fmt.Errorf("%w: no repository in the response", ErrResponse)
	}
	repo, err := d.Repository.repository()
	if err != nil {
		return Discovery{}, err
	}
	if d.Repository.PullRequests == nil {
		return Discovery{}, fmt.Errorf("%w: no pullRequests connection", ErrResponse)
	}
	if err := remaining("the head branch's pull request list", d.Repository.PullRequests.PageInfo); err != nil {
		return Discovery{}, err
	}
	return Discovery{ViewerLogin: d.Viewer.Login, ViewerID: d.Viewer.DatabaseID, Repository: repo, Candidates: d.Repository.PullRequests.Nodes}, nil
}

// repoNode is a repository's identity fields in a GraphQL response.
type repoNode struct {
	DatabaseID       int64  `json:"databaseId"`
	NameWithOwner    string `json:"nameWithOwner"`
	DefaultBranchRef *struct {
		Name string `json:"name"`
	} `json:"defaultBranchRef"`
}

func (r repoNode) repository() (Repository, error) {
	if r.DatabaseID <= 0 || !validNameWithOwner(r.NameWithOwner) {
		return Repository{}, fmt.Errorf("%w: the repository has id %d and name %q", ErrResponse, r.DatabaseID, r.NameWithOwner)
	}
	if r.DefaultBranchRef == nil || r.DefaultBranchRef.Name == "" {
		return Repository{}, fmt.Errorf("%w: the repository has no default branch", ErrResponse)
	}
	return Repository{NameWithOwner: r.NameWithOwner, DatabaseID: r.DatabaseID, DefaultBranch: r.DefaultBranchRef.Name}, nil
}

// ErrNoPR reports a head branch with no PR that passes the discovery
// filter.
var ErrNoPR = errors.New("merge: no pull request on the worker's branch")

// ErrAmbiguousPR reports more than one open PR on the head branch.
var ErrAmbiguousPR = errors.New("merge: more than one open pull request on the worker's branch")

// SelectPR applies the discovery filter, then picks the PR: same head
// branch name exactly, not cross-repository, head repository id equal to
// the base repository's, author id equal to the operator's. The filter runs
// before any ordering, so a fork's or another author's PR on the same head
// name is never picked. More than one open PR refuses; one open PR is it;
// with none open, the most recently created closed or merged one is shown.
// A PR number from a worker's report is never used.
func SelectPR(d Discovery, branch string) (Candidate, error) {
	var kept []Candidate
	for _, c := range d.Candidates {
		if c.HeadRefName != branch || c.IsCrossRepository || c.HeadRepository == nil || c.HeadRepository.DatabaseID != d.Repository.DatabaseID {
			continue
		}
		if c.Author == nil || c.Author.DatabaseID != d.ViewerID || c.Author.DatabaseID == 0 {
			continue
		}
		kept = append(kept, c)
	}
	var open []Candidate
	for _, c := range kept {
		if c.State == "OPEN" {
			open = append(open, c)
		}
	}
	switch {
	case len(open) > 1:
		nums := make([]int, len(open))
		for i, c := range open {
			nums[i] = c.Number
		}
		return Candidate{}, fmt.Errorf("%w: %v on %s; close all but one", ErrAmbiguousPR, nums, branch)
	case len(open) == 1:
		return open[0], nil
	case len(kept) == 0:
		return Candidate{}, fmt.Errorf("%w %s", ErrNoPR, branch)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		ti, _ := time.Parse(time.RFC3339, kept[i].CreatedAt)
		tj, _ := time.Parse(time.RFC3339, kept[j].CreatedAt)
		if ti.Equal(tj) {
			return kept[i].Number > kept[j].Number
		}
		return ti.After(tj)
	})
	return kept[0], nil
}

// PRRead is a PRQuery response.
type PRRead struct {
	Repository Repository
	PR         PullRequest
	Reviews    []Review
	Threads    []Thread
	Comments   []Comment
}

// DecodePR reads a PRQuery response, refusing on any remaining page.
func DecodePR(data []byte) (PRRead, error) {
	type comment struct {
		Author *Actor `json:"author"`
		Body   string `json:"body"`
	}
	var d struct {
		Repository *struct {
			repoNode
			PullRequest *struct {
				Number            int    `json:"number"`
				URL               string `json:"url"`
				Title             string `json:"title"`
				State             string `json:"state"`
				IsDraft           bool   `json:"isDraft"`
				IsCrossRepository bool   `json:"isCrossRepository"`
				BaseRefName       string `json:"baseRefName"`
				BaseRefOid        string `json:"baseRefOid"`
				HeadRefName       string `json:"headRefName"`
				HeadRefOid        string `json:"headRefOid"`
				Mergeable         string `json:"mergeable"`
				MergeStateStatus  string `json:"mergeStateStatus"`
				ChangedFiles      int    `json:"changedFiles"`
				Author            *Actor `json:"author"`
				HeadRepository    *struct {
					DatabaseID int64 `json:"databaseId"`
				} `json:"headRepository"`
				BaseRepository *struct {
					DatabaseID int64 `json:"databaseId"`
				} `json:"baseRepository"`
				Reviews *struct {
					PageInfo pageInfo `json:"pageInfo"`
					Nodes    []struct {
						Author      *Actor  `json:"author"`
						State       string  `json:"state"`
						SubmittedAt *string `json:"submittedAt"`
						URL         string  `json:"url"`
						Body        string  `json:"body"`
						Commit      *struct {
							OID string `json:"oid"`
						} `json:"commit"`
					} `json:"nodes"`
				} `json:"reviews"`
				ReviewThreads *struct {
					PageInfo pageInfo `json:"pageInfo"`
					Nodes    []struct {
						IsResolved bool   `json:"isResolved"`
						ResolvedBy *Actor `json:"resolvedBy"`
						Comments   struct {
							PageInfo pageInfo  `json:"pageInfo"`
							Nodes    []comment `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
				Comments *struct {
					PageInfo pageInfo  `json:"pageInfo"`
					Nodes    []comment `json:"nodes"`
				} `json:"comments"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := graphQLEnvelope(data, &d); err != nil {
		return PRRead{}, err
	}
	if d.Repository == nil {
		return PRRead{}, fmt.Errorf("%w: no repository in the response", ErrResponse)
	}
	repo, err := d.Repository.repository()
	if err != nil {
		return PRRead{}, err
	}
	p := d.Repository.PullRequest
	if p == nil {
		return PRRead{}, fmt.Errorf("%w: no pull request in the response", ErrResponse)
	}
	if p.Reviews == nil || p.ReviewThreads == nil || p.Comments == nil {
		return PRRead{}, fmt.Errorf("%w: the pull request has no reviews, reviewThreads or comments connection", ErrResponse)
	}
	for _, c := range []struct {
		what string
		pi   pageInfo
	}{{"the review list", p.Reviews.PageInfo}, {"the review thread list", p.ReviewThreads.PageInfo}, {"the comment list", p.Comments.PageInfo}} {
		if err := remaining(c.what, c.pi); err != nil {
			return PRRead{}, err
		}
	}
	actor := func(a *Actor) Actor {
		if a == nil {
			return Actor{}
		}
		return *a
	}
	out := PRRead{Repository: repo, PR: PullRequest{
		Number: p.Number, URL: p.URL, Title: p.Title, State: p.State, IsDraft: p.IsDraft, IsCrossRepository: p.IsCrossRepository,
		Author: actor(p.Author), BaseRefName: p.BaseRefName, BaseRefOid: p.BaseRefOid, HeadRefName: p.HeadRefName, HeadRefOid: p.HeadRefOid,
		Mergeable: p.Mergeable, MergeStateStatus: p.MergeStateStatus, ChangedFiles: p.ChangedFiles,
	}}
	if p.HeadRepository != nil {
		out.PR.HeadRepoID = p.HeadRepository.DatabaseID
	}
	if p.BaseRepository != nil {
		out.PR.BaseRepoID = p.BaseRepository.DatabaseID
	}
	if out.PR.Number <= 0 || !isSHA(out.PR.HeadRefOid) || !isSHA(out.PR.BaseRefOid) {
		return PRRead{}, fmt.Errorf("%w: the pull request has number %d, head %q and base %q", ErrResponse, out.PR.Number, out.PR.HeadRefOid, out.PR.BaseRefOid)
	}
	for _, r := range p.Reviews.Nodes {
		rv := Review{Author: actor(r.Author), State: r.State, URL: r.URL, Body: r.Body}
		if r.SubmittedAt != nil {
			rv.SubmittedAt = *r.SubmittedAt
		}
		if r.Commit != nil {
			rv.CommitOID = r.Commit.OID
		}
		out.Reviews = append(out.Reviews, rv)
	}
	for _, t := range p.ReviewThreads.Nodes {
		if err := remaining("a review thread's comment list", t.Comments.PageInfo); err != nil {
			return PRRead{}, err
		}
		th := Thread{IsResolved: t.IsResolved}
		if t.ResolvedBy != nil {
			a := *t.ResolvedBy
			th.ResolvedBy = &a
		}
		for _, c := range t.Comments.Nodes {
			th.Comments = append(th.Comments, Comment{Author: actor(c.Author), Body: c.Body})
		}
		out.Threads = append(out.Threads, th)
	}
	for _, c := range p.Comments.Nodes {
		out.Comments = append(out.Comments, Comment{Author: actor(c.Author), Body: c.Body})
	}
	return out, nil
}

// ChecksRead is a ChecksQuery response.
type ChecksRead struct {
	// HeadRefOid and BaseRefOid are the PR's, read again in this query.
	HeadRefOid string
	BaseRefOid string
	Runs       []CheckRun
	// Statuses are the commit's statuses ("context: state description"),
	// shown in status output only: they never count toward a check.
	Statuses []string
}

// DecodeChecks reads a ChecksQuery response for head, refusing on any
// remaining page or an object that is not that commit.
func DecodeChecks(data []byte, head string) (ChecksRead, error) {
	var d struct {
		Repository *struct {
			PullRequest *struct {
				HeadRefOid string `json:"headRefOid"`
				BaseRefOid string `json:"baseRefOid"`
			} `json:"pullRequest"`
			Object *struct {
				Typename    string `json:"__typename"`
				OID         string `json:"oid"`
				CheckSuites *struct {
					PageInfo pageInfo `json:"pageInfo"`
					Nodes    []struct {
						App *struct {
							DatabaseID int64 `json:"databaseId"`
						} `json:"app"`
						WorkflowRun *struct {
							Event    string `json:"event"`
							Workflow *struct {
								ResourcePath string `json:"resourcePath"`
							} `json:"workflow"`
						} `json:"workflowRun"`
						CheckRuns struct {
							PageInfo pageInfo `json:"pageInfo"`
							Nodes    []struct {
								DatabaseID int64  `json:"databaseId"`
								Name       string `json:"name"`
								Status     string `json:"status"`
								Conclusion string `json:"conclusion"`
								StartedAt  string `json:"startedAt"`
							} `json:"nodes"`
						} `json:"checkRuns"`
					} `json:"nodes"`
				} `json:"checkSuites"`
				Status *struct {
					Contexts []struct {
						Context     string `json:"context"`
						State       string `json:"state"`
						Description string `json:"description"`
					} `json:"contexts"`
				} `json:"status"`
			} `json:"object"`
		} `json:"repository"`
	}
	if err := graphQLEnvelope(data, &d); err != nil {
		return ChecksRead{}, err
	}
	if d.Repository == nil || d.Repository.PullRequest == nil || d.Repository.Object == nil {
		return ChecksRead{}, fmt.Errorf("%w: no pull request or commit in the checks response", ErrResponse)
	}
	o := d.Repository.Object
	if o.Typename != "Commit" || o.OID != head || o.CheckSuites == nil {
		return ChecksRead{}, fmt.Errorf("%w: the checks response names %s %q, expected commit %s", ErrResponse, o.Typename, o.OID, head)
	}
	if err := remaining("the commit's check suite list", o.CheckSuites.PageInfo); err != nil {
		return ChecksRead{}, err
	}
	out := ChecksRead{HeadRefOid: d.Repository.PullRequest.HeadRefOid, BaseRefOid: d.Repository.PullRequest.BaseRefOid}
	for _, s := range o.CheckSuites.Nodes {
		if err := remaining("a check suite's run list", s.CheckRuns.PageInfo); err != nil {
			return ChecksRead{}, err
		}
		var app int64
		if s.App != nil {
			app = s.App.DatabaseID
		}
		for _, r := range s.CheckRuns.Nodes {
			run := CheckRun{DatabaseID: r.DatabaseID, Name: r.Name, Status: r.Status, Conclusion: r.Conclusion, StartedAt: r.StartedAt, AppID: app}
			if s.WorkflowRun != nil {
				run.HasWorkflowRun = true
				run.Event = s.WorkflowRun.Event
				if s.WorkflowRun.Workflow != nil {
					run.WorkflowPath = s.WorkflowRun.Workflow.ResourcePath
				}
			}
			out.Runs = append(out.Runs, run)
		}
	}
	if o.Status != nil {
		for _, c := range o.Status.Contexts {
			out.Statuses = append(out.Statuses, fmt.Sprintf("%s: %s %s", c.Context, c.State, c.Description))
		}
	}
	return out, nil
}

// MaxCompareFiles is the most files GitHub's compare API lists: past it
// the list is cut short, so the count check refuses.
const MaxCompareFiles = 300

// CompareRead is a REST compare response.
type CompareRead struct {
	Status string
	// MergeBase is merge_base_commit.sha: the base side of the file list.
	MergeBase string
	Files     []File
}

// DecodeCompare reads a REST compare response. Modes are filled in later
// from the trees.
func DecodeCompare(data []byte) (CompareRead, error) {
	var d struct {
		Status          *string `json:"status"`
		MergeBaseCommit *struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
		Files []struct {
			Filename         string `json:"filename"`
			Status           string `json:"status"`
			PreviousFilename string `json:"previous_filename"`
		} `json:"files"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return CompareRead{}, fmt.Errorf("%w: compare: not JSON: %w", ErrResponse, err)
	}
	if d.Status == nil || *d.Status == "" {
		return CompareRead{}, fmt.Errorf("%w: compare: no status", ErrResponse)
	}
	out := CompareRead{Status: *d.Status}
	if d.MergeBaseCommit != nil {
		out.MergeBase = d.MergeBaseCommit.SHA
	}
	for _, f := range d.Files {
		out.Files = append(out.Files, File{Path: f.Filename, Status: f.Status, PreviousPath: f.PreviousFilename})
	}
	return out, nil
}

// DecodeTree reads a REST git tree response (one directory, not
// recursive) into entry name to mode. A truncated tree refuses.
func DecodeTree(data []byte) (map[string]string, error) {
	var d struct {
		Truncated *bool `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
		} `json:"tree"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("%w: tree: not JSON: %w", ErrResponse, err)
	}
	if d.Truncated == nil || *d.Truncated {
		return nil, fmt.Errorf("%w: the tree listing is truncated or does not say", ErrResponse)
	}
	modes := make(map[string]string, len(d.Tree))
	for _, e := range d.Tree {
		modes[e.Path] = e.Mode
	}
	return modes, nil
}
