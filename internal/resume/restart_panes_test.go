package resume

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
)

const resolveSID = "5e8f1915-10b7-46a3-8c57-52ab9c864bb9"

func TestResolvePane(t *testing.T) {
	other := HerdrPane{ID: "w83:p1"}
	mine := HerdrPane{ID: "w83:p2", Agent: "claude", Session: resolveSID}
	for _, tc := range []struct {
		name      string
		env       string
		panes     []HerdrPane
		listErr   error
		wantPane  string
		wantNote  string // substring; "" means no note
		ambiguous []string
	}{
		{name: "one claim replaces a stale env pane", env: "w7K:pQ",
			panes: []HerdrPane{other, mine}, wantPane: "w83:p2", wantNote: "found by session; env said w7K:pQ"},
		{name: "one claim matching the env pane needs no note", env: "w83:p2",
			panes: []HerdrPane{mine}, wantPane: "w83:p2"},
		{name: "one claim with no env pane", env: "",
			panes: []HerdrPane{mine}, wantPane: "w83:p2", wantNote: "none in its environment"},
		{name: "two claims are ambiguous", env: "w7K:pQ",
			panes:    []HerdrPane{mine, {ID: "w83:p3", Agent: "claude", Session: resolveSID}},
			wantPane: "w7K:pQ", ambiguous: []string{"w83:p2", "w83:p3"}},
		{name: "no claim falls back to the env pane", env: "w7K:pQ",
			panes: []HerdrPane{other}, wantPane: "w7K:pQ"},
		{name: "another harness with the same id is not a claim", env: "w7K:pQ",
			panes: []HerdrPane{{ID: "w83:p9", Agent: "pi", Session: resolveSID}}, wantPane: "w7K:pQ"},
		{name: "a failed list falls back and says why", env: "w7K:pQ",
			panes: []HerdrPane{mine}, listErr: errors.New("connection refused"),
			wantPane: "w7K:pQ", wantNote: "herdr pane list failed (connection refused); from its environment"},
		{name: "a rejected list says rejected", env: "w7K:pQ",
			panes: []HerdrPane{mine}, listErr: fmt.Errorf("%w: it has a pane id that is not a plain operand", errPaneListRejected),
			wantPane: "w7K:pQ", wantNote: "herdr's pane list was rejected: it has a pane id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ResolvePane(resolveSID, tc.env, tc.panes, tc.listErr)
			if r.Pane != tc.wantPane {
				t.Errorf("pane = %q, want %q", r.Pane, tc.wantPane)
			}
			if tc.wantNote == "" && r.Note != "" || !strings.Contains(r.Note, tc.wantNote) {
				t.Errorf("note = %q, want %q", r.Note, tc.wantNote)
			}
			if !slices.Equal(r.Ambiguous, tc.ambiguous) {
				t.Errorf("ambiguous = %v, want %v", r.Ambiguous, tc.ambiguous)
			}
		})
	}
}

func TestResolvePaneClipsTheListError(t *testing.T) {
	r := ResolvePane(resolveSID, "w1:p1", nil, errors.New(strings.Repeat("x", 5000)))
	if len([]rune(r.Note)) > detailLimit+100 {
		t.Fatalf("note is %d runes; the list error must go through clipDetail", len([]rune(r.Note)))
	}
}

func TestSkipAmbiguousPanes(t *testing.T) {
	s := testSession()
	plan := []RestartPlanItem{
		{Session: s, SessionID: s.SessionID, Action: ActionRestart},
		{SessionID: "bbbb", Action: ActionSkip, Reason: "its own reason"},
		{SessionID: "cccc", Action: ActionRestart},
	}
	got := SkipAmbiguousPanes(plan, map[string][]string{s.SessionID: {"w1:p1", "w1:p2"}, "bbbb": {"w1:p3", "w1:p4"}})
	if got[0].Action != ActionSkip || !strings.Contains(got[0].Reason, "2 panes (w1:p1, w1:p2)") || !strings.Contains(got[0].Reason, ManualResume(s.SessionID)) {
		t.Errorf("ambiguous item = %+v", got[0])
	}
	if got[1].Reason != "its own reason" {
		t.Errorf("an existing skip lost its reason: %+v", got[1])
	}
	if got[2].Action != ActionRestart {
		t.Errorf("an unambiguous item was re-planned: %+v", got[2])
	}
}

