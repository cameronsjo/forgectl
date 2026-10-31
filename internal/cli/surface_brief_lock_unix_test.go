//go:build unix

package cli

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/herdr/ready"
	"github.com/cameronsjo/forgectl/internal/resume"
	"github.com/cameronsjo/forgectl/internal/surface/hcadapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

// TestConcurrentHcBriefsSpaceTheirEscapes: two `surface brief` runs to one
// worker at once must not both send an Escape inside the spacing. The brief
// lock makes the second wait, and it then reads the first's record, so its
// Escape comes at least briefEscapeSpacing later (real ledger, real clock).
func TestConcurrentHcBriefsSpaceTheirEscapes(t *testing.T) {
	led := testWorkerLedger(t)
	opened := launchedLedgerRow(t, led, time.Unix(1e9, 0).UTC())
	var mu sync.Mutex
	var escapes []time.Time
	steps := func() hcBriefSteps {
		return hcBriefSteps{
			status: func(context.Context) (hcadapter.Worker, error) {
				return hcadapter.Worker{Live: true, Status: "idle", Turn: 1}, nil
			},
			screen:      func(context.Context) (string, error) { return hcFixture("claude-ready-acceptedits"), nil },
			promptShown: func(text string) ready.Verdict { return hcTable.EvaluateText("claude", text) },
			escape: func(context.Context, hcadapter.Worker) error {
				mu.Lock()
				escapes = append(escapes, time.Now())
				mu.Unlock()
				return nil
			},
			send: func(context.Context, string, hcadapter.Worker) (hcadapter.SendResult, error) {
				return hcadapter.SendResult{Status: "working", Turn: 2}, nil
			},
			record: func(b worker.Brief) (int, error) {
				return 1, led.Update("w", func(r *worker.Row) { r.Brief = &b })
			},
			now:      time.Now,
			sleep:    resume.SleepContext,
			start:    time.Second,
			interval: 10 * time.Millisecond,
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	results := make([]briefResult, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = hcBriefLocked(ctx, led.LockBrief, opened, steps(), "go", "m1")
		}()
	}
	wg.Wait()
	for i, r := range results {
		if r.Outcome != briefSent {
			t.Fatalf("brief %d: %q at %s: %s", i, r.Outcome, r.Step, r.Reason)
		}
	}
	sort.Slice(escapes, func(i, j int) bool { return escapes[i].Before(escapes[j]) })
	if len(escapes) != 2 {
		t.Fatalf("%d Escapes, want 2", len(escapes))
	}
	if gap := escapes[1].Sub(escapes[0]); gap < briefEscapeSpacing {
		t.Fatalf("the two Escapes were %v apart, want at least %v", gap, briefEscapeSpacing)
	}
}

// TestHcBriefLockedRefusesAChangedRow: a row that is not the one the worker
// was opened with (closed and relaunched under the name) sends nothing.
func TestHcBriefLockedRefusesAChangedRow(t *testing.T) {
	led := testWorkerLedger(t)
	launchedLedgerRow(t, led, time.Unix(1e9, 0).UTC().Add(time.Hour))
	opened := worker.Row{Name: "w", Branch: "b", Stage: worker.StageLaunched, StartedAt: time.Unix(1e9, 0).UTC()}
	f := hcBriefFake{statuses: []hcadapter.Worker{{Live: true, Status: "idle", Turn: 1}}}
	res := hcBriefLocked(context.Background(), led.LockBrief, opened, f.steps(), "go", "m1")
	if res.Outcome != briefRefused || len(f.events) != 0 {
		t.Fatalf("outcome %q events %v, want refused with nothing sent", res.Outcome, f.events)
	}
}

// launchedLedgerRow writes a launched row "w" started at at, and returns it
// as the ledger holds it, the way openWorker reads it.
func launchedLedgerRow(t *testing.T, led *worker.Ledger, at time.Time) worker.Row {
	t.Helper()
	if err := led.Begin(worker.Row{Name: "w", Branch: "b", StartedAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := led.Update("w", func(r *worker.Row) { r.Stage = worker.StageLaunched }); err != nil {
		t.Fatal(err)
	}
	return onlyRow(t, led)
}

// TestCanceledBriefWaitKeepsTheHoldersLock: a brief that gives up waiting
// for the lock (its context ends mid-wait) must not release the lock the
// running brief holds. A third brief still waits until the first has sent
// and returned.
func TestCanceledBriefWaitKeepsTheHoldersLock(t *testing.T) {
	led := testWorkerLedger(t)
	opened := launchedLedgerRow(t, led, time.Unix(1e9, 0).UTC())
	sending := make(chan struct{})
	finish := make(chan struct{})
	var finishOnce sync.Once
	letFirstFinish := func() { finishOnce.Do(func() { close(finish) }) }
	t.Cleanup(letFirstFinish) // a failed assertion never leaves the first brief holding the lock
	var mu sync.Mutex
	var sends []string
	steps := func(who string, block bool) hcBriefSteps {
		return hcBriefSteps{
			status: func(context.Context) (hcadapter.Worker, error) {
				return hcadapter.Worker{Live: true, Status: "idle", Turn: 1}, nil
			},
			screen:      func(context.Context) (string, error) { return hcFixture("claude-ready-acceptedits"), nil },
			promptShown: func(text string) ready.Verdict { return hcTable.EvaluateText("claude", text) },
			escape:      func(context.Context, hcadapter.Worker) error { return nil },
			send: func(context.Context, string, hcadapter.Worker) (hcadapter.SendResult, error) {
				mu.Lock()
				sends = append(sends, who)
				mu.Unlock()
				if block {
					close(sending)
					<-finish
				}
				return hcadapter.SendResult{Status: "working", Turn: 2}, nil
			},
			record:   func(worker.Brief) (int, error) { return 1, nil },
			now:      time.Now,
			sleep:    resume.SleepContext,
			start:    time.Second,
			interval: 10 * time.Millisecond,
		}
	}

	first := make(chan briefResult, 1)
	go func() {
		first <- hcBriefLocked(context.Background(), led.LockBrief, opened, steps("first", true), "go", "m1")
	}()
	select {
	case <-sending:
	case <-time.After(5 * time.Second):
		t.Fatal("the first brief never reached its send")
	}

	// The second brief waits, then its context is canceled mid-wait.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	if res := hcBriefLocked(ctx, led.LockBrief, opened, steps("second", false), "go", "m2"); res.Outcome != briefRefused {
		t.Fatalf("the canceled brief: %q at %s: %s, want refused", res.Outcome, res.Step, res.Reason)
	}

	// The first brief is still sending, so the lock must still be held.
	short, cancelShort := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancelShort()
	if release, _, err := led.LockBrief(short, "w"); !errors.Is(err, worker.ErrBriefBusy) {
		if release != nil {
			release()
		}
		letFirstFinish()
		t.Fatalf("LockBrief after the canceled wait = %v, want ErrBriefBusy while the first brief sends", err)
	}

	letFirstFinish()
	if res := <-first; res.Outcome != briefSent {
		t.Fatalf("the first brief: %q at %s: %s", res.Outcome, res.Step, res.Reason)
	}
	release, _, err := led.LockBrief(context.Background(), "w")
	if err != nil {
		t.Fatalf("LockBrief after the first brief returned: %v", err)
	}
	release()
	mu.Lock()
	defer mu.Unlock()
	if len(sends) != 1 || sends[0] != "first" {
		t.Fatalf("sends = %v, want only the first brief's", sends)
	}
}
