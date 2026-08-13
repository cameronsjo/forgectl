package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/githubauth"
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

// maxGitHubQueryConcurrency caps how many gh search processes this source has
// in flight at once. Owners come from config and each contributes two queries,
// so an unbounded fan-out would let a 64-owner list spawn 128 concurrent
// subprocesses; GitHub's own rate limiter would punish that even if the
// machine tolerated it.
const maxGitHubQueryConcurrency = 8

// ErrGitHubQueriesUnavailable is the safe sentinel returned when no GitHub
// query produced anything usable. It carries no subprocess output: gh stderr
// can hold tokens and terminal control sequences, and this error is rendered.
var ErrGitHubQueriesUnavailable = errors.New("github review queries unavailable")

// GitHub enumerates open issues and PRs owner-wide via `gh search`. It is
// deliberately --owner-scoped, NOT the @me-involvement scoping pr prs/dash
// use: this is the whole-inventory view.
type GitHub struct {
	run exec.Runner
	// configured is the raw [review].owners list. Empty means "the
	// authenticated GitHub.com login", resolved per Items call.
	configured []string
}

// NewGitHub builds the source over run for the given owners. The runner is
// wrapped so every gh call it makes — owner discovery, the issue leg, and the
// shared pr.SearchPRs leg — is pinned to github.com regardless of an ambient
// GH_HOST. Without that pin an enterprise instance's rows would come back
// stamped as github.com data.
//
// Owner validation and resolution happen in Items rather than here: config is
// low-trust input headed for an argv, and a constructor with no error return
// is the wrong place to refuse it.
func NewGitHub(run exec.Runner, owners []string) *GitHub {
	return &GitHub{run: githubauth.Runner(run), configured: owners}
}

// Name implements Source.
func (g *GitHub) Name() string { return "github" }

// ghQueryResult carries one gh-search query's outcome: its label (for notes),
// the parsed items, whether it hit --limit, and any error. The error is held
// only long enough to classify the query categorically — it is never rendered.
type ghQueryResult struct {
	label     string
	items     []Item
	truncated bool
	err       error
}

// Items runs two `gh search` queries per owner (issues + PRs), bounded to
// maxGitHubQueryConcurrency in flight, and folds the results in a fixed order:
// owner order, issues before PRs, regardless of which query finishes first.
//
// A degraded query contributes a categorical note and leaves the healthy
// queries' rows intact. Items errors only when every query failed, or when the
// owner set could not be resolved at all — and those errors carry
// ErrGitHubQueriesUnavailable plus, when applicable, the context sentinel,
// never a raw runner cause.
func (g *GitHub) Items(ctx context.Context) ([]Item, []string, error) {
	owners, err := githubauth.ResolveOwners(ctx, g.run, g.configured)
	if err != nil {
		slog.Warn("Failed to resolve GitHub review owners.", "configured", len(g.configured), "error", err)
		return nil, nil, fmt.Errorf("%w: %w", ErrGitHubQueriesUnavailable, err)
	}

	type query struct {
		label string
		run   func(ctx context.Context, owner string) ([]Item, bool, error)
		owner string
	}
	var queries []query
	for _, owner := range owners {
		queries = append(queries,
			query{fmt.Sprintf("issues(%s)", owner), g.searchIssues, owner},
			query{fmt.Sprintf("prs(%s)", owner), g.searchPRs, owner},
		)
	}

	// Indexed results, not a channel: the fold order must be the query order,
	// and each goroutine owning one index makes that true without a sort. The
	// semaphore is acquired BEFORE the `go` statement so the bound caps live
	// goroutines, not merely concurrent gh processes (the same reasoning
	// projects.fanOut documents).
	results := make([]ghQueryResult, len(queries))
	sem := make(chan struct{}, maxGitHubQueryConcurrency)
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			items, truncated, err := q.run(ctx, q.owner)
			results[i] = ghQueryResult{q.label, items, truncated, err}
		}()
	}
	wg.Wait()

	var (
		notes       []string
		items       []Item
		failed      int
		sawDeadline bool
		sawCanceled bool
	)
	for _, res := range results {
		if res.err != nil {
			// The raw cause is logged, never rendered: it can carry gh stderr.
			slog.Warn("Review query degraded.", "query", res.label, "error", res.err)
			notes = append(notes, fmt.Sprintf("%s: query failed", res.label))
			failed++
			sawDeadline = sawDeadline || errors.Is(res.err, context.DeadlineExceeded)
			sawCanceled = sawCanceled || errors.Is(res.err, context.Canceled)
			continue
		}
		if res.truncated {
			notes = append(notes, fmt.Sprintf("%s: results may be truncated at %d", res.label, searchLimit))
		}
		items = append(items, res.items...)
	}
	if failed == len(queries) {
		return nil, notes, safeAggregateError(ErrGitHubQueriesUnavailable, sawDeadline, sawCanceled)
	}
	slog.Info("Successfully loaded GitHub review inventory.", "items", len(items), "owners", len(owners), "degraded_queries", failed)
	return items, notes, nil
}

// safeAggregateError joins base with whichever standard context sentinels were
// observed, in a fixed deadline-then-canceled order so the rendered error is
// identical run to run. Only the two standard sentinels are ever joined — a
// raw runner error would carry subprocess output into a public error.
func safeAggregateError(base error, deadline, canceled bool) error {
	errs := []error{base}
	if deadline {
		errs = append(errs, context.DeadlineExceeded)
	}
	if canceled {
		errs = append(errs, context.Canceled)
	}
	if len(errs) == 1 {
		return base
	}
	return errors.Join(errs...)
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
