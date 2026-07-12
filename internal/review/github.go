package review

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/pr"
)

// searchLimit is the --limit each review query passes — gh search's own
// maximum, because owner-wide inventory legitimately runs into the hundreds
// (611 open issues at first live run). Hitting it degrades to a truncation
// note, never a silent cap; real pagination is the Phase B path if the
// inventory ever outgrows this.
const searchLimit = 1000

// issueSearchFields is the --json field set every `gh search issues` query
// requests — the issue analogue of pr's prSearchFields (no isDraft; issues
// aren't drafts).
const issueSearchFields = "number,title,url,author,updatedAt,state,labels,repository"

// GitHub enumerates open issues and PRs owner-wide via `gh search`. It is
// deliberately --owner-scoped, NOT the @me-involvement scoping pr prs/dash
// use: this is the whole-inventory view.
type GitHub struct {
	run    exec.Runner
	owners []string
}

// NewGitHub builds the source over run for the given owners. Owner validation
// happens per-query in Items (config is low-trust input headed for an argv;
// an invalid owner fails loudly there rather than being silently dropped).
func NewGitHub(run exec.Runner, owners []string) *GitHub {
	return &GitHub{run: run, owners: owners}
}

// Name implements Source.
func (g *GitHub) Name() string { return "github" }

// ghQueryResult carries one gh-search query's outcome across the fan-out
// channel: its label (for notes), the parsed items, whether it hit --limit,
// and any error.
type ghQueryResult struct {
	label     string
	items     []Item
	truncated bool
	err       error
}

// Items runs two `gh search` queries per owner (issues + PRs) concurrently on
// the Inventory model. A degraded query contributes a note; Items errors only
// when every query failed (nothing usable) or when no owners are configured.
func (g *GitHub) Items(ctx context.Context) ([]Item, []string, error) {
	if len(g.owners) == 0 {
		return nil, nil, fmt.Errorf("github source: no owners configured")
	}

	type query struct {
		label string
		run   func(ctx context.Context, owner string) ([]Item, bool, error)
		owner string
	}
	var queries []query
	for _, owner := range g.owners {
		queries = append(queries,
			query{fmt.Sprintf("issues(%s)", owner), g.searchIssues, owner},
			query{fmt.Sprintf("prs(%s)", owner), g.searchPRs, owner},
		)
	}

	ch := make(chan ghQueryResult, len(queries))
	for _, q := range queries {
		go func() {
			items, truncated, err := q.run(ctx, q.owner)
			ch <- ghQueryResult{q.label, items, truncated, err}
		}()
	}

	var notes []string
	var items []Item
	failed := 0
	for range queries {
		res := <-ch
		if res.err != nil {
			slog.Warn("Review query degraded.", "query", res.label, "error", res.err)
			notes = append(notes, fmt.Sprintf("%s: %v", res.label, res.err))
			failed++
			continue
		}
		if res.truncated {
			notes = append(notes, fmt.Sprintf("%s: results may be truncated at %d", res.label, searchLimit))
		}
		items = append(items, res.items...)
	}
	if failed == len(queries) {
		return nil, notes, fmt.Errorf("github source: every query failed")
	}
	slog.Info("Successfully loaded GitHub review inventory.", "items", len(items), "owners", len(g.owners), "degraded_queries", failed)
	return items, notes, nil
}

// searchPRs runs the PR leg for one owner through the shared pr.SearchPRs
// helper — the identical invocation/parse path pr prs/dash use — and maps the
// rows to Items.
func (g *GitHub) searchPRs(ctx context.Context, owner string) ([]Item, bool, error) {
	prs, truncated, err := pr.SearchPRs(ctx, g.run, pr.SearchOpts{Owner: owner, Limit: searchLimit})
	if err != nil {
		return nil, false, err
	}
	items := make([]Item, 0, len(prs))
	for _, p := range prs {
		items = append(items, Item{
			Kind:      KindPR,
			Host:      GitHubHost,
			Owner:     p.Ref.Owner,
			Repo:      p.Ref.Repo,
			Number:    p.Ref.Number,
			Title:     p.Title,
			Author:    p.Author,
			State:     p.State,
			IsDraft:   p.IsDraft,
			Labels:    p.Labels,
			UpdatedAt: p.UpdatedAt,
			URL:       p.URL,
		})
	}
	return items, truncated, nil
}

// ghSearchIssue is the on-the-wire shape of one `gh search issues --json …`
// row.
type ghSearchIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	UpdatedAt time.Time `json:"updatedAt"`
	State     string    `json:"state"`
	Labels    []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
}

// searchIssues runs the issue leg for one owner. The owner value comes from
// config — low-trust input headed for an argv — so it is vetted through the
// same anchored guards as every ref component before the shell-out (the PR leg
// gets the identical check inside pr.SearchPRs).
func (g *GitHub) searchIssues(ctx context.Context, owner string) ([]Item, bool, error) {
	if !pr.ValidOwnerRepoPart(owner) {
		return nil, false, fmt.Errorf("owner %q outside allowed charset", owner)
	}
	out, err := g.run.Run(ctx, "gh", "search", "issues",
		"--owner", owner,
		"--state", "open",
		"--json", issueSearchFields,
		"--limit", fmt.Sprint(searchLimit))
	if err != nil {
		return nil, false, err
	}
	items, rawCount, err := parseSearchIssues(out)
	if err != nil {
		return nil, false, err
	}
	// Raw-count comparison, mirroring pr.SearchPRs: a skipped hostile row in an
	// exactly-full response must not silence the truncation sentinel.
	return items, rawCount >= searchLimit, nil
}

// parseSearchIssues decodes `gh search issues --json` output into Items.
// Tracker output is hostile input: each row routes through the canonical
// anchored validator; an invalid row is skipped (logged), never fatal. Rows
// whose URL points at a pull request are skipped too — gh's issue search
// excludes PRs by default, but the guard keeps Kind honest if that default
// ever shifts (dedupe-by-Key already prevents a double render; this prevents
// a mislabeled one).
//
// rawCount is the PRE-filter row count for the truncation sentinel (see
// searchIssues).
func parseSearchIssues(jsonOut string) (items []Item, rawCount int, err error) {
	if strings.TrimSpace(jsonOut) == "" {
		return nil, 0, nil
	}
	var raw []ghSearchIssue
	if err := json.Unmarshal([]byte(jsonOut), &raw); err != nil {
		return nil, 0, fmt.Errorf("parse gh search issues output: %w", err)
	}
	out := make([]Item, 0, len(raw))
	for _, r := range raw {
		owner, repo, ok := strings.Cut(strings.TrimSpace(r.Repository.NameWithOwner), "/")
		if !ok || owner == "" || repo == "" {
			slog.Warn("Skipping issue with unparseable repository.", "nameWithOwner", r.Repository.NameWithOwner)
			continue
		}
		if strings.Contains(r.URL, "/pull/") {
			slog.Warn("Skipping pull request row in issue search.", "url", r.URL)
			continue
		}
		item, err := itemFromParts(KindIssue, GitHubHost, owner, repo, r.Number)
		if err != nil {
			slog.Warn("Skipping issue with invalid ref.", "nameWithOwner", r.Repository.NameWithOwner, "number", r.Number, "error", err)
			continue
		}
		labels := make([]string, 0, len(r.Labels))
		for _, l := range r.Labels {
			labels = append(labels, l.Name)
		}
		item.Title = r.Title
		item.Author = r.Author.Login
		item.State = r.State
		item.Labels = labels
		item.UpdatedAt = r.UpdatedAt
		item.URL = r.URL
		out = append(out, item)
	}
	return out, len(raw), nil
}
