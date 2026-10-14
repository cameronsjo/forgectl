//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/launch"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

type fakeTerminalReader struct {
	id  string
	err error
}

func (f fakeTerminalReader) TerminalID(context.Context, backend.Ref) (string, error) {
	return f.id, f.err
}

// A worker launch's --json names what it created: the herdr ids, the
// terminal_id when it can be read, and the row's worktree, branch, session and
// marker.
func TestWorkerLaunchResult(t *testing.T) {
	ref := testHerdrRef(t)
	attempt := workerAttempt{
		launched: workerLaunched{ref: ref, worktree: testRepoTop + "/.claude/worktrees/w1"},
		row: &worker.Row{Name: "w1", Harness: "claude", Branch: "feat/w1", SessionID: "sess-1",
			Transcript: "/t/sess-1.jsonl", Brief: &worker.Brief{Marker: "mk-1"}},
	}
	got, err := workerLaunchResult(t.Context(), fakeTerminalReader{id: "term-3"}, attempt, "w1", testRepoTop)
	if err != nil {
		t.Fatalf("workerLaunchResult: %v", err)
	}
	want := launchResult{
		Surface: "herdr", Name: "w1", Target: testRepoTop, Harness: "claude", Ref: ref.String(),
		WorkspaceID: "w9", TabID: "w9:t1", PaneID: "w9:p1", TerminalID: "term-3",
		Worker: &launchedWorkerResult{Repo: testRepoTop, Worktree: testRepoTop + "/.claude/worktrees/w1",
			Branch: "feat/w1", SessionID: "sess-1", Transcript: "/t/sess-1.jsonl", Marker: "mk-1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result = %+v\nwant     %+v", got, want)
	}

	var b bytes.Buffer
	if err := writeJSON(&b, got); err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(b.Bytes(), &keys); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if k := mapKeys(keys); !reflect.DeepEqual(k, []string{"harness", "name", "pane_id", "ref", "surface", "tab_id", "target", "terminal_id", "worker", "workspace_id"}) {
		t.Errorf("keys = %v", k)
	}
}

// A row that could not be read back still names the worker and its ids, and
// the result says the rest is missing rather than reporting success.
func TestWorkerLaunchResultWithoutRow(t *testing.T) {
	ref := testHerdrRef(t)
	for name, attempt := range map[string]workerAttempt{
		"row unreadable": {launched: workerLaunched{ref: ref, worktree: "/wt"}, rowErr: errors.New("ledger locked")},
		"row missing":    {launched: workerLaunched{ref: ref, worktree: "/wt"}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := workerLaunchResult(t.Context(), struct{}{}, attempt, "w1", testRepoTop)
			if err == nil {
				t.Fatal("a launch whose row could not be read reported no error")
			}
			if got.Name != "w1" || got.PaneID != "w9:p1" || got.Worker == nil || got.Worker.Worktree != "/wt" {
				t.Errorf("result = %+v", got)
			}
		})
	}
}

// A launch whose bookkeeping failed after the start says where the running
// worker is; one that never started has no reference to name.
func TestRunningWorkerError(t *testing.T) {
	ref := testHerdrRef(t)
	if !launchStarted(workerAttempt{launched: workerLaunched{ref: ref}}) {
		t.Error("a launch with a reference reads as not started")
	}
	if launchStarted(workerAttempt{}) {
		t.Error("a launch with no reference reads as started")
	}
	base := errors.New("ledger row could not be completed")
	err := runningWorkerError(base, ref)
	if !errors.Is(err, base) || !strings.Contains(err.Error(), "workspace w9, pane w9:p1") {
		t.Errorf("err = %v", err)
	}
}

// A terminal_id that cannot be read is left out; the launch still reports.
func TestLaunchResultWithoutTerminal(t *testing.T) {
	ref := testHerdrRef(t)
	got := newLaunchResult(t.Context(), fakeTerminalReader{err: errors.New("herdr went away")}, ref, "herdr", "n", "/p", "codex")
	if got.TerminalID != "" || got.PaneID != "w9:p1" {
		t.Fatalf("result = %+v", got)
	}
	// An adapter that cannot read one is not asked.
	got = newLaunchResult(t.Context(), struct{}{}, ref, "herdr", "n", "/p", "codex")
	if got.TerminalID != "" || got.WorkspaceID != "w9" {
		t.Fatalf("result = %+v", got)
	}
}

