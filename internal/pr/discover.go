package pr

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PR is one open pull request surfaced by the discovery layer — the rich type
// Phase 1 never fetched (its core only resolves head ref/oid/repo). UpdatedAt
// is the PR's latest activity timestamp (gh's updatedAt — bumped by a push,
// comment, label, or review, not commits alone): the reviewed store compares
// its mark against it, so any newer activity auto-un-dims the row.
type PR struct {
	Ref       Ref
	Title     string
	Author    string
	State     string
	IsDraft   bool
	UpdatedAt time.Time
	URL       string
}

// Dashboard is the sectioned view `pr dash` renders: the local in-flight
// reviews (breadcrumbs, no gh), the PRs whose review is requested of you, and
// your own open PRs.
type Dashboard struct {
	ActiveReviews []Session
	AwaitingYou   []PR
	YourOpen      []PR
}

// prSearchFields is the --json field set every `gh search prs` query requests.
// repository is fetched for nameWithOwner → Ref; updatedAt drives the un-dim.
const prSearchFields = "number,title,url,author,updatedAt,isDraft,state,repository"

// prepareConcurrency bounds the total number of in-flight git checkouts across
// a PrepareMany fan-out, so a large multiselect doesn't spawn one git process
// per PR at once. Same-repo PRs are additionally serialized by a keyed mutex.
const prepareConcurrency = 4

