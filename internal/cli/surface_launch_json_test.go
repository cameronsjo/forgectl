//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
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
	got := workerLaunchResult(t.Context(), fakeTerminalReader{id: "term-3"}, attempt, testRepoTop)
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

// list reads a present worker's terminal_id, and leaves it out when the row's
// reference does not decode or the read fails.
func TestRowTerminalID(t *testing.T) {
	encoded, err := testHerdrRef(t).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	row := worker.Row{Name: "w1", Ref: encoded}
	if got := rowTerminalID(t.Context(), fakeTerminalReader{id: "term-3"}, row); got != "term-3" {
		t.Errorf("terminal_id = %q, want term-3", got)
	}
	if got := rowTerminalID(t.Context(), fakeTerminalReader{err: errors.New("x")}, row); got != "" {
		t.Errorf("terminal_id = %q after a failed read, want none", got)
	}
	if got := rowTerminalID(t.Context(), fakeTerminalReader{id: "term-3"}, worker.Row{Name: "w1", Ref: []byte(`{}`)}); got != "" {
		t.Errorf("terminal_id = %q for an undecodable ref, want none", got)
	}
}