type fakeNameShower struct {
	backend.Adapter
	got []string
	err error
}

func (f *fakeNameShower) ShowWorkerName(_ context.Context, _ backend.Ref, name string) error {
	f.got = append(f.got, name)
	return f.err
}

// The worker name goes to herdr after the launch is recorded, and a failure
// is a note, never a failed launch.
func TestWorkerLaunchShowsTheName(t *testing.T) {
	led := testWorkerLedger(t)
	shower := &fakeNameShower{err: errors.New("herdr: unknown command")}
	var warn bytes.Buffer
	steps := goodSteps(t)
	steps.showName = func(ctx context.Context, ref backend.Ref) {
		if row := onlyRow(t, led); row.Stage != worker.StageLaunched {
			t.Errorf("the name was shown before the launch was recorded (stage %q)", row.Stage)
		}
		showWorkerName(ctx, shower, ref, "w1", &warn)
	}
	if _, err := runWorkerSteps(t.Context(), led, "w1", "feat/w1", steps); err != nil {
		t.Fatalf("runWorkerSteps: %v", err)
	}
	if !reflect.DeepEqual(shower.got, []string{"w1"}) {
		t.Errorf("names shown = %v, want [w1]", shower.got)
	}
	if !strings.Contains(warn.String(), "note: herdr's sidebar will not show the worker name") {
		t.Errorf("warn = %q, want the cosmetic note", warn.String())
	}
}

// A failed launch shows no name.
func TestFailedWorkerLaunchShowsNoName(t *testing.T) {
	led := testWorkerLedger(t)
	called := false
	steps := goodSteps(t)
	steps.launch = func(context.Context, launch.Invocation) (backend.Ref, error) {
		return backend.Ref{}, errors.New("herdr went away")
	}
	steps.showName = func(context.Context, backend.Ref) { called = true }
	if _, err := runWorkerSteps(t.Context(), led, "w1", "feat/w1", steps); err == nil {
		t.Fatal("the failed launch was not reported")
	}
	if called {
		t.Error("a failed launch showed a name")
	}
}

// list reads present workers' terminal_ids after the probes, each under its
// own deadline, and stops at the first failed read.
func TestFillTerminalIDs(t *testing.T) {
	encoded, err := testHerdrRef(t).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	rows := []worker.Row{{Name: "a", Ref: encoded}, {Name: "b", Ref: encoded}, {Name: "c", Ref: encoded}, {Name: "d", Ref: encoded}}
	out := []listRow{{State: workerPresent}, {State: workerGone}, {State: workerPresent}, {State: workerPresent}}
	reader := &scriptedTerminals{replies: []error{nil, errors.New("timed out"), nil}}
	fillTerminalIDs(t.Context(), reader, rows, out)
	if out[0].TerminalID != "term-1" || out[1].TerminalID != "" || out[2].TerminalID != "" || out[3].TerminalID != "" {
		t.Errorf("terminal ids = %q %q %q %q, want only the first", out[0].TerminalID, out[1].TerminalID, out[2].TerminalID, out[3].TerminalID)
	}
	if reader.calls != 2 {
		t.Errorf("reads = %d, want 2 (the gone row skipped, nothing after the failure)", reader.calls)
	}
	if !reader.hadDeadline {
		t.Error("a terminal read had no deadline of its own")
	}
	// An undecodable reference is a failed read, never a herdr call.
	out = []listRow{{State: workerPresent}}
	reader = &scriptedTerminals{}
	fillTerminalIDs(t.Context(), reader, []worker.Row{{Name: "a", Ref: []byte(`{}`)}}, out)
	if reader.calls != 0 || out[0].TerminalID != "" {
		t.Errorf("undecodable ref: calls %d, id %q", reader.calls, out[0].TerminalID)
	}
}

type scriptedTerminals struct {
	replies     []error
	calls       int
	hadDeadline bool
}

func (s *scriptedTerminals) TerminalID(ctx context.Context, _ backend.Ref) (string, error) {
	s.calls++
	_, s.hadDeadline = ctx.Deadline()
	if s.calls <= len(s.replies) && s.replies[s.calls-1] != nil {
		return "", s.replies[s.calls-1]
	}
	return "term-" + strconv.Itoa(s.calls), nil
}