// ghSearchPR is the on-the-wire shape of one `gh search prs --json …` row.
type ghSearchPR struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	UpdatedAt  time.Time `json:"updatedAt"`
	IsDraft    bool      `json:"isDraft"`
	State      string    `json:"state"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
}

// prQueryResult carries one gh-search query's outcome across the fan-out
// channel in PRs and Dash: its label (for a degradation note), the parsed rows,
// and any error.
type prQueryResult struct {
	label string
	prs   []PR
	err   error
}

// PRs returns the union of open PRs you authored, are assigned, or have been
// asked to review — three `gh search prs` queries fanned out concurrently on
// the Inventory model (buffered channel, degrade-to-note, fixed receive loop).
// A degraded query contributes a note, not a failure. The result is deduped by
// Ref.String() and sorted deterministically by (slug, number).
func (c *Client) PRs(ctx context.Context) ([]PR, []string, error) {
	queries := []struct {
		label string
		flag  string
	}{
		{"authored", "--author"},
		{"assigned", "--assignee"},
		{"review-requested", "--review-requested"},
	}

	ch := make(chan prQueryResult, len(queries))
	for _, q := range queries {
		q := q
		go func() {
			prs, err := c.searchPRs(ctx, q.flag)
			ch <- prQueryResult{q.label, prs, err}
		}()
	}

	var notes []string
	byRef := make(map[string]PR)
	for range queries {
		res := <-ch
		if res.err != nil {
			slog.Warn("PR query degraded.", "query", res.label, "error", res.err)
			notes = append(notes, fmt.Sprintf("%s: %v", res.label, res.err))
			continue
		}
		for _, p := range res.prs {
			byRef[p.Ref.String()] = p
		}
	}

	out := make([]PR, 0, len(byRef))
	for _, p := range byRef {
		out = append(out, p)
	}
	sortPRs(out)
	slog.Info("Successfully loaded open PRs.", "count", len(out), "degraded_queries", len(notes))
	return out, notes, nil
}

// Dash builds the sectioned dashboard: local active reviews (c.List, no gh),
// the review-requested-of-you queue, and your own open PRs. The two gh queries
// run concurrently; a degraded query (or an unreadable breadcrumb dir) becomes
// a note rather than failing the whole view.
func (c *Client) Dash(ctx context.Context) (Dashboard, []string, error) {
	var notes []string

	active, err := c.List()
	if err != nil {
		slog.Warn("Active-reviews section degraded.", "error", err)
		notes = append(notes, fmt.Sprintf("active-reviews: %v", err))
	}

	const sections = 2
	ch := make(chan prQueryResult, sections)
	go func() {
		prs, err := c.searchPRs(ctx, "--review-requested")
		ch <- prQueryResult{"awaiting-you", prs, err}
	}()
	go func() {
		prs, err := c.searchPRs(ctx, "--author")
		ch <- prQueryResult{"your-open", prs, err}
	}()

	dash := Dashboard{ActiveReviews: active}
	for i := 0; i < sections; i++ {
		res := <-ch
		if res.err != nil {
			slog.Warn("Dashboard section degraded.", "section", res.label, "error", res.err)
			notes = append(notes, fmt.Sprintf("%s: %v", res.label, res.err))
			continue
		}
		sortPRs(res.prs)
		switch res.label {
		case "awaiting-you":
			dash.AwaitingYou = res.prs
		case "your-open":
			dash.YourOpen = res.prs
		}
	}
	slog.Info("Successfully built dashboard.", "active_reviews", len(dash.ActiveReviews), "awaiting_you", len(dash.AwaitingYou), "your_open", len(dash.YourOpen), "degraded_sections", len(notes))
	return dash, notes, nil
}

// searchPRs runs one open-PR search scoped to @me by whoFlag (--author,
// --assignee, or --review-requested) and parses the rows. The flag and its
// literal "@me" value are the only variable argv, so nothing hostile reaches
// the shell-out.
func (c *Client) searchPRs(ctx context.Context, whoFlag string) ([]PR, error) {
	out, err := c.run.Run(ctx, "gh", "search", "prs",
		whoFlag, "@me",
		"--state", "open",
		"--json", prSearchFields)
	if err != nil {
		return nil, err
	}
	return parseSearchPRs(out)
}

// parseSearchPRs decodes `gh search prs --json` output into PRs. gh output is
// hostile input: each row's owner/repo (from repository.nameWithOwner) is
// re-validated against the anchored owner/repo charset AND routed through the
// canonical refFrom validator, so an unparseable, out-of-charset, or otherwise
// invalid row is skipped (logged), never fatal.
func parseSearchPRs(jsonOut string) ([]PR, error) {
	if strings.TrimSpace(jsonOut) == "" {
		return nil, nil
	}
	var raw []ghSearchPR
	if err := json.Unmarshal([]byte(jsonOut), &raw); err != nil {
		return nil, fmt.Errorf("parse gh search prs output: %w", err)
	}
	out := make([]PR, 0, len(raw))
	for _, r := range raw {
		owner, repo, ok := splitSlug(r.Repository.NameWithOwner)
		if !ok {
			slog.Warn("Skipping PR with unparseable repository.", "nameWithOwner", r.Repository.NameWithOwner)
			continue
		}
		if !reOwner.MatchString(owner) || !reOwner.MatchString(repo) {
			slog.Warn("Skipping PR with out-of-charset owner/repo.", "owner", owner, "repo", repo)
			continue
		}
		// Route through refFrom — the same validator the CLI ref path uses — so
		// the discovery path applies the identical ".."/leading-"-"/positive-
		// number guards. gh output flows into the same git/gh sinks; the two
		// paths must not drift (breadcrumbFilename relies on this invariant).
		ref, err := refFrom(owner, repo, strconv.Itoa(r.Number))
		if err != nil {
			slog.Warn("Skipping PR with invalid ref.", "nameWithOwner", r.Repository.NameWithOwner, "number", r.Number, "error", err)
			continue
		}
		out = append(out, PR{
			Ref:       ref,
			Title:     r.Title,
			Author:    r.Author.Login,
			State:     r.State,
			IsDraft:   r.IsDraft,
			UpdatedAt: r.UpdatedAt,
			URL:       r.URL,
		})
	}
	return out, nil
}

// sortPRs orders PRs deterministically by (slug, number) — the same
// tiebreak-for-stability discipline projects.Inventory applies to its rows.
func sortPRs(prs []PR) {
	sort.Slice(prs, func(i, j int) bool {
		si, sj := prs[i].Ref.Slug(), prs[j].Ref.Slug()
		if si != sj {
			return si < sj
		}
		return prs[i].Ref.Number < prs[j].Ref.Number
	})
}

// PrepResult is one PrepareMany outcome. Err captures a per-item failure in the
// degrade-to-note philosophy — one PR failing to prepare never sinks the batch.
type PrepResult struct {
	Ref     Ref
	Session Session
	Err     error
}

// PrepareMany prepares each ref concurrently and returns results aligned to the
// INPUT order — results[i] is refs[i]'s outcome. Each goroutine writes a
// distinct results[i], so there is no shared-write race and no post-sort is
// needed for determinism.
//
// Two bounds govern the fan-out: a semaphore caps total in-flight git
// checkouts, and a keyed mutex (on ref.Slug()) serializes any goroutines that
// share a clone so their checkouts never race the same repo. Per-item errors
// land in PrepResult.Err; the call itself never fails.
func (c *Client) PrepareMany(ctx context.Context, refs []Ref, opts PrepareOpts) []PrepResult {
	slog.Debug("Preparing to prepare multiple PRs concurrently.", "count", len(refs))
	results := make([]PrepResult, len(refs))
	km := newKeyedMutex()
	sem := make(chan struct{}, prepareConcurrency)
	var wg sync.WaitGroup

	for i, ref := range refs {
		i, ref := i, ref
		results[i].Ref = ref
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Take the per-repo mutex FIRST, then a semaphore slot: a goroutine
			// waiting on a busy repo must not hold a concurrency slot while it
			// blocks, or a same-repo burst would pin every slot and starve
			// distinct-repo prepares. Lock order is always slug-mutex → sem, so
			// no cycle — no deadlock.
			unlock := km.Lock(ref.Slug())
			defer unlock()

			sem <- struct{}{}
			defer func() { <-sem }()

			sess, err := c.Prepare(ctx, ref, opts)
			results[i].Session = sess
			results[i].Err = err
		}()
	}
	wg.Wait()

	// Count outcomes for summary logging.
	succeeded := 0
	for _, r := range results {
		if r.Err == nil {
			succeeded++
		}
	}
	slog.Info("Successfully prepared PR batch.", "count", len(refs), "succeeded", succeeded, "failed", len(refs)-succeeded)
	return results
}

// keyedMutex serializes goroutines by string key. An outer mutex guards the
// map of per-key mutexes; Lock hands back an unlock func for defer.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{locks: make(map[string]*sync.Mutex)}
}

// Lock blocks until the mutex for key is held, returning its unlock func.
func (k *keyedMutex) Lock(key string) (unlock func()) {
	k.mu.Lock()
	m, ok := k.locks[key]
	if !ok {
		m = &sync.Mutex{}
		k.locks[key] = m
	}
	k.mu.Unlock()

	m.Lock()
	return m.Unlock
}
