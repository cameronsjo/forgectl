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
	// gitTimeout bounds the branch checks of one collect.
	gitTimeout = 5 * time.Second
	// branchCheckGap is the least time between two branch checks of one
	// worktree, so event-driven collects do not run git for every worker
	// every second.
	branchCheckGap = 5 * time.Second
	// hcParallel caps how many hc or git calls one collect runs at once.
	hcParallel = 8
	// detailRetry is how long a failed `hc get` for a terminal's start
	// time and model waits before it is tried again. The row shows without
	// them meanwhile.
	detailRetry = time.Minute
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

	mu         sync.Mutex
	cache      map[string]prEntry
	details    map[string]detail    // hc get by address: start time, model
	detailFail map[string]time.Time // when a get for detailKey last failed
	cosSeen    map[string]bool      // cos-* names seen live in this watch
}

// detail is one address's `hc get`, with what identifies the terminal it
// was read from: its name and the highest turn `hc ls` has shown for it.
type detail struct {
	t    Terminal
	name string
	turn int64
}

// sameTerminal reports whether the `hc ls` record t is still the terminal d
// was read from. hc reuses addresses after a core restart; `hc ls` carries
// no start time, but a new terminal at the address has another name, or the
// same name with its turn count started again. A relaunch under the same
// name that reaches the old turn count before the next collect is not told
// apart; its age and model stay the old ones until the address is reused
// again or `hc ls` stops listing it.
func (d detail) sameTerminal(t Terminal) bool {
	return d.name == t.Name && t.Turn >= d.turn
}

// detailKey keys a failed get by address and name, so a failure on one
// terminal does not hold back the get for a new one at the same address.
func detailKey(t Terminal) string { return t.Address + "\x00" + t.Name }

type prEntry struct {
	at      time.Time // the last attempt: what the TTL counts from
	checked time.Time // the last branch check git answered (branchesMoved)
	readAt  time.Time // the last successful read of pr
	branch  string
	pr      *PR
	issue   string
	note    string
}

