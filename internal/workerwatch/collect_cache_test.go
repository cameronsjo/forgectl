package workerwatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestCollectorDropsADetailWhenTheAddressIsReused: after an hc core restart
// a new terminal can get an old address. Its row must show its own model,
// not the old terminal's, whether it has another name or the same name with
// its turns started again; and an address `hc ls` stops listing leaves the
// cache.
func TestCollectorDropsADetailWhenTheAddressIsReused(t *testing.T) {
	hc := &countingHC{fakeHC: fakeHC{
		terms: map[string]Terminal{"t1": {Address: "t1", Name: "cos-a", State: "live", Model: "opus"}},
		list:  []Terminal{{Address: "t1", Name: "cos-a", State: "live", Turn: 5}},
	}}
	c := &Collector{HC: hc, Entries: []Entry{{Name: "cos-a"}, {Name: "cos-b"}}}
	if s := c.Collect(context.Background(), false); s.Workers[0].Model != "opus" {
		t.Fatalf("first terminal = %+v", s.Workers[0])
	}

	// Another name at t1.
	hc.terms["t1"] = Terminal{Address: "t1", Name: "cos-b", State: "live", Model: "sonnet"}
	hc.list = []Terminal{{Address: "t1", Name: "cos-b", State: "live", Turn: 0}}
	if s := c.Collect(context.Background(), false); s.Workers[1].Model != "sonnet" {
		t.Fatalf("a new name at a reused address kept the old detail: %+v", s.Workers[1])
	}

	// The same name at t1 again, its turns started over.
	hc.list = []Terminal{{Address: "t1", Name: "cos-b", State: "live", Turn: 3}}
	c.Collect(context.Background(), false)
	hc.terms["t1"] = Terminal{Address: "t1", Name: "cos-b", State: "live", Model: "haiku"}
	hc.list = []Terminal{{Address: "t1", Name: "cos-b", State: "live", Turn: 1}}
	if s := c.Collect(context.Background(), false); s.Workers[1].Model != "haiku" {
		t.Fatalf("a relaunch under the same name at the same address kept the old detail: %+v", s.Workers[1])
	}

	hc.list = nil
	c.Collect(context.Background(), false)
	if len(c.details) != 0 {
		t.Errorf("details for addresses hc ls no longer lists were kept: %v", c.details)
	}
}

// slowHC answers every Get after delay, and fails it.
type slowHC struct {
	delay    time.Duration
	list     []Terminal
	mu       sync.Mutex
	gets     int
	inFlight int
	maxIn    int
}

func (s *slowHC) Get(ctx context.Context, _ string) (Terminal, error) {
	s.mu.Lock()
	s.gets++
	s.inFlight++
	s.maxIn = max(s.maxIn, s.inFlight)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}()
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
	}
	return Terminal{}, errors.New("hc get: timed out")
}

func (s *slowHC) List(context.Context) ([]Terminal, error)     { return s.list, nil }
func (s *slowHC) Read(context.Context, string) (string, error) { return "", nil }

// TestCollectorRunsSlowGetsAtOnceAndBacksOff: three slow gets run at the
// same time, not one after another, and a get that failed is not retried on the next
// collect inside detailRetry.
func TestCollectorRunsSlowGetsAtOnceAndBacksOff(t *testing.T) {
	const delay = 300 * time.Millisecond
	hc := &slowHC{delay: delay, list: []Terminal{
		{Address: "t1", Name: "cos-a", State: "live"},
		{Address: "t2", Name: "cos-b", State: "live"},
		{Address: "t3", Name: "cos-c", State: "live"},
	}}
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	c := &Collector{HC: hc, Now: func() time.Time { return now }, Entries: []Entry{{Name: "cos-a"}, {Name: "cos-b"}, {Name: "cos-c"}}}
	s := c.Collect(context.Background(), false)
	if hc.maxIn != 3 {
		t.Fatalf("at most %d gets were in flight at once, want all 3", hc.maxIn)
	}
	if s.Workers[0].Address != "t1" || s.Workers[0].HCError != "" {
		t.Fatalf("a failed detail get should still show the listed row: %+v", s.Workers[0])
	}
	if hc.gets != 3 {
		t.Fatalf("gets = %d, want 3", hc.gets)
	}
	c.Collect(context.Background(), false)
	if hc.gets != 3 {
		t.Fatalf("gets after a second collect = %d, want 3: a failed get was retried inside detailRetry", hc.gets)
	}
	now = now.Add(detailRetry)
	c.Collect(context.Background(), false)
	if hc.gets != 6 {
		t.Fatalf("gets after detailRetry = %d, want 6", hc.gets)
	}
}

