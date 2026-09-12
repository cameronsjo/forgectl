package pr

// Test plan for drain.go
//
// Drain (Classification: composite verb, crash-safety bridge)
//   [x] Claims the oldest queued records first, under ONE lock hold, and
//       launches them through the ordinary Prepare -> Launch path
//   [x] A `preparing`/`prepared`/`launching` record occupies a slot exactly
//       as it does for reserve/Admit, so drain claims fewer queued records
//       when one is already in flight
//   [x] An unreadable record refuses the WHOLE pass before claiming anything
//   [x] A launch failure returns the record to `queued` with attempts=1 and
//       lastError set
//   [x] A third failure (attempts already 2) moves the record to
//       `needs-repair` with a reason naming the attempt count, and it is no
//       longer counted as `queued` on the next listing
//   [x] Two Clients racing one queued record launch it exactly once
//   [x] `--dry-run` claims nothing, launches nothing, and reports would-launch
//   [x] A mixed pass (one success, one failure) reports both items
//   [x] An empty queue reports zero queued/launched/failed and no items

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
)

// drainLaunchRunner fakes gh (pr view always resolves a valid head), git
// (clone always succeeds), and tmux — new-window fails on the call numbers
// named in failOn (1-indexed, in invocation order) and succeeds with a fresh
// generation-qualified identity otherwise.
func drainLaunchRunner(failOn map[int]error) *exec.FakeRunner {
	created := false
	call := 0
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		switch {
		case name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "view":
			return `{"headRefName":"feature","headRefOid":"abc123",` +
				`"headRepositoryOwner":{"login":"cameronsjo"},"headRepository":{"name":"forgectl"}}`, nil
		case name == "git" && len(args) > 0 && args[0] == "clone":
			return "", nil
		case name == "tmux" && len(args) > 0:
			switch args[0] {
			case "-V":
				return "tmux 3.7b", nil
			case "display-message":
				return "123\x1f456\x1f@0", nil
			case "list-sessions":
				if created {
					return "123\x1f456\x1f$1\x1fforgectl\x1f1\x1f0\x1f0\x1f/tmp", nil
				}
				return "", nil
			case "list-windows":
				return "", nil
			case "new-session":
				created = true
				return "123\x1f456\x1f$1", nil
			case "new-window":
				call++
				if err, ok := failOn[call]; ok {
					return "", err
				}
				return fmt.Sprintf("123\x1f456\x1f@%d", call), nil
			}
		}
		return "", nil
	}}
}

// drainClient builds a Client wired for drain tests: real sessions dir,
// fakeClaude on PATH via env, no interactive TTY concerns (Drain never shows
// one).
func drainClient(t *testing.T, dir string, run *exec.FakeRunner) *Client {
	t.Helper()
	fakeClaude(t)
	return New(run, WithSessionsDir(dir), WithFindingsDir(t.TempDir()),
		WithTmuxSession("forgectl"), WithLockWait(2*time.Second))
}

// seedQueued writes a `queued` record directly (bypassing Client.Queue) so
// tests can pin CreatedAt for deterministic FIFO ordering and pre-seed
// Attempts for the exhaustion test.
func seedQueued(t *testing.T, c *Client, ref Ref, createdAt time.Time, attempts int) string {
	t.Helper()
	bc := Breadcrumb{
		Ref: ref.String(), Agent: "claude", CreatedAt: createdAt,
		Version: breadcrumbVersion, Phase: PhaseQueued, Revision: 1,
		Attempts: attempts,
	}
	path, err := writeBreadcrumb(c.SessionsDir(), ref, bc)
	if err != nil {
		t.Fatalf("seed queued record: %v", err)
	}
	return path
}