// Collect reads every worker once. force refreshes PR reads older than
// ForceMinAge, still at most ghPerCollect of them.
//
// hc is read with one `hc ls` per collect for every worker's address and
// status. `hc ls` carries no start time or argv, so `hc get` runs once per
// new terminal (an address is cached while `hc ls` shows the same terminal
// there) and for a name `hc ls` does not list (finished, or gone). If
// `hc ls` fails, every worker falls back to `hc get`. The gets run at once,
// under one hcTimeout.
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
	// Every hc get this collect needs runs at once under one deadline, so a
	// few hung terminals cost one hcTimeout, not one each.
	rows := make([]rowBuild, len(entries))
	listed := make([]bool, len(entries))
	var jobs []func(context.Context)
	var readRows []int // the row each readHC job fills, in job order
	var gets []Terminal
	for i, e := range entries {
		rows[i] = rowBuild{Worker: Worker{Name: e.Name, Worktree: e.Worktree, BriefDir: e.BriefDir, Source: sources[e.Name]}}
		if t, ok := live[e.Name]; ok && listErr == nil {
			listed[i] = true
			if c.needDetail(t, now) && !slices.ContainsFunc(gets, func(g Terminal) bool { return g.Address == t.Address }) {
				gets = append(gets, t)
			}
			continue
		}
		readRows = append(readRows, i)
		jobs = append(jobs, func(ctx context.Context) { c.readHC(ctx, &rows[i], now) })
	}
	got := make([]Terminal, len(gets))
	gotErr := make([]error, len(gets))
	for j, t := range gets {
		jobs = append(jobs, func(ctx context.Context) { got[j], gotErr[j] = c.HC.Get(ctx, t.Address) })
	}
	ran := runAll(ctx, hcTimeout, jobs)
	for k, i := range readRows {
		if !ran[k] {
			rows[i].HCError = hcNotReadText
		}
	}
	for j := range gets {
		if !ran[len(readRows)+j] {
			gotErr[j] = errNotRun
		}
	}
	if listErr == nil {
		c.storeDetails(live, gets, got, gotErr, now)
	}

	workers := make([]Worker, 0, len(entries))
	unreachable := 0
	for i := range rows {
		w := &rows[i]
		if listed[i] {
			c.fromList(w, live[w.Name], now)
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

// hcNotReadText is the HCError for a worker whose hc get was still queued
// when the collect's hc deadline passed.
const hcNotReadText = "hc was not read: other hc calls used up this refresh's time"

// errNotRun marks a detail get runAll skipped. It is neither cached nor
// backed off: it says nothing about the terminal.
var errNotRun = errors.New("hc get was not run")

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

// needDetail reports whether the terminal t `hc ls` listed needs an `hc get`
// for its start time and model: none is cached for it, and no get for it
// failed within detailRetry.
func (c *Collector) needDetail(t Terminal, now time.Time) bool {
	if d, ok := c.details[t.Address]; ok && d.sameTerminal(t) {
		return false
	}
	failed, ok := c.detailFail[detailKey(t)]
	return !ok || now.Sub(failed) >= detailRetry
}

// storeDetails updates the detail cache after a collect whose `hc ls`
// answered: an entry whose address `hc ls` no longer lists, or now lists
// for another terminal, is dropped, so the cache holds only live addresses;
// then the gets this collect ran are stored, or their failures recorded.
func (c *Collector) storeDetails(live map[string]Terminal, gets, got []Terminal, gotErr []error, now time.Time) {
	if c.details == nil {
		c.details = map[string]detail{}
		c.detailFail = map[string]time.Time{}
	}
	byAddr := make(map[string]Terminal, len(live))
	keys := make(map[string]bool, len(live))
	for _, t := range live {
		byAddr[t.Address] = t
		keys[detailKey(t)] = true
	}
	for addr, d := range c.details {
		if t, ok := byAddr[addr]; !ok || !d.sameTerminal(t) {
			delete(c.details, addr)
		} else {
			d.turn = t.Turn
			c.details[addr] = d
		}
	}
	for k := range c.detailFail {
		if !keys[k] {
			delete(c.detailFail, k)
		}
	}
	for j, t := range gets {
		if errors.Is(gotErr[j], errNotRun) {
			continue
		}
		if gotErr[j] != nil {
			c.detailFail[detailKey(t)] = now
			continue
		}
		delete(c.detailFail, detailKey(t))
		c.details[t.Address] = detail{t: got[j], name: t.Name, turn: t.Turn}
	}
}

// fromList fills w from its `hc ls` record and the cached `hc get` for its
// address, when there is one for this terminal.
func (c *Collector) fromList(w *rowBuild, t Terminal, now time.Time) {
	var d Terminal
	if cached, ok := c.details[t.Address]; ok && cached.sameTerminal(t) {
		d = cached.t
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

// readHC fills w from `hc get` by name. It runs beside the collect's other
// hc calls and touches only w.
func (c *Collector) readHC(ctx context.Context, w *rowBuild, now time.Time) {
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
	moved := c.branchesMoved(ctx, workers, now)
	var due []stale
	for i, w := range workers {
		if w.Worktree == "" || c.GH == nil || c.Git == nil {
			continue
		}
		e, ok := c.cache[w.Worktree]
		age := now.Sub(e.at)
		switch {
		case !ok, moved[w.Worktree]:
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

// branchesMoved checks the branch of each worktree with a cached PR read,
// at most once per branchCheckGap, and reports the worktrees now on another
// branch than the read was for: a worker that switched branch is re-read
// within branchCheckGap, not after the TTL. A branch git cannot read (or a
// check the deadline skipped) keeps the cached read.
func (c *Collector) branchesMoved(ctx context.Context, workers []Worker, now time.Time) map[string]bool {
	if c.Git == nil || c.GH == nil {
		return nil
	}
	var trees []string
	for _, w := range workers {
		e, ok := c.cache[w.Worktree]
		last := e.at
		if e.checked.After(last) {
			last = e.checked
		}
		if ok && w.Worktree != "" && now.Sub(last) >= branchCheckGap && !slices.Contains(trees, w.Worktree) {
			trees = append(trees, w.Worktree)
		}
	}
	branches := make([]string, len(trees))
	errs := make([]error, len(trees))
	jobs := make([]func(context.Context), len(trees))
	for i, wt := range trees {
		jobs[i] = func(ctx context.Context) { branches[i], errs[i] = c.Git.Branch(ctx, wt) }
	}
	ran := runAll(ctx, gitTimeout, jobs)
	moved := map[string]bool{}
	for i, wt := range trees {
		if !ran[i] || errs[i] != nil {
			continue
		}
		e := c.cache[wt]
		e.checked = now
		c.cache[wt] = e
		if branches[i] != e.branch {
			moved[wt] = true
		}
	}
	return moved
}

// runAll runs jobs at once, at most hcParallel at a time, under one shared
// timeout, and returns when every started job has. A job still queued when
// the timeout passes is skipped rather than run on a dead context, and
// ran[i] reports whether jobs[i] ran, so the caller does not read a skipped
// job as a failure of the thing it would have read.
func runAll(ctx context.Context, timeout time.Duration, jobs []func(context.Context)) (ran []bool) {
	ran = make([]bool, len(jobs))
	if len(jobs) == 0 {
		return ran
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	sem := make(chan struct{}, hcParallel)
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return // both cases were ready and select chose the slot
			}
			ran[i] = true
			job(ctx)
		})
	}
	wg.Wait()
	return ran
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
