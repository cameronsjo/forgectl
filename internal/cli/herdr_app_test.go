// SPDX-License-Identifier: Apache-2.0 WITH Commons-Clause

package cli

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr"
)

// appWorld is a fake herdr-app window: `herdr-app ls`, `hc ls`, and the `do`
// commands that change tab order.
type appWorld struct {
	mu     sync.Mutex
	spaces []*appFakeSpace
}

type appFakeSpace struct {
	id, name string
	active   bool
	tabs     []appFakeTab
}

type appFakeTab struct {
	id, name, cwd string
	active        bool
}

func newAppWorld() *appWorld {
	return &appWorld{spaces: []*appFakeSpace{
		{id: "spF", name: "forge", active: true, tabs: []appFakeTab{
			{id: "tbF2", name: "second", cwd: "/r/forge/b"},
			{id: "tbF1", name: "first", cwd: "/r/forge/a", active: true},
		}},
		{id: "spM", name: "misc", tabs: []appFakeTab{
			{id: "tbM1", name: "wants-forge", cwd: "/r/forge/c", active: true},
			{id: "tbM2", name: "stays", cwd: "/r/other"},
		}},
	}}
}

func (w *appWorld) ids() []string {
	var out []string
	for _, s := range w.spaces {
		for _, t := range s.tabs {
			out = append(out, t.id)
		}
	}
	return out
}

