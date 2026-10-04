package resume

import (
	"context"
	"errors"
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
			panes: []HerdrPane{mine}, listErr: errors.New("herdr pane list: connection refused"),
			wantPane: "w7K:pQ", wantNote: "herdr pane list failed (herdr pane list: connection refused)"},
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
		if _, err := parsePaneList(bad); err == nil {
			t.Errorf("%s: accepted %q", name, bad)
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