func TestDrain_ClaimsOldestQueuedUnderOneLockHoldAndLaunches(t *testing.T) {
	dir := t.TempDir()
	run := drainLaunchRunner(nil)
	inner := run.RunFunc
	var mu sync.Mutex
	var log []string
	note := func(s string) { mu.Lock(); log = append(log, s); mu.Unlock() }
	run.RunFunc = func(name string, args []string) (string, error) {
		note("run:" + name)
		return inner(name, args)
	}
	c := drainClient(t, dir, run)
	c.onLock = func(verb, event string) { note(event + ":" + verb) }

	base := time.Now().UTC().Add(-time.Hour)
	seedQueued(t, c, testRef(1), base, 0)
	seedQueued(t, c, testRef(2), base.Add(time.Minute), 0)
	seedQueued(t, c, testRef(3), base.Add(2*time.Minute), 0) // newest — must stay queued

	report, err := c.Drain(context.Background(), config.Config{Pr: config.PrConfig{MaxConcurrent: 2}}, DrainOpts{})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if report.Free != 2 || report.Queued != 3 {
		t.Fatalf("report = %+v, want Free=2 Queued=3", report)
	}
	if report.Launched != 2 || report.Failed != 0 {
		t.Fatalf("report = %+v, want Launched=2 Failed=0", report)
	}
	gotRefs := map[string]bool{}
	for _, item := range report.Items {
		gotRefs[item.Ref] = true
		if item.Outcome != drainOutcomeLaunched || item.ToPhase != string(PhaseActive) {
			t.Errorf("item %+v, want outcome=launched toPhase=active", item)
		}
	}
	if !gotRefs[testRef(1).String()] || !gotRefs[testRef(2).String()] {
		t.Fatalf("launched refs = %v, want the two oldest (1 and 2)", gotRefs)
	}

	// The newest stays queued.
	third := readRecordByRef(t, dir, testRef(3))
	if third.Phase != PhaseQueued {
		t.Errorf("newest record phase = %q, want still queued", third.Phase)
	}
	launchedOldest := readRecordByRef(t, dir, testRef(1))
	if launchedOldest.Phase != PhaseActive {
		t.Errorf("oldest record phase = %q, want active", launchedOldest.Phase)
	}

	// The claim (queued -> preparing transitions) happened under ONE lock
	// hold. The hold legitimately makes one tmux list-windows call to count
	// occupancy (the same pattern reserve/Admit already use) — what must
	// NEVER fall inside it is the clone (`git`) or the gh round-trip, since
	// those are the long operations phase records exist to keep off the
	// lock.
	mu.Lock()
	defer mu.Unlock()
	start, end := -1, -1
	holds := 0
	for i, e := range log {
		if e == "acquire:drain" {
			holds++
			if start < 0 {
				start = i
			}
		}
		if e == "release:drain" && end < 0 && start >= 0 {
			end = i
		}
	}
	if holds != 1 {
		t.Fatalf("drain lock held %d times, want exactly 1", holds)
	}
	for i := start; i <= end; i++ {
		if log[i] == "run:git" || log[i] == "run:gh" {
			t.Errorf("run call %q (clone/gh round-trip) happened inside the drain lock hold", log[i])
		}
	}
}

// readRecordByRef finds the one record on disk for ref and decodes it.
func readRecordByRef(t *testing.T, dir string, ref Ref) Breadcrumb {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		bc := readRecord(t, path)
		if bc.Ref == ref.String() {
			return bc
		}
	}
	t.Fatalf("no record found for ref %s", ref.String())
	return Breadcrumb{}
}

func TestDrain_InFlightRecordsConsumeSlots(t *testing.T) {
	dir := t.TempDir()
	c := drainClient(t, dir, drainLaunchRunner(nil))

	// Two slots already occupied: one preparing, one launching. Default cap
	// is 4, so only two slots remain free for three queued refs. Preparing
	// allows no workspace (the clone has not landed); launching requires one
	// (it has already cloned).
	seedPhaseRecord(t, c, testRef(10), PhasePreparing, "")
	seedPhaseRecord(t, c, testRef(11), PhaseLaunching, fakeWorkspace(t))

	base := time.Now().UTC().Add(-time.Hour)
	seedQueued(t, c, testRef(1), base, 0)
	seedQueued(t, c, testRef(2), base.Add(time.Minute), 0)
	seedQueued(t, c, testRef(3), base.Add(2*time.Minute), 0)

	report, err := c.Drain(context.Background(), config.Config{}, DrainOpts{})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if report.Free != 2 {
		t.Fatalf("Free = %d, want 2 (cap 4 minus 2 in-flight records)", report.Free)
	}
	if report.Launching != 2 {
		t.Errorf("Launching = %d, want 2", report.Launching)
	}
	if len(report.Items) != 2 {
		t.Fatalf("claimed %d items, want 2 (one queued record left behind)", len(report.Items))
	}
}