func TestParsePaneList(t *testing.T) {
	// Shape measured on herdr 0.9.1: a pane with no agent has no
	// agent_session key; a recognized agent with no session has it null.
	out := `{"result":{"type":"pane_list","panes":[
		{"pane_id":"w83:p1","cwd":"/x"},
		{"pane_id":"w7D:p2N","agent":"claude","agent_session":null},
		{"pane_id":"w83:p2","agent":"claude","agent_session":{"agent":"claude","kind":"id","source":"herdr:claude","value":"` + resolveSID + `"}}
	]}}`
	got, err := parsePaneList(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []HerdrPane{{ID: "w83:p1"}, {ID: "w7D:p2N"}, {ID: "w83:p2", Agent: "claude", Session: resolveSID}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for name, bad := range map[string]string{
		"not JSON":       "pane w83:p1",
		"no panes":       `{"result":{}}`,
		"empty pane id":  `{"result":{"panes":[{"pane_id":""}]}}`,
		"odd pane id":    `{"result":{"panes":[{"pane_id":"w1:p1;rm"}]}}`,
		"overlong id":    `{"result":{"panes":[{"pane_id":"` + strings.Repeat("x", 65) + `"}]}}`,
		"flag-shaped id": `{"result":{"panes":[{"pane_id":"--help"}]}}`,
	} {
		if _, err := parsePaneList(bad); !errors.Is(err, errPaneListRejected) {
			t.Errorf("%s: %q gave %v, want a rejected list", name, bad, err)
		}
	}
}

func TestListPanesRunsOneReadOnlyCall(t *testing.T) {
	run := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return `{"result":{"panes":[]}}`, nil }}
	panes, err := SystemRestartEnv{runner: run, herdr: "/opt/tools/herdr"}.ListPanes(context.Background())
	if err != nil || len(panes) != 0 {
		t.Fatalf("panes %v, err %v", panes, err)
	}
	if len(run.Calls) != 1 || run.Calls[0].Name != "/opt/tools/herdr" || !slices.Equal(run.Calls[0].Args, []string{"pane", "list"}) {
		t.Fatalf("calls %+v", run.Calls)
	}
	if _, err := (SystemRestartEnv{}).ListPanes(context.Background()); err == nil {
		t.Fatal("no runner: listed")
	}
}

// movedPaneEnv is fakeEnv where some pane ids are gone (herdr answers
// pane_not_found for them), and every pane a check, clear, or relaunch names
// is recorded.
type movedPaneEnv struct {
	*fakeEnv
	gone map[string]bool
	seen []string
}

func (m *movedPaneEnv) Pane(ctx context.Context, pane string) (PaneState, error) {
	m.seen = append(m.seen, pane)
	if m.gone[pane] {
		return PaneState{}, fmt.Errorf("pane %s: %w", pane, ErrPaneGone)
	}
	return m.fakeEnv.Pane(ctx, pane)
}
func (m *movedPaneEnv) Screen(ctx context.Context, pane string) (string, error) {
	m.seen = append(m.seen, pane)
	return m.fakeEnv.Screen(ctx, pane)
}
func (m *movedPaneEnv) ClearInput(ctx context.Context, pane string) error {
	m.seen = append(m.seen, pane)
	return m.fakeEnv.ClearInput(ctx, pane)
}
func (m *movedPaneEnv) Relaunch(ctx context.Context, pane, id string) error {
	m.seen = append(m.seen, pane)
	return m.fakeEnv.Relaunch(ctx, pane, id)
}

// runMoved runs one session whose pane testPane is gone, with relist as the
// pane-list re-read, and returns the final event and how often it was read.
func runMoved(t *testing.T, env *movedPaneEnv, relist func() ([]HerdrPane, error)) (RestartEvent, int) {
	t.Helper()
	var events []RestartEvent
	var clock fakeClock
	opts := clock.opts(&events)
	reads := 0
	if relist != nil {
		opts.Panes = func(context.Context) ([]HerdrPane, error) { reads++; return relist() }
	}
	finals := RunRestart(context.Background(), env, PlanRestart([]OutdatedSession{testSession()}, nil), opts)
	if len(finals) != 1 {
		t.Fatalf("got %d finals", len(finals))
	}
	return finals[0], reads
}

func TestRunRestart_GonePaneIsFoundAgainBySession(t *testing.T) {
	env := &movedPaneEnv{fakeEnv: newFakeEnv(), gone: map[string]bool{testPane: true}}
	final, reads := runMoved(t, env, func() ([]HerdrPane, error) {
		return []HerdrPane{{ID: "w9:p4", Agent: "claude", Session: testSID}}, nil
	})
	if final.State != StateResumed || !strings.Contains(final.Detail, "pane w9:p4") || reads != 1 {
		t.Fatalf("final = %+v, reads = %d", final, reads)
	}
	// The first check met the gone pane; every one after the re-read used the
	// found pane, through the relaunch.
	if env.seen[0] != testPane || slices.Contains(env.seen[1:], testPane) || env.relaunched != 1 {
		t.Fatalf("panes = %v, relaunched = %d", env.seen, env.relaunched)
	}
}

