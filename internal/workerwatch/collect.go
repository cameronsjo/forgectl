package workerwatch

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultGHTTL is how long a worker's PR read is reused.
	DefaultGHTTL = 90 * time.Second
	// MinGHTTL is the floor on --gh-ttl.
	MinGHTTL = 30 * time.Second
	// ForceMinAge is how old a cached PR read must be before a forced
	// refresh replaces it.
	ForceMinAge = 15 * time.Second
	// ghPerCollect caps how many PR reads one collect starts, so a dozen
	// workers with cold caches spread their gh calls over several collects.
	ghPerCollect = 2
	// hcTimeout bounds one hc call.
	hcTimeout = 10 * time.Second
	// ghTimeout bounds one PR read (git, then gh).
	ghTimeout = 30 * time.Second
)

// Worker is one row of the watch.
type Worker struct {
	Name     string `json:"name"`
	Worktree string `json:"worktree,omitempty"`
	BriefDir string `json:"brief_dir,omitempty"`
	// Source is "file" for a workers-file line, "hc" for a --cos find.
	Source string `json:"source"`

	Address string `json:"address,omitempty"`
	// State is hc's live or finished.
	State string `json:"state,omitempty"`
	// Status is the agent's self-reported status (hc OSC 7501).
	Status string `json:"status,omitempty"`
	// AgeSeconds is how long ago its terminal started.
	AgeSeconds int64  `json:"age_seconds,omitempty"`
	Model      string `json:"model,omitempty"`
	// HCError says why hc could not be read for this worker.
	HCError string `json:"hc_error,omitempty"`

	Reports Reports `json:"reports"`

	Branch string `json:"branch,omitempty"`
	PR     *PR    `json:"pr,omitempty"`
	// PRIssue says why PR is missing or stale, as one of the PRIssue*
	// values; "" when PR is a fresh read. With PRIssueGH, PR is the last good
	// read, kept beside the failure.
	PRIssue string `json:"pr_issue,omitempty"`
	// PRNote is PRIssue in words, for a person.
	PRNote string `json:"pr_note,omitempty"`
	// PRAt is when PR was last read successfully; zero before that.
	PRAt time.Time `json:"pr_read_at,omitzero"`
}

// PRIssue values.
const (
	PRIssueNoBranch  = "no-branch"  // detached, or git could not read the branch
	PRIssueNoOrigin  = "no-origin"  // no readable origin remote
	PRIssueNotGitHub = "not-github" // origin is not on github.com
	PRIssueNoPR      = "no-pr"      // no PR from this branch yet
	PRIssueGH        = "gh-error"   // the GitHub read failed
)

// Gone reports that hc has no terminal by the worker's name. Any other hc
// failure leaves the worker unread, not gone: hc stumbled, and the worker may
// still be running.
func (w Worker) Gone() bool { return w.HCError == hcGoneText }

// NeedsYou reports whether a person has to act: the agent says it is
// blocked (a permission dialog or a question is on its screen) and its
// terminal is still live. A finished terminal keeps its last report.
func (w Worker) NeedsYou() bool {
	return w.Status == StatusBlocked && w.State == "live" && w.HCError == ""
}

// Snapshot is one collection.
type Snapshot struct {
	Taken   time.Time `json:"taken"`
	Workers []Worker  `json:"workers"`
	// Notes are collection-wide problems (a bad workers line, hc ls failing).
	Notes []string `json:"notes,omitempty"`
	// HCUnavailable says why hc could not be read at all (not on PATH, or
	// every read failed); "" when hc answered. The off-terminal snapshot
	// exits 1 when it is set.
	HCUnavailable string `json:"hc_unavailable,omitempty"`
}

// NeedingYou lists the workers that need a person, in row order.
func (s Snapshot) NeedingYou() []Worker {
	var out []Worker
	for _, w := range s.Workers {
		if w.NeedsYou() {
			out = append(out, w)
		}
	}
	return out
}

// PRSource is the GitHub read the collector caches.
type PRSource interface {
	PRFor(ctx context.Context, repo, branch string) (PR, error)
}

// Collector gathers snapshots. It is safe for one Collect at a time per
// Collector; a second concurrent call waits.
type Collector struct {
	HC      HC
	Git     Git
	GH      PRSource
	Reports func(dir string) Reports
	Now     func() time.Time
	// Entries are the workers-file lines.
	Entries []Entry
	// Notes are carried into every snapshot (workers-file problems).
	Notes []string
	// Cos adds every live hc terminal named cos-*.
	Cos bool
	// GHTTL is how long a PR read is reused; below MinGHTTL means MinGHTTL.
	GHTTL time.Duration
	// Reviewers is the [sweep] reviewers allowlist for review markers;
	// empty counts every reviewer.
	Reviewers []string

	mu      sync.Mutex
	cache   map[string]prEntry
	details map[string]Terminal // hc get by address: start time, model
	cosSeen map[string]bool     // cos-* names seen live in this watch
}