func TestDrain_UnreadableRecordRefusesWholePass(t *testing.T) {
	dir := t.TempDir()
	c := drainClient(t, dir, drainLaunchRunner(nil))
	seedQueued(t, c, testRef(1), time.Now().UTC(), 0)
	bad := []byte(`{"workspace":"/tmp/x","ref":"o/r#9","createdAt":"2026-09-11T00:00:00Z","futureKey":true}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, "o-r-9-1.json"), bad, 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := c.Drain(context.Background(), config.Config{}, DrainOpts{})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if report.Refusal == "" {
		t.Fatal("expected a pass refusal naming the unreadable record")
	}
	if !strings.Contains(report.Refusal, "could not be read") {
		t.Errorf("refusal = %q, want it to name the unreadable record", report.Refusal)
	}
	if len(report.Items) != 0 {
		t.Errorf("items = %v, want none — an unreadable cap must launch nothing", report.Items)
	}
	// Nothing was claimed: the queued record is untouched.
	bc := readRecordByRef(t, dir, testRef(1))
	if bc.Phase != PhaseQueued {
		t.Errorf("queued record phase = %q, want still queued", bc.Phase)
	}
}

func TestDrain_LaunchFailureReturnsToQueuedWithAttempts(t *testing.T) {
	dir := t.TempDir()
	run := drainLaunchRunner(map[int]error{1: errors.New("boom: agent refused")})
	c := drainClient(t, dir, run)
	seedQueued(t, c, testRef(1), time.Now().UTC(), 0)

	report, err := c.Drain(context.Background(), config.Config{}, DrainOpts{})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if report.Launched != 0 || report.Failed != 1 {
		t.Fatalf("report = %+v, want Launched=0 Failed=1", report)
	}
	if len(report.Items) != 1 || report.Items[0].Outcome != drainOutcomeRetryQueued {
		t.Fatalf("items = %+v, want one retry-queued item", report.Items)
	}
	bc := readRecordByRef(t, dir, testRef(1))
	if bc.Phase != PhaseQueued {
		t.Fatalf("phase = %q, want queued (a failure retries)", bc.Phase)
	}
	if bc.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", bc.Attempts)
	}
	if bc.LastError == "" {
		t.Error("lastError is empty, want it set")
	}
	if bc.LastAttempt.IsZero() {
		t.Error("lastAttemptAt is zero, want it set")
	}
}

func TestDrain_ThirdFailureMovesToNeedsRepair(t *testing.T) {
	dir := t.TempDir()
	run := drainLaunchRunner(map[int]error{1: errors.New("boom: agent refused again")})
	c := drainClient(t, dir, run)
	// Two prior failed attempts already recorded.
	seedQueued(t, c, testRef(1), time.Now().UTC(), 2)

	report, err := c.Drain(context.Background(), config.Config{}, DrainOpts{})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(report.Items) != 1 || report.Items[0].Outcome != drainOutcomeNeedsRepair {
		t.Fatalf("items = %+v, want one needs-repair item", report.Items)
	}
	bc := readRecordByRef(t, dir, testRef(1))
	if bc.Phase != PhaseNeedsRepair {
		t.Fatalf("phase = %q, want needs-repair at 3 attempts", bc.Phase)
	}
	if bc.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", bc.Attempts)
	}
	if !strings.Contains(bc.RepairReason, "drain: 3 attempts") {
		t.Errorf("repairReason = %q, want it to name the attempt count", bc.RepairReason)
	}

	// The next pass does not pick it up: it is no longer `queued`.
	report2, err := c.Drain(context.Background(), config.Config{}, DrainOpts{})
	if err != nil {
		t.Fatalf("second Drain: %v", err)
	}
	if report2.Queued != 0 || len(report2.Items) != 0 {
		t.Fatalf("second pass = %+v, want nothing queued and nothing claimed", report2)
	}
}

func TestDrain_TwoClientsAgainstOneQueuedRecordLaunchOnce(t *testing.T) {
	dir := t.TempDir()
	var launches int32Counter
	run := drainLaunchRunner(nil)
	inner := run.RunFunc
	run.RunFunc = func(name string, args []string) (string, error) {
		if name == "tmux" && len(args) > 0 && args[0] == "new-window" {
			launches.add(1)
		}
		return inner(name, args)
	}
	mk := func() *Client { return drainClient(t, dir, run) }
	first, second := mk(), mk()
	seedQueued(t, first, testRef(1), time.Now().UTC(), 0)

	var wg sync.WaitGroup
	reports := make([]DrainReport, 2)
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		reports[0], errs[0] = first.Drain(context.Background(), config.Config{}, DrainOpts{})
	}()
	go func() {
		defer wg.Done()
		reports[1], errs[1] = second.Drain(context.Background(), config.Config{}, DrainOpts{})
	}()
	wg.Wait()

	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("Drain errors: %v, %v", errs[0], errs[1])
	}
	if got := launches.get(); got != 1 {
		t.Fatalf("tmux new-window called %d times, want exactly 1", got)
	}
	launchedTotal := reports[0].Launched + reports[1].Launched
	if launchedTotal != 1 {
		t.Fatalf("total launched across both clients = %d, want 1", launchedTotal)
	}
}

// int32Counter is a tiny race-safe counter, local to this test file so it
// carries no dependency beyond sync.
type int32Counter struct {
	mu sync.Mutex
	n  int
}

func (c *int32Counter) add(d int) { c.mu.Lock(); c.n += d; c.mu.Unlock() }
func (c *int32Counter) get() int  { c.mu.Lock(); defer c.mu.Unlock(); return c.n }

func TestDrain_DryRunCreatesNothingAndReportsWouldLaunch(t *testing.T) {
	dir := t.TempDir()
	c := drainClient(t, dir, drainLaunchRunner(nil))
	seedQueued(t, c, testRef(1), time.Now().UTC(), 0)

	report, err := c.Drain(context.Background(), config.Config{}, DrainOpts{DryRun: true})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(report.Items) != 1 || report.Items[0].Outcome != drainOutcomeWouldLaunch {
		t.Fatalf("items = %+v, want one would-launch item", report.Items)
	}
	bc := readRecordByRef(t, dir, testRef(1))
	if bc.Phase != PhaseQueued {
		t.Fatalf("phase = %q, want unchanged queued — dry-run must create nothing", bc.Phase)
	}
	entries, _ := os.ReadDir(dir)
	jsonFiles := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			jsonFiles++
		}
	}
	if jsonFiles != 1 {
		t.Errorf("session dir has %d records, want 1 — dry-run must create no new record", jsonFiles)
	}
}

func TestDrain_MixedPassOneSuccessOneFailure(t *testing.T) {
	dir := t.TempDir()
	// First claimed (oldest) succeeds; second fails.
	run := drainLaunchRunner(map[int]error{2: errors.New("boom: second agent refused")})
	c := drainClient(t, dir, run)
	base := time.Now().UTC().Add(-time.Hour)
	seedQueued(t, c, testRef(1), base, 0)
	seedQueued(t, c, testRef(2), base.Add(time.Minute), 0)

	report, err := c.Drain(context.Background(), config.Config{}, DrainOpts{})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if report.Launched != 1 || report.Failed != 1 {
		t.Fatalf("report = %+v, want Launched=1 Failed=1", report)
	}
	if len(report.Items) != 2 {
		t.Fatalf("items = %+v, want 2", report.Items)
	}
}

func TestDrain_EmptyQueueReportsZero(t *testing.T) {
	dir := t.TempDir()
	c := drainClient(t, dir, drainLaunchRunner(nil))

	report, err := c.Drain(context.Background(), config.Config{}, DrainOpts{})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if report.Queued != 0 || report.Launched != 0 || report.Failed != 0 || len(report.Items) != 0 {
		t.Fatalf("report = %+v, want an all-zero empty report", report)
	}
	if report.Free != DefaultMaxConcurrentReviews {
		t.Errorf("Free = %d, want the full default cap", report.Free)
	}
}