func TestRunRestart_GonePaneStillGoneIsIncomplete(t *testing.T) {
	for name, tc := range map[string]struct {
		relist    func() ([]HerdrPane, error)
		wantState RestartState
		wantText  string
		wantReads int
	}{
		"no other pane": {func() ([]HerdrPane, error) { return []HerdrPane{{ID: "w9:p1"}}, nil },
			StatePaneGone, "shows the session in no other pane", 1},
		"re-read fails": {func() ([]HerdrPane, error) { return nil, errors.New("connection refused") },
			StatePaneGone, "re-reading the pane list: herdr pane list failed (connection refused)", 1},
		"the found pane is gone too": {func() ([]HerdrPane, error) {
			return []HerdrPane{{ID: "w9:p4", Agent: "claude", Session: testSID}}, nil
		}, StatePaneGone, "pane w9:p4 no longer exists", 1},
		"now in two panes": {func() ([]HerdrPane, error) {
			return []HerdrPane{{ID: "w9:p4", Agent: "claude", Session: testSID}, {ID: "w9:p5", Agent: "claude", Session: testSID}}, nil
		}, StateSkipped, "2 panes (w9:p4, w9:p5)", 1},
		"no re-read seam": {nil, StatePaneGone, "pane " + testPane + " no longer exists", 0},
	} {
		t.Run(name, func(t *testing.T) {
			env := &movedPaneEnv{fakeEnv: newFakeEnv(), gone: map[string]bool{testPane: true, "w9:p4": name == "the found pane is gone too"}}
			final, reads := runMoved(t, env, tc.relist)
			if final.State != tc.wantState || !strings.Contains(final.Detail, tc.wantText) || final.Manual != ManualResume(testSID) || reads != tc.wantReads {
				t.Fatalf("final = %+v, reads = %d", final, reads)
			}
			if env.terminated+env.prepared+env.relaunched != 0 {
				t.Fatal("a refused session was touched")
			}
		})
	}
}

func TestRestartResultCountsAGonePaneIncomplete(t *testing.T) {
	if !(RestartResult{PaneGone: 1}).Incomplete() {
		t.Fatal("a session with no herdr pane must make the run incomplete, so the watcher retries it")
	}
	if (RestartResult{}).Incomplete() {
		t.Fatal("an empty result is not incomplete")
	}
}

// A session with no pane reports why from the resolution: a failed list is
// not "none found by session".
func TestPlanNamesAFailedListForAPanelessSession(t *testing.T) {
	s := testSession()
	s.Pane = ""
	list, _ := resolvePanes([]OutdatedSession{s}, nil, errors.New("connection refused"))
	plan := PlanRestart(list, nil)
	if plan[0].Action != ActionManual || !strings.Contains(plan[0].Reason, "no herdr pane (herdr pane list failed (connection refused) and none in its environment)") {
		t.Fatalf("plan = %+v", plan[0])
	}
	list, _ = resolvePanes([]OutdatedSession{s}, nil, nil)
	if plan := PlanRestart(list, nil); !strings.Contains(plan[0].Reason, "none found by session and none in its environment") {
		t.Fatalf("plan = %+v", plan[0])
	}
}

// TestRestartDefaultPanesUsesHerdrBin pins the production pane-list seam: it
// runs the configured herdr binary with exactly `pane list`.
func TestRestartDefaultPanesUsesHerdrBin(t *testing.T) {
	run := &exec.FakeRunner{RunFunc: func(string, []string) (string, error) { return `{"result":{"panes":[]}}`, nil }}
	req := RestartRequest{Runner: run, DryRun: true, HerdrBin: "/opt/tools/herdr"}
	req.fill()
	if _, err := req.Panes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(run.Calls) != 1 || run.Calls[0].Name != "/opt/tools/herdr" || !slices.Equal(run.Calls[0].Args, []string{"pane", "list"}) {
		t.Fatalf("calls %+v", run.Calls)
	}
}

// The pane list is re-read at most once per session: a found pane that goes
// missing at the check after Prepare is not chased again.
func TestRunRestart_ReReadsThePaneListOncePerSession(t *testing.T) {
	env := &movedPaneEnv{fakeEnv: newFakeEnv(), gone: map[string]bool{testPane: true}}
	env.afterPrepare = func(*fakeEnv) { env.gone["w9:p4"] = true }
	final, reads := runMoved(t, env, func() ([]HerdrPane, error) {
		return []HerdrPane{{ID: "w9:p4", Agent: "claude", Session: testSID}}, nil
	})
	if final.State != StatePaneGone || !strings.Contains(final.Detail, "pane w9:p4 no longer exists") || reads != 1 || env.terminated != 0 {
		t.Fatalf("final = %+v, reads = %d, terminated = %d", final, reads, env.terminated)
	}
}