func (w *appWorld) lsJSON() string {
	type term struct {
		Terminal string `json:"terminal"`
		Pane     string `json:"pane"`
		App      string `json:"app"`
	}
	type tab struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Active    bool   `json:"active"`
		Terminals []term `json:"terminals"`
	}
	type space struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Active bool   `json:"active"`
		Tabs   []tab  `json:"tabs"`
	}
	var doc struct {
		Spaces []space `json:"spaces"`
	}
	for _, s := range w.spaces {
		sp := space{ID: s.id, Name: s.name, Active: s.active}
		for _, t := range s.tabs {
			sp.Tabs = append(sp.Tabs, tab{ID: t.id, Name: t.name, Active: t.active,
				Terminals: []term{{Terminal: "t-" + t.id, Pane: "p-" + t.id, App: "zsh"}}})
		}
		doc.Spaces = append(doc.Spaces, sp)
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

func (w *appWorld) hcJSON() string {
	type t struct {
		Address string `json:"address"`
		CWD     string `json:"cwd"`
	}
	var doc struct {
		Terminals []t `json:"terminals"`
	}
	for _, s := range w.spaces {
		for _, tb := range s.tabs {
			doc.Terminals = append(doc.Terminals, t{Address: "t-" + tb.id, CWD: tb.cwd})
		}
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

func (w *appWorld) move(id string, index int) {
	for _, s := range w.spaces {
		for i, tb := range s.tabs {
			if tb.id != id {
				continue
			}
			s.tabs = append(s.tabs[:i], s.tabs[i+1:]...)
			at := min(index-1, len(s.tabs))
			s.tabs = append(s.tabs[:at], append([]appFakeTab{tb}, s.tabs[at:]...)...)
			return
		}
	}
}

func (w *appWorld) runner() *exec.FakeRunner {
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		switch {
		case name == herdr.AppBinary && len(args) == 1 && args[0] == "ls":
			return w.lsJSON(), nil
		case name == herdr.HCBinary:
			return w.hcJSON(), nil
		case name == herdr.AppBinary && len(args) == 3 && args[0] == "do" && args[1] == "tab.move":
			var a struct {
				ID    string `json:"id"`
				Index int    `json:"index"`
			}
			_ = json.Unmarshal([]byte(args[2]), &a)
			w.move(a.ID, a.Index)
			return `{"ok":true}`, nil
		case name == herdr.AppBinary && len(args) > 1 && args[0] == "do":
			return `{"ok":true}`, nil
		}
		return "", nil
	}}
}

func setAppSeams(t *testing.T) *[]string {
	t.Helper()
	var events []string
	setHerdrSeams(t, herdrSeams{env: map[string]string{"HC_TERMINAL_ID": "t-tbF1"}, events: &events})
	oldApp := herdrCheckApp
	t.Cleanup(func() { herdrCheckApp = oldApp })
	herdrCheckApp = func(context.Context, exec.Runner) error { events = append(events, "app"); return nil }
	return &events
}

func TestHerdrOrganize_App_ReportMakesNoMoves(t *testing.T) {
	setAppSeams(t)
	w := newAppWorld()
	r := runOrganizeApp(t, w)
	if r.err != nil {
		t.Fatalf("err = %v", r.err)
	}
	for _, c := range r.runner.Calls {
		if c.Name == herdr.Binary {
			t.Errorf("the herdr 0.9 binary was called from a herdr-app terminal: %v", c)
		}
		if len(c.Args) > 0 && c.Args[0] == "do" {
			t.Errorf("a report ran a mutation: %v", c)
		}
	}
	for _, want := range []string{"blocked", "wants-forge", "move it by hand", "order    forge:", `"first"`, "position 1"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("report lacks %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "move     ") {
		t.Errorf("report offers a cross-space move herdr-app cannot make:\n%s", r.stdout)
	}
}

func TestHerdrOrganize_App_ApplyReordersAndNeverCrossesSpaces(t *testing.T) {
	events := setAppSeams(t)
	w := newAppWorld()
	r := runOrganizeApp(t, w, "--apply")
	if r.err != nil {
		t.Fatalf("err = %v\nstdout:\n%s", r.err, r.stdout)
	}
	if got := strings.Join(*events, ","); got != "app,lock" {
		t.Errorf("gate order = %q, want the herdr-app check before the lock", got)
	}
	if got := w.spaces[0].tabs[0].id; got != "tbF1" {
		t.Errorf("forge order = %v, want tbF1 first", w.ids())
	}
	if len(w.spaces[0].tabs) != 2 || len(w.spaces[1].tabs) != 2 {
		t.Errorf("a tab changed space: %v", w.ids())
	}
	var moves []string
	for _, c := range r.runner.Calls {
		if c.Name == herdr.Binary {
			t.Errorf("herdr 0.9 binary called: %v", c)
		}
		if len(c.Args) > 1 && c.Args[0] == "do" && c.Args[1] == "tab.move" {
			moves = append(moves, c.Args[2])
		}
	}
	if len(moves) != 1 || moves[0] != `{"id":"tbF1","index":1}` {
		t.Errorf("tab.move calls = %v, want exactly one for tbF1 to position 1", moves)
	}
	// The last mutation selects the caller's space, after every tab select.
	var dos [][]string
	for _, c := range r.runner.Calls {
		if len(c.Args) > 1 && c.Args[0] == "do" {
			dos = append(dos, c.Args)
		}
	}
	if last := dos[len(dos)-1]; last[1] != "space.select" || last[2] != `{"id":"spF"}` {
		t.Errorf("last mutation = %v, want space.select of the caller's space spF; all: %v", last, dos)
	}
	if prev := dos[len(dos)-2]; prev[1] != "tab.select" {
		t.Errorf("the space select should follow the tab selects: %v", dos)
	}
	if !strings.Contains(r.stdout, "ordered") || !strings.Contains(r.stdout, "focus restored") {
		t.Errorf("apply report:\n%s", r.stdout)
	}
}

func TestHerdrOrganize_App_ApplyRefusesBeforeChangingWhenCommandsMissing(t *testing.T) {
	setAppSeams(t)
	herdrCheckApp = func(context.Context, exec.Runner) error { return herdr.ErrUnsupportedMove }
	w := newAppWorld()
	r := runOrganizeApp(t, w, "--apply")
	if r.err == nil || ExitCode(r.err) != 2 {
		t.Fatalf("err = %v (exit %d), want exit 2", r.err, ExitCode(r.err))
	}
	if len(r.runner.Calls) != 0 {
		t.Errorf("calls before refusal: %v", r.runner.Calls)
	}
}

func TestHerdrOrganize_NoHost_NamesBothHosts(t *testing.T) {
	setHerdrSeams(t, herdrSeams{sessionErr: herdr.ErrNotInSession})
	r := runOrganize(t, organizeCfg(), nil)
	if ExitCode(r.err) != 2 || r.err == nil {
		t.Fatalf("err = %v", r.err)
	}
	for _, want := range []string{"not inside a herdr session", "herdr-app", "HC_TERMINAL_ID"} {
		if !strings.Contains(r.err.Error(), want) {
			t.Errorf("refusal lacks %q: %v", want, r.err)
		}
	}
}

func TestHerdrOrganize_BothHostsSet_UsesHerdr(t *testing.T) {
	events := []string{}
	setHerdrSeams(t, herdrSeams{env: map[string]string{"HERDR_ENV": "1", "HC_TERMINAL_ID": "t1"}, events: &events})
	runOrganize(t, organizeCfg(), nil)
	if len(events) == 0 || events[0] != "gate" {
		t.Errorf("events = %v, want the herdr 0.9 session gate to run", events)
	}
}

func runOrganizeApp(t *testing.T, w *appWorld, args ...string) organizeRun {
	t.Helper()
	cfg := organizeCfg()
	cfg.Herdr.Organize.WorkspaceOrder = []string{"forge", "misc"}
	return runOrganizeWith(t, cfg, w.runner(), args...)
}

func TestHerdrOrganize_App_ApplyRefusesAWindowThatDoesNotShowThisTerminal(t *testing.T) {
	setAppSeams(t)
	lookupHerdrEnv = func(k string) (string, bool) {
		if k == "HC_TERMINAL_ID" {
			return "t-elsewhere", true
		}
		return "", false
	}
	w := newAppWorld()
	r := runOrganizeApp(t, w, "--apply")
	if r.err == nil || ExitCode(r.err) != 2 || !strings.Contains(r.err.Error(), "this terminal is not in the window herdr-app reports") {
		t.Fatalf("err = %v (exit %d), want exit 2 with the window reason", r.err, ExitCode(r.err))
	}
	for _, c := range r.runner.Calls {
		if len(c.Args) > 0 && c.Args[0] == "do" {
			t.Errorf("a mutation ran before the refusal: %v", c)
		}
	}
}

func TestHerdrOrganize_App_ReportOnlyNotesAWindowMismatchButDoesNotRefuse(t *testing.T) {
	setAppSeams(t)
	lookupHerdrEnv = func(k string) (string, bool) {
		if k == "HC_TERMINAL_ID" {
			return "t-elsewhere", true
		}
		return "", false
	}
	r := runOrganizeApp(t, newAppWorld())
	if r.err != nil {
		t.Fatalf("a report must not refuse: %v", r.err)
	}
	if !strings.Contains(r.stdout, "note: this terminal is not in the window herdr-app reports") {
		t.Errorf("report lacks the note:\n%s", r.stdout)
	}
}

func TestHerdrOrganize_App_ReportInTheWindowHasNoNote(t *testing.T) {
	setAppSeams(t)
	r := runOrganizeApp(t, newAppWorld())
	if r.err != nil || strings.Contains(r.stdout, "note:") {
		t.Errorf("err = %v; stdout:\n%s", r.err, r.stdout)
	}
}

// A name herdr-app reports is chosen by whatever runs in the terminal, so it
// is untrusted: it must not reach the human report or --json as a raw
// control or bidi character.
func TestHerdrOrganize_App_HostileNamesAreNeutralized(t *testing.T) {
	const hostile = "\x1b]0;x\x07evil\u202e"
	setAppSeams(t)
	w := newAppWorld()
	w.spaces[1].name = hostile
	w.spaces[1].tabs[0].name = hostile
	for _, args := range [][]string{nil, {"--json"}, {"--explain"}} {
		r := runOrganizeApp(t, w, args...)
		if r.err != nil {
			t.Fatalf("%v: err = %v", args, r.err)
		}
		for name, out := range map[string]string{"stdout": r.stdout, "stderr": r.stderr} {
			if strings.ContainsAny(out, "\x1b\x07\u202e") {
				t.Errorf("%v %s carries a raw control or bidi character: %q", args, name, out)
			}
		}
		if all := r.stdout + r.stderr; !strings.Contains(all, "evil") {
			t.Errorf("%v: the hostile name never reached the output, so the check proves nothing:\n%s", args, all)
		}
	}
}

func TestHerdrOrganize_App_JSONMarksUnsupportedMoves(t *testing.T) {
	setAppSeams(t)
	r := runOrganizeApp(t, newAppWorld(), "--json")
	if r.err != nil {
		t.Fatalf("err = %v", r.err)
	}
	var doc struct {
		Plan struct {
			Moves []map[string]any `json:"moves"`
		} `json:"plan"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("stdout: %v\n%s", err, r.stdout)
	}
	if len(doc.Plan.Moves) != 1 || doc.Plan.Moves[0]["unsupported"] != true || doc.Plan.Moves[0]["blocked"] != true {
		t.Errorf("moves = %v, want one blocked move with unsupported true", doc.Plan.Moves)
	}
}

func TestHerdrOrganize_BothHostsSet_RefusalExplainsInheritedHerdrVars(t *testing.T) {
	setHerdrSeams(t, herdrSeams{
		sessionErr: herdr.ErrNotInSession,
		env:        map[string]string{"HERDR_ENV": "1", "HC_TERMINAL_ID": "t1"},
	})
	r := runOrganize(t, organizeCfg(), nil)
	if ExitCode(r.err) != 2 || r.err == nil {
		t.Fatalf("err = %v", r.err)
	}
	for _, want := range []string{"not inside a herdr session", "inherits HERDR_ENV", "unset HERDR_ENV HERDR_SOCKET_PATH HERDR_PANE_ID"} {
		if !strings.Contains(r.err.Error(), want) {
			t.Errorf("refusal lacks %q: %v", want, r.err)
		}
	}
}

func TestHerdrOrganize_HerdrOnly_RefusalHasNoHerdrAppHint(t *testing.T) {
	setHerdrSeams(t, herdrSeams{sessionErr: herdr.ErrNotInSession, env: map[string]string{"HERDR_ENV": "1"}})
	r := runOrganize(t, organizeCfg(), nil)
	if r.err == nil || strings.Contains(r.err.Error(), "herdr-app") {
		t.Errorf("err = %v, want no herdr-app hint", r.err)
	}
}
