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
//   [x] A PRE-DISPATCH failure (gh/clone) returns the record to `queued` with
//       attempts=1 and lastError set
//   [x] A third pre-dispatch failure (attempts already 2) moves the record to
//       `needs-repair` with a reason naming the attempt count, and it is no
//       longer counted as `queued` on the next listing
//   [x] A failure AFTER the window exists leaves the record parked in
//       needs-repair with its window-naming reason and its workspace, and the
//       next pass launches nothing for that ref
//   [x] A failure with a live window on an unparked record parks it and tears
//       down nothing
//   [x] A pre-dispatch failure's workspace teardown happens OUTSIDE the
//       lifecycle lock
//   [x] A queued record marked local is refused at claim time: never claimed,
//       never cloned, reported as refused
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

// drainGhFailRunner fails `gh pr view` for every ref — a PRE-DISPATCH
// failure, the one shape a retry is safe for: no clone happened, no window
// exists, and Launch was never reached, so nothing parked the record.
func drainGhFailRunner() *exec.FakeRunner {
	inner := drainLaunchRunner(nil).RunFunc
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "view" {
			return "", errors.New("boom: gh could not read the PR")
		}
		return inner(name, args)
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
	// A gh failure: nothing was cloned and no window exists, so this is the
	// one failure shape the drainer may retry.
	run := drainGhFailRunner()
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
	run := drainGhFailRunner()
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

// drainWindowExistsRunner creates the review window for ref but hands back a
// windowId that is NOT generation-qualified, which is one of the two Launch
// branches where the agent is running and the record is parked in needs-repair
// with a reason naming the window. list-windows then reports that window, so
// both retry-stopping signals are present.
func drainWindowExistsRunner(t *testing.T, ref Ref) (*exec.FakeRunner, *int) {
	t.Helper()
	name, err := ReviewWindowName(ref)
	if err != nil {
		t.Fatalf("review window name: %v", err)
	}
	newWindows := 0
	created := false
	opened := false
	row := strings.Join([]string{"123", "456", "@9", "$1", "forgectl", "1", name, "1", "1"}, "\x1f")
	run := &exec.FakeRunner{RunFunc: func(cmd string, args []string) (string, error) {
		switch {
		case cmd == "gh" && len(args) >= 2 && args[0] == "pr" && args[1] == "view":
			return `{"headRefName":"feature","headRefOid":"abc123",` +
				`"headRepositoryOwner":{"login":"cameronsjo"},"headRepository":{"name":"forgectl"}}`, nil
		case cmd == "git" && len(args) > 0 && args[0] == "clone":
			return "", nil
		case cmd == "tmux" && len(args) > 0:
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
			case "new-session":
				created = true
				return "123\x1f456\x1f$1", nil
			case "list-windows":
				if opened {
					return row, nil
				}
				return "", nil
			case "new-window":
				newWindows++
				opened = true
				// A real window whose identity is not generation-qualified:
				// the server start time is not numeric, so validWindowID
				// refuses it after the window already exists.
				return "123\x1fnot-a-timestamp\x1f@9", nil
			}
		}
		return "", nil
	}}
	return run, &newWindows
}