type prEntry struct {
	at     time.Time // the last attempt: what the TTL counts from
	readAt time.Time // the last successful read of pr
	branch string
	pr     *PR
	issue  string
	note   string
}

// Collect reads every worker once. force refreshes PR reads older than
// ForceMinAge, still at most ghPerCollect of them.
//
// hc is read with one `hc ls` per collect for every worker's address and
// status. `hc ls` carries no start time or argv, so `hc get` runs once per
// new address (those never change) and for a name `hc ls` does not list
// (finished, or gone). If `hc ls` fails, every worker falls back to
// `hc get`.
func (c *Collector) Collect(ctx context.Context, force bool) Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().UTC()
	snap := Snapshot{Taken: now, Notes: slices.Clone(c.Notes)}
	entries := slices.Clone(c.Entries)
	for i := range entries {
		entries[i].Name = strings.TrimSpace(entries[i].Name)
	}
	live, listErr := c.list(ctx)
	if c.Cos {
		if listErr != nil {
			snap.Notes = append(snap.Notes, "--cos: "+listErr.Error())
		}
		entries = append(entries, c.discover(live, entries)...)
	}
	sources := make(map[string]string, len(entries))
	for i, e := range entries {
		if i < len(c.Entries) {
			sources[e.Name] = "file"
		} else {
			sources[e.Name] = "hc"
		}
	}
	workers := make([]Worker, 0, len(entries))
	unreachable := 0
	for _, e := range entries {
		w := rowBuild{Worker: Worker{Name: e.Name, Worktree: e.Worktree, BriefDir: e.BriefDir, Source: sources[e.Name]}}
		if t, ok := live[e.Name]; ok && listErr == nil {
			c.fromList(ctx, &w, t, now)
		} else {
			c.readHC(ctx, &w, now)
		}
		if w.HCError != "" && w.HCError != hcGoneText {
			unreachable++
		}
		if w.Worktree == "" && w.Source == "hc" {
			w.Worktree = w.cwdFallback
		}
		if c.Reports != nil && w.BriefDir != "" {
			w.Reports = c.Reports(w.BriefDir)
		}
		if w.Reports.Files == nil {
			w.Reports.Files = []ReportFile{} // --json always carries an array
		}
		workers = append(workers, w.Worker)
	}
	switch {
	case errors.Is(listErr, ErrNoHC):
		snap.HCUnavailable = ErrNoHC.Error()
	case len(entries) > 0 && unreachable == len(entries):
		snap.HCUnavailable = "hc could not be read for any worker"
	case len(entries) == 0 && listErr != nil:
		snap.HCUnavailable = "hc ls failed"
	}
	c.readPRs(ctx, workers, now, force)
	snap.Workers = workers
	return snap
}

// rowBuild carries a Worker and the cwd hc reported, used as the worktree of
// a --cos find.
type rowBuild struct {
	Worker
	cwdFallback string
}

// hcGoneText is the HCError for a name hc does not know.
const hcGoneText = "no hc terminal by this name"

// list is one `hc ls`, keyed by name.
func (c *Collector) list(ctx context.Context) (map[string]Terminal, error) {
	ctx, cancel := context.WithTimeout(ctx, hcTimeout)
	defer cancel()
	ts, err := c.HC.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Terminal, len(ts))
	for _, t := range ts {
		if t.Name != "" {
			if _, dup := out[t.Name]; !dup {
				out[t.Name] = t
			}
		}
	}
	return out, nil
}

