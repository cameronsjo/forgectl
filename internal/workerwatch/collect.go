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
	// PRNote says why there is no PR row: no branch, no PR yet, or a failed
	// read. A failed read keeps the last good PR beside the note.
	PRNote string `json:"pr_note,omitempty"`
	// PRAt is when PR was read; zero before the first read.
	PRAt time.Time `json:"pr_read_at,omitzero"`
}

// NeedsYou reports whether a person has to act: the agent says it is
// blocked (a permission dialog or a question is on its screen).
func (w Worker) NeedsYou() bool { return w.Status == StatusBlocked }

// Snapshot is one collection.
type Snapshot struct {
	Taken   time.Time `json:"taken"`
	Workers []Worker  `json:"workers"`
	// Notes are collection-wide problems (a bad workers line, hc ls failing).
	Notes []string `json:"notes,omitempty"`
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

	mu    sync.Mutex
	cache map[string]prEntry
}

type prEntry struct {
	at     time.Time
	branch string
	pr     *PR
	note   string
}

// Collect reads every worker once. force refreshes PR reads older than
// ForceMinAge, still at most ghPerCollect of them.
func (c *Collector) Collect(ctx context.Context, force bool) Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().UTC()
	snap := Snapshot{Taken: now, Notes: slices.Clone(c.Notes)}
	entries := slices.Clone(c.Entries)
	for i := range entries {
		entries[i].Name = strings.TrimSpace(entries[i].Name)
	}
	if c.Cos {
		found, err := c.discover(ctx, entries)
		if err != nil {
			snap.Notes = append(snap.Notes, "--cos: "+err.Error())
		}
		entries = append(entries, found...)
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
	for _, e := range entries {
		w := rowBuild{Worker: Worker{Name: e.Name, Worktree: e.Worktree, BriefDir: e.BriefDir, Source: sources[e.Name]}}
		c.readHC(ctx, &w, now)
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

func (c *Collector) discover(ctx context.Context, have []Entry) ([]Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, hcTimeout)
	defer cancel()
	ts, err := c.HC.List(ctx)
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(have))
	for _, e := range have {
		known[e.Name] = true
	}
	var out []Entry
	for _, t := range ts {
		if t.State != "live" || !strings.HasPrefix(t.Name, CosPrefix) || known[t.Name] {
			continue
		}
		known[t.Name] = true
		out = append(out, Entry{Name: t.Name})
	}
	slices.SortFunc(out, func(a, b Entry) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

func (c *Collector) readHC(ctx context.Context, w *rowBuild, now time.Time) {
	ctx, cancel := context.WithTimeout(ctx, hcTimeout)
	defer cancel()
	t, err := c.HC.Get(ctx, w.Name)
	if err != nil {
		w.HCError = "hc could not read this terminal"
		if errors.Is(err, ErrGone) {
			w.HCError = "no hc terminal by this name"
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
		workers[i].Branch, workers[i].PR, workers[i].PRNote, workers[i].PRAt = e.branch, e.pr, e.note, e.at
	}
}

// readPR resolves the worktree's repo and branch, then its PR. A failed gh
// read keeps the previous PR beside the note.
func (c *Collector) readPR(ctx context.Context, worktree string, prev prEntry, now time.Time) prEntry {
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	e := prEntry{at: now}
	branch, err := c.Git.Branch(ctx, worktree)
	if err != nil {
		e.note = "git could not read the worktree's branch"
		return e
	}
	if branch == "" {
		e.note = "the worktree has no branch checked out"
		return e
	}
	e.branch = branch
	repo, err := c.Git.Repo(ctx, worktree)
	switch {
	case errors.Is(err, ErrNotGitHub):
		e.note = "origin is not a github.com repository"
		return e
	case err != nil:
		e.note = "the worktree has no readable origin remote"
		return e
	}
	p, err := c.GH.PRFor(ctx, repo, branch)
	switch {
	case errors.Is(err, ErrNoPR):
		e.note = "no PR yet"
	case err != nil:
		e.note = "the PR could not be read: " + err.Error()
		if prev.branch == branch {
			e.pr = prev.pr
		}
	default:
		e.pr = &p
	}
	return e
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