func TestDrain_FailureAfterWindowExistsStaysParkedAndIsNotRetried(t *testing.T) {
	dir := t.TempDir()
	ref := testRef(1)
	run, newWindows := drainWindowExistsRunner(t, ref)
	c := drainClient(t, dir, run)
	seedQueued(t, c, ref, time.Now().UTC(), 0)

	report, err := c.Drain(context.Background(), config.Config{}, DrainOpts{})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(report.Items) != 1 || report.Items[0].Outcome != drainOutcomeNeedsRepair {
		t.Fatalf("items = %+v, want one needs-repair item (a live window must not be retried)", report.Items)
	}
	if report.Failed != 1 {
		t.Errorf("Failed = %d, want 1", report.Failed)
	}

	bc := readRecordByRef(t, dir, ref)
	if bc.Phase != PhaseNeedsRepair {
		t.Fatalf("phase = %q, want needs-repair", bc.Phase)
	}
	// Launch's own reason — naming the window — is the only pointer
	// `pr repair --adopt-window` has left, and the drainer must not erase it
	// or overwrite it with a "drain: N attempts" reason.
	windowName, nerr := ReviewWindowName(ref)
	if nerr != nil {
		t.Fatalf("review window name: %v", nerr)
	}
	if !strings.Contains(bc.RepairReason, windowName) {
		t.Errorf("repairReason = %q, want Launch's window-naming reason preserved", bc.RepairReason)
	}
	if strings.HasPrefix(bc.RepairReason, "drain:") {
		t.Errorf("repairReason = %q, want the drainer to leave Launch's reason alone", bc.RepairReason)
	}
	if bc.Workspace == "" {
		t.Fatal("workspace was cleared from the record; the agent's clean room must stay")
	}
	if _, serr := os.Stat(bc.Workspace); serr != nil {
		t.Errorf("workspace %s was removed under a live agent: %v", bc.Workspace, serr)
	}
	if bc.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 recorded on the parked record", bc.Attempts)
	}

	// The next pass claims nothing and opens no second window for the ref.
	report2, err := c.Drain(context.Background(), config.Config{}, DrainOpts{})
	if err != nil {
		t.Fatalf("second Drain: %v", err)
	}
	if report2.Queued != 0 || len(report2.Items) != 0 {
		t.Fatalf("second pass = %+v, want nothing queued and nothing claimed", report2)
	}
	if *newWindows != 1 {
		t.Errorf("tmux new-window called %d times, want exactly 1 — a parked ref must never launch again", *newWindows)
	}
}

func TestSettleDrainFailure_LiveWindowParksAndTearsDownNothing(t *testing.T) {
	dir := t.TempDir()
	ref := testRef(4)
	run, _ := drainWindowExistsRunner(t, ref)
	// The window is reported from the start: this is the record Launch failed
	// to park (its own park is best-effort), still sitting in `launching`.
	name, err := ReviewWindowName(ref)
	if err != nil {
		t.Fatalf("review window name: %v", err)
	}
	row := strings.Join([]string{"123", "456", "@9", "$1", "forgectl", "1", name, "1", "1"}, "\x1f")
	inner := run.RunFunc
	run.RunFunc = func(cmd string, args []string) (string, error) {
		if cmd == "tmux" && len(args) > 0 && args[0] == "list-windows" {
			return row, nil
		}
		return inner(cmd, args)
	}
	c := drainClient(t, dir, run)
	ws := fakeWorkspace(t)
	path := seedPhaseRecord(t, c, ref, PhaseLaunching, ws)

	teardowns := 0
	orig := sandboxTeardown
	sandboxTeardown = func(context.Context, exec.Runner, string) error { teardowns++; return nil }
	t.Cleanup(func() { sandboxTeardown = orig })

	outcome, toPhase := c.settleDrainFailure(context.Background(), ref, path, 0, 3, errors.New("boom: dispatch reported an error"))
	if outcome != drainOutcomeNeedsRepair || toPhase != string(PhaseNeedsRepair) {
		t.Fatalf("outcome = %q/%q, want needs-repair", outcome, toPhase)
	}
	if teardowns != 0 {
		t.Errorf("sandboxTeardown called %d times, want 0 — the window may be live", teardowns)
	}
	bc := readRecord(t, path)
	if bc.Phase != PhaseNeedsRepair {
		t.Fatalf("phase = %q, want needs-repair", bc.Phase)
	}
	if bc.Workspace != ws {
		t.Errorf("workspace = %q, want it untouched (%q)", bc.Workspace, ws)
	}
	if !strings.Contains(bc.RepairReason, "adopt-window") {
		t.Errorf("repairReason = %q, want it to name the way out", bc.RepairReason)
	}
}