// discover lists every live cos-* terminal not already in have, plus every
// cos-* name seen live earlier in this watch, so a worker that exits or
// crashes stays on screen as exited (or gone) rather than vanishing.
func (c *Collector) discover(live map[string]Terminal, have []Entry) []Entry {
	if c.cosSeen == nil {
		c.cosSeen = map[string]bool{}
	}
	for name, t := range live {
		if t.State == "live" && strings.HasPrefix(name, CosPrefix) {
			c.cosSeen[name] = true
		}
	}
	known := make(map[string]bool, len(have))
	for _, e := range have {
		known[e.Name] = true
	}
	var out []Entry
	for name := range c.cosSeen {
		if !known[name] {
			out = append(out, Entry{Name: name})
		}
	}
	slices.SortFunc(out, func(a, b Entry) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// fromList fills w from its `hc ls` record, reading the start time and model
// with `hc get` only the first time an address is seen.
func (c *Collector) fromList(ctx context.Context, w *rowBuild, t Terminal, now time.Time) {
	if c.details == nil {
		c.details = map[string]Terminal{}
	}
	d, ok := c.details[t.Address]
	if !ok {
		gctx, cancel := context.WithTimeout(ctx, hcTimeout)
		g, err := c.HC.Get(gctx, t.Address)
		cancel()
		if err == nil {
			d = g
			c.details[t.Address] = g
		}
	}
	w.Address, w.State, w.Status = t.Address, t.State, t.Status
	w.Model = d.Model
	if age := d.Age(now); age > 0 {
		w.AgeSeconds = int64(age / time.Second)
	}
	cwd := t.Cwd
	if cwd == "" {
		cwd = d.Cwd
	}
	if strings.HasPrefix(cwd, "/") {
		w.cwdFallback = cwd
	}
}

func (c *Collector) readHC(ctx context.Context, w *rowBuild, now time.Time) {
	ctx, cancel := context.WithTimeout(ctx, hcTimeout)
	defer cancel()
	t, err := c.HC.Get(ctx, w.Name)
	if err != nil {
		switch {
		case errors.Is(err, ErrGone):
			w.HCError = hcGoneText
		case errors.Is(err, ErrNoHC):
			w.HCError = ErrNoHC.Error()
		default:
			w.HCError = "hc could not read this terminal"
		}
		return
	}
	w.Address, w.State, w.Status, w.Model = t.Address, t.State, t.Status, t.Model
	if age := t.Age(now); age > 0 {
		w.AgeSeconds = int64(age / time.Second)
	}
	if strings.HasPrefix(t.Cwd, "/") {
		w.cwdFallback = t.Cwd
	}
}

// readPRs fills each worker's PR from the cache, refreshing at most
// ghPerCollect stale entries, the stalest first.
func (c *Collector) readPRs(ctx context.Context, workers []Worker, now time.Time, force bool) {
	if c.cache == nil {
		c.cache = map[string]prEntry{}
	}
	ttl := max(c.GHTTL, MinGHTTL)
	type stale struct {
		i   int
		age time.Duration
	}
	var due []stale
	for i, w := range workers {
		if w.Worktree == "" || c.GH == nil || c.Git == nil {
			continue
		}
		e, ok := c.cache[w.Worktree]
		age := now.Sub(e.at)
		switch {
		case !ok:
			due = append(due, stale{i, time.Duration(1<<62 - 1)})
		case age >= ttl, force && age >= ForceMinAge:
			due = append(due, stale{i, age})
		}
	}
	slices.SortStableFunc(due, func(a, b stale) int { return cmp.Compare(b.age, a.age) })
	for _, d := range due[:min(len(due), ghPerCollect)] {
		w := workers[d.i]
		c.cache[w.Worktree] = c.readPR(ctx, w.Worktree, c.cache[w.Worktree], now)
	}
	for i := range workers {
		e, ok := c.cache[workers[i].Worktree]
		if !ok {
			continue
		}
		w := &workers[i]
		w.Branch, w.PR, w.PRIssue, w.PRNote, w.PRAt = e.branch, e.pr, e.issue, e.note, e.readAt
	}
}

// readPR resolves the worktree's repo and branch, then its PR. A failed gh
// read keeps the previous PR and its read time beside the issue, so the row
// shows stale data as stale; the attempt still restarts the TTL, so an
// outage is not retried on every collect.
func (c *Collector) readPR(ctx context.Context, worktree string, prev prEntry, now time.Time) prEntry {
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	e := prEntry{at: now}
	branch, err := c.Git.Branch(ctx, worktree)
	if err != nil {
		e.issue, e.note = PRIssueNoBranch, "git could not read the worktree's branch"
		return e
	}
	if branch == "" {
		e.issue, e.note = PRIssueNoBranch, "the worktree has no branch checked out"
		return e
	}
	e.branch = branch
	repo, err := c.Git.Repo(ctx, worktree)
	switch {
	case errors.Is(err, ErrNotGitHub):
		e.issue, e.note = PRIssueNotGitHub, "origin is not a github.com repository"
		return e
	case err != nil:
		e.issue, e.note = PRIssueNoOrigin, "the worktree has no readable origin remote"
		return e
	}
	p, err := c.GH.PRFor(ctx, repo, branch)
	switch {
	case errors.Is(err, ErrNoPR):
		e.issue, e.note, e.readAt = PRIssueNoPR, "no PR yet", now
	case err != nil:
		e.issue, e.note = PRIssueGH, "the PR could not be read: "+err.Error()
		if prev.branch == branch {
			e.pr, e.readAt = prev.pr, prev.readAt
		}
	default:
		p.Review = SummarizeMarkers(p.Markers, c.Reviewers)
		e.pr, e.readAt = &p, now
	}
	return e
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