// switchGit reports a branch the test can change.
type switchGit struct {
	mu     sync.Mutex
	branch string
}

func (g *switchGit) Branch(context.Context, string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.branch, nil
}

func (g *switchGit) Repo(context.Context, string) (string, error) { return "o/r", nil }

// TestCollectorRereadsAPRAfterABranchSwitch: a worker that switches branch
// shows the new branch's PR within branchCheckGap, not after the TTL.
func TestCollectorRereadsAPRAfterABranchSwitch(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	git := &switchGit{branch: "branch-old"}
	gh := &fakeGH{}
	c := &Collector{
		HC:      &fakeHC{terms: map[string]Terminal{"cos-a": {Address: "t1", Name: "cos-a", State: "live"}}},
		Git:     git,
		GH:      gh,
		Now:     func() time.Time { return now },
		Entries: []Entry{{Name: "cos-a", Worktree: "/wt/a"}},
	}
	if s := c.Collect(context.Background(), false); s.Workers[0].Branch != "branch-old" {
		t.Fatalf("first read = %+v", s.Workers[0])
	}
	now = now.Add(branchCheckGap)
	c.Collect(context.Background(), false)
	if len(gh.calls) != 1 {
		t.Fatalf("PR reads inside the TTL on one branch = %v, want 1", gh.calls)
	}
	git.mu.Lock()
	git.branch = "branch-new"
	git.mu.Unlock()
	c.Collect(context.Background(), false)
	if len(gh.calls) != 1 {
		t.Fatalf("a branch check ran inside branchCheckGap: PR reads %v", gh.calls)
	}
	now = now.Add(branchCheckGap)
	s := c.Collect(context.Background(), false)
	if s.Workers[0].Branch != "branch-new" || len(gh.calls) != 2 || gh.calls[1] != "branch-new" {
		t.Fatalf("after a branch switch: row branch %q, PR reads %v; want the new branch read at once", s.Workers[0].Branch, gh.calls)
	}
}

// TestRunAllSkipsJobsQueuedPastTheDeadline: with more jobs than hcParallel
// and every running job hung until the deadline, the queued jobs are
// skipped, not run on a dead context, and ran says which ones ran.
func TestRunAllSkipsJobsQueuedPastTheDeadline(t *testing.T) {
	var mu sync.Mutex
	started := 0
	jobs := make([]func(context.Context), hcParallel+2)
	for i := range jobs {
		jobs[i] = func(ctx context.Context) {
			mu.Lock()
			started++
			mu.Unlock()
			<-ctx.Done()
		}
	}
	ran := runAll(context.Background(), 100*time.Millisecond, jobs)
	n := 0
	for _, r := range ran {
		if r {
			n++
		}
	}
	if n != hcParallel || started != hcParallel {
		t.Fatalf("ran %d, started %d; want %d of %d (the queued ones skipped)", n, started, hcParallel, len(jobs))
	}
}

// TestCollectorDoesNotBackOffASkippedGet: a detail get the deadline skipped
// is not recorded as failed, so the next collect tries it.
func TestCollectorDoesNotBackOffASkippedGet(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	c := &Collector{Now: func() time.Time { return now }}
	live := map[string]Terminal{"cos-a": {Address: "t1", Name: "cos-a"}}
	gets := []Terminal{live["cos-a"]}
	c.storeDetails(live, gets, make([]Terminal, 1), []error{errNotRun}, now)
	if !c.needDetail(live["cos-a"], now) {
		t.Fatal("a skipped get was backed off")
	}
	c.storeDetails(live, gets, make([]Terminal, 1), []error{errors.New("hung")}, now)
	if c.needDetail(live["cos-a"], now) {
		t.Fatal("a failed get was not backed off")
	}
}