func TestSettleDrainFailure_PreDispatchRequeuesAndRemovesWorkspaceOutsideTheLock(t *testing.T) {
	dir := t.TempDir()
	ref := testRef(5)
	run := drainLaunchRunner(nil) // list-windows reports no window
	c := drainClient(t, dir, run)
	ws := fakeWorkspace(t)
	path := seedPhaseRecord(t, c, ref, PhasePreparing, "")
	// Give the record a workspace the way a completed Prepare would.
	if terr := c.transition(context.Background(), path, PhasePreparing, PhasePrepared, func(rec *Breadcrumb) error {
		rec.Workspace = ws
		return nil
	}); terr != nil {
		t.Fatalf("seed a prepared record with a workspace: %v", terr)
	}

	var mu sync.Mutex
	var log []string
	note := func(s string) { mu.Lock(); log = append(log, s); mu.Unlock() }
	c.onLock = func(verb, event string) { note(event + ":" + verb) }
	var torn []string
	orig := sandboxTeardown
	sandboxTeardown = func(_ context.Context, _ exec.Runner, workspace string) error {
		note("teardown")
		torn = append(torn, workspace)
		return nil
	}
	t.Cleanup(func() { sandboxTeardown = orig })

	outcome, toPhase := c.settleDrainFailure(context.Background(), ref, path, 0, 3, errors.New("boom: clone failed"))
	if outcome != drainOutcomeRetryQueued || toPhase != string(PhaseQueued) {
		t.Fatalf("outcome = %q/%q, want retry-queued/queued", outcome, toPhase)
	}
	bc := readRecord(t, path)
	if bc.Phase != PhaseQueued || bc.Attempts != 1 {
		t.Fatalf("record = phase %q attempts %d, want queued/1", bc.Phase, bc.Attempts)
	}
	if bc.Workspace != "" || bc.WindowID != "" {
		t.Errorf("requeued record still names workspace %q / window %q", bc.Workspace, bc.WindowID)
	}
	if len(torn) != 1 || torn[0] != ws {
		t.Fatalf("tore down %v, want exactly [%s]", torn, ws)
	}

	// THE REMOVAL MUST NOT HAPPEN UNDER THE LOCK: a recursive delete of a full
	// clone would stall every other lifecycle-lock user for its duration.
	mu.Lock()
	defer mu.Unlock()
	held := 0
	for _, e := range log {
		switch {
		case strings.HasPrefix(e, "acquire:"):
			held++
		case strings.HasPrefix(e, "release:"):
			held--
		case e == "teardown" && held > 0:
			t.Fatalf("workspace teardown ran inside a lifecycle-lock hold; log = %v", log)
		}
	}
}

func TestDrain_QueuedLocalRecordIsRefusedAtClaim(t *testing.T) {
	dir := t.TempDir()
	run := drainLaunchRunner(nil)
	launches := 0
	inner := run.RunFunc
	run.RunFunc = func(name string, args []string) (string, error) {
		if name == "git" && len(args) > 0 && args[0] == "clone" {
			launches++
		}
		if name == "tmux" && len(args) > 0 && args[0] == "new-window" {
			launches++
		}
		return inner(name, args)
	}
	c := drainClient(t, dir, run)

	ref := newLocalRef("abc1234def")
	bc := Breadcrumb{
		Ref: ref.String(), Agent: "claude", CreatedAt: time.Now().UTC(), Local: true,
		Provenance: ReviewProvenanceOperatorAuthored.persisted(),
		Version:    breadcrumbVersion, Phase: PhaseQueued, Revision: 1,
	}
	path, err := writeBreadcrumb(c.SessionsDir(), ref, bc)
	if err != nil {
		t.Fatalf("seed queued local record: %v", err)
	}

	report, err := c.Drain(context.Background(), config.Config{}, DrainOpts{})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(report.Items) != 1 || report.Items[0].Outcome != drainOutcomeRefused {
		t.Fatalf("items = %+v, want one refused item", report.Items)
	}
	if !strings.Contains(report.Items[0].Error, "local reviews cannot be drained") {
		t.Errorf("item error = %q, want the local refusal reason", report.Items[0].Error)
	}
	if report.Failed != 1 {
		t.Errorf("Failed = %d, want 1", report.Failed)
	}
	if launches != 0 {
		t.Errorf("clone/new-window ran %d times, want 0 — a local record is refused before it is claimed", launches)
	}
	after := readRecord(t, path)
	if after.Phase != PhaseQueued {
		t.Errorf("phase = %q, want still queued (never claimed to preparing)", after.Phase)
	}
	if after.Revision != 1 {
		t.Errorf("revision = %d, want 1 — the record must not be written at all", after.Revision)
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
