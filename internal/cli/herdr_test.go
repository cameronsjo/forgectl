package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/config"
	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr"
	"github.com/cameronsjo/forgectl/internal/module"
)

// herdrWorld is a fake herdr session: what `workspace list`, `tab list`, and
// `pane list` answer.
type herdrWorld struct {
	workspaces []herdr.Workspace
	tabs       map[string][]herdr.Tab
	panes      []herdr.Pane

	// Mutation state, used by the apply tests (herdr_apply_test.go).
	active     map[string]string // workspace id -> active tab id
	focusedTab string            // tab holding the caller's focus
	nextID     int
	// renumberAll makes a move out of a workspace renumber every remaining tab
	// in it, the worst case for a plan that holds tab ids.
	renumberAll bool
	// intercept, when set, sees every mutating call first. Returning handled
	// short-circuits the world's own handling.
	intercept func(args []string) (out string, err error, handled bool)
	focusLog  []string // "tab:<id>" and "workspace:<id>" in call order
}

func newWorld(wss ...herdr.Workspace) *herdrWorld {
	return &herdrWorld{workspaces: wss, tabs: map[string][]herdr.Tab{}}
}

func (w *herdrWorld) tab(wsID, tabID, term, cwd, title string) *herdrWorld {
	w.tabs[wsID] = append(w.tabs[wsID], herdr.Tab{TabID: tabID, WorkspaceID: wsID, Label: title})
	w.panes = append(w.panes, herdr.Pane{
		PaneID: tabID + "-p", TabID: tabID, WorkspaceID: wsID, TerminalID: term,
		CWD: cwd, TerminalTitleStripped: title,
	})
	return w
}

func envelope(t *testing.T, key string, v any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"id": "x", "result": map[string]any{key: v}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (w *herdrWorld) runner(t *testing.T) *exec.FakeRunner {
	t.Helper()
	return &exec.FakeRunner{RunFunc: func(name string, args []string) (string, error) {
		if name != herdr.Binary {
			t.Errorf("ran %q, want only herdr", name)
			return "", errors.New("unexpected binary")
		}
		if w.intercept != nil {
			if out, err, handled := w.intercept(args); handled {
				return out, err
			}
		}
		if out, ok := w.mutate(t, args); ok {
			return out, nil
		}
		switch got := strings.Join(args, " "); {
		case got == "workspace list":
			return envelope(t, "workspaces", w.workspaceRows()), nil
		case got == "pane list":
			return envelope(t, "panes", w.paneRows()), nil
		case strings.HasPrefix(got, "tab list --workspace "):
			id := strings.TrimPrefix(got, "tab list --workspace ")
			tabs := w.tabs[id]
			if tabs == nil {
				tabs = []herdr.Tab{}
			}
			return envelope(t, "tabs", tabs), nil
		}
		t.Errorf("unexpected herdr call: %v", args)
		return "", errors.New("unexpected call")
	}}
}

func hws(id, label string, number int) herdr.Workspace {
	return herdr.Workspace{WorkspaceID: id, Label: label, Number: number}
}

// herdrSeams describes the process the command believes it runs in.
type herdrSeams struct {
	env         map[string]string
	sessionErr  error
	forkErr     error
	legacyRules bool
	// lockContended makes the lock announce a wait before running fn.
	lockContended bool
	// events, when set, receives "gate", "fork" and "lock" as they happen, so a
	// test can order them against the herdr calls the fake runner records.
	events *[]string
}

func setHerdrSeams(t *testing.T, s herdrSeams) {
	t.Helper()
	oldEnv, oldCheck, oldRoot, oldHome, oldExists := lookupHerdrEnv, herdrCheckSession, herdrProjectsRoot, herdrUserHome, herdrFileExists
	oldFork, oldLock, oldLockPath := herdrCheckFork, herdrWithLock, herdrLockPath
	t.Cleanup(func() {
		lookupHerdrEnv, herdrCheckSession, herdrProjectsRoot, herdrUserHome, herdrFileExists = oldEnv, oldCheck, oldRoot, oldHome, oldExists
		herdrCheckFork, herdrWithLock, herdrLockPath = oldFork, oldLock, oldLockPath
	})
	note := func(e string) {
		if s.events != nil {
			*s.events = append(*s.events, e)
		}
	}
	lookupHerdrEnv = func(k string) (string, bool) { v, ok := s.env[k]; return v, ok }
	herdrCheckSession = func(func(string) (string, bool)) error { note("gate"); return s.sessionErr }
	herdrCheckFork = func(context.Context, exec.Runner) error { note("fork"); return s.forkErr }
	herdrLockPath = func() (string, error) { return "/lock/herdr-organize", nil }
	herdrWithLock = func(_ string, onWait func(), fn func() error) error {
		note("lock")
		if s.lockContended && onWait != nil {
			onWait()
		}
		return fn()
	}
	herdrProjectsRoot = func() string { return "/r" }
	herdrUserHome = func() (string, error) { return "/home/u", nil }
	herdrFileExists = func(p string) bool { return s.legacyRules && p == "/home/u/.config/herdr-organize/rules.toml" }
}

func organizeCfg() config.Config {
	return config.Config{Herdr: config.HerdrConfig{Organize: config.HerdrOrganizeConfig{
		Default:        "misc",
		WorkspaceOrder: []string{"forge", "misc"},
		Rules: []config.HerdrOrganizeRule{
			{Glob: "/r/forge/* :: *", Workspace: "forge"},
			{Glob: "/r/home/* :: *", Workspace: "home"},
		},
	}}}
}

type organizeRun struct {
	stdout, stderr string
	err            error
	runner         *exec.FakeRunner
}

func runOrganize(t *testing.T, cfg config.Config, w *herdrWorld, args ...string) organizeRun {
	t.Helper()
	fake := &exec.FakeRunner{}
	if w != nil {
		fake = w.runner(t)
	}
	cmd := newHerdrCmd(module.Deps{Cfg: cfg, Runner: fake})
	// The real root sets both (root.go); a standalone command would otherwise
	// print usage into stdout on any error and corrupt a --json document.
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(append([]string{"organize"}, args...))
	err := cmd.Execute()
	return organizeRun{stdout: out.String(), stderr: errb.String(), err: err, runner: fake}
}

var inSession = herdrSeams{env: map[string]string{"HERDR_ENV": "1"}}

func TestHerdrOrganize_NotInHerdrAndNoConfig_ReportsBothInOneRun(t *testing.T) {
	setHerdrSeams(t, herdrSeams{sessionErr: herdr.ErrNotInSession})
	r := runOrganize(t, config.Config{}, nil)
	if r.err == nil || ExitCode(r.err) != 2 {
		t.Fatalf("err = %v (exit %d), want exit 2", r.err, ExitCode(r.err))
	}
	msg := r.err.Error()
	if !strings.Contains(msg, "no rules are configured") || !strings.Contains(msg, "not inside a herdr session") {
		t.Errorf("error = %q, want both the config problem and the session problem", msg)
	}
	if len(r.runner.Calls) != 0 {
		t.Errorf("herdr was called %d time(s) before the checks finished: %v", len(r.runner.Calls), r.runner.Calls)
	}
}

func TestHerdrOrganize_NotInHerdr_WithConfig(t *testing.T) {
	setHerdrSeams(t, herdrSeams{sessionErr: herdr.ErrNotInSession})
	r := runOrganize(t, organizeCfg(), nil)
	if ExitCode(r.err) != 2 || r.err == nil || !strings.Contains(r.err.Error(), "not inside a herdr session") {
		t.Errorf("err = %v (exit %d), want exit 2 naming the missing session", r.err, ExitCode(r.err))
	}
}

func TestHerdrOrganize_InvalidConfig_ExitsTwoBeforeHerdr(t *testing.T) {
	setHerdrSeams(t, inSession)
	cfg := organizeCfg()
	cfg.Herdr.Organize.Rules[1].Workspace = ""
	r := runOrganize(t, cfg, nil)
	if ExitCode(r.err) != 2 || r.err == nil || !strings.Contains(r.err.Error(), `[[herdr.organize.rule]] #2 (glob "/r/home/* :: *"): workspace is empty`) {
		t.Errorf("err = %v (exit %d)", r.err, ExitCode(r.err))
	}
	if len(r.runner.Calls) != 0 {
		t.Errorf("herdr was called: %v", r.runner.Calls)
	}
}

func TestHerdrOrganize_UncompilableGlobIsRefused(t *testing.T) {
	setHerdrSeams(t, inSession)
	cfg := organizeCfg()
	cfg.Herdr.Organize.Rules[0].Glob = "/r/[z-a]/* :: *"
	r := runOrganize(t, cfg, nil)
	if ExitCode(r.err) != 2 || r.err == nil || !strings.Contains(r.err.Error(), `#1 (glob "/r/[z-a]/* :: *"): invalid glob`) {
		t.Errorf("err = %v (exit %d), want exit 2 naming the rule and the invalid glob", r.err, ExitCode(r.err))
	}
	if len(r.runner.Calls) != 0 {
		t.Errorf("herdr was called: %v", r.runner.Calls)
	}
}

func TestHerdrOrganize_NoRules_Variants(t *testing.T) {
	sectionPresent, err := config.DecodeStrict([]byte("[herdr.organize]\n"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		cfg      config.Config
		seams    herdrSeams
		contains []string
		absent   []string
	}{
		{
			name:     "no legacy file, no section",
			cfg:      config.Config{},
			seams:    inSession,
			contains: []string{"no rules are configured", "forgectl init adds a commented [herdr.organize] section", "forgectl config shows the path"},
			absent:   []string{"no longer reads", "HERDR_ORGANIZE_RULES"},
		},
		{
			name:  "legacy rules file found",
			cfg:   config.Config{},
			seams: herdrSeams{env: map[string]string{"HERDR_ENV": "1"}, legacyRules: true},
			contains: []string{
				"forgectl no longer reads ~/.config/herdr-organize/rules.toml",
				"default", "workspace_order", "[[rule]]", "[[herdr.organize.rule]]", "[herdr.organize]",
			},
		},
		{
			name:     "HERDR_ORGANIZE_RULES set",
			cfg:      config.Config{},
			seams:    herdrSeams{env: map[string]string{"HERDR_ENV": "1", "HERDR_ORGANIZE_RULES": "/x/rules.toml"}},
			contains: []string{"HERDR_ORGANIZE_RULES is set, and forgectl ignores it"},
		},
		{
			name:     "section exists from init: point at editing, never at init",
			cfg:      sectionPresent,
			seams:    inSession,
			contains: []string{"no rules are configured", "edit the [herdr.organize] section", "forgectl config shows the path"},
			absent:   []string{"forgectl init"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setHerdrSeams(t, tt.seams)
			r := runOrganize(t, tt.cfg, nil)
			if r.err == nil || ExitCode(r.err) != 2 {
				t.Fatalf("err = %v (exit %d), want exit 2", r.err, ExitCode(r.err))
			}
			for _, want := range tt.contains {
				if !strings.Contains(r.err.Error(), want) {
					t.Errorf("error missing %q:\n%s", want, r.err)
				}
			}
			for _, bad := range tt.absent {
				if strings.Contains(r.err.Error(), bad) {
					t.Errorf("error contains %q, which it must not:\n%s", bad, r.err)
				}
			}
		})
	}
}

func TestHerdrScaffold_DecodesToAnEmptyExistingSection(t *testing.T) {
	cfg, err := config.DecodeStrict([]byte(herdrScaffold))
	if err != nil {
		t.Fatalf("the scaffold does not decode: %v", err)
	}
	if !cfg.HasHerdrOrganizeSection() {
		t.Error("the scaffold's [herdr.organize] header must exist once written")
	}
	if len(cfg.Herdr.Organize.Rules) != 0 || cfg.Herdr.Organize.Default != "" {
		t.Errorf("the scaffold must not install active rules: %+v", cfg.Herdr.Organize)
	}
	if !hasSection([]byte(herdrScaffold), "herdr") {
		t.Error("init's own presence check does not see the scaffold, so a second init would append it again")
	}
	setHerdrSeams(t, inSession)
	msg := organizeConfigProblem(cfg)
	if msg == nil || strings.Contains(msg.Error(), "forgectl init") {
		t.Errorf("after init the message must point at editing, not at init: %v", msg)
	}
}

// listOnly asserts the run made only list calls: no mutation, and no
// `tab move --help` capability probe (a dry run needs no fork).
func listOnly(t *testing.T, r organizeRun) {
	t.Helper()
	if len(r.runner.Calls) == 0 {
		t.Error("no herdr calls were made; the assertion would pass vacuously")
	}
	for _, c := range r.runner.Calls {
		got := strings.Join(c.Args, " ")
		if got != "workspace list" && got != "pane list" && !strings.HasPrefix(got, "tab list --workspace ") {
			t.Errorf("dry run made a non-list herdr call: %s", got)
		}
	}
}

func TestHerdrOrganize_DryRun_NothingToDo(t *testing.T) {
	setHerdrSeams(t, inSession)
	w := newWorld(hws("w1", "forge", 1), hws("w2", "misc", 2)).
		tab("w1", "t1", "term1", "/r/forge/a", "a").
		tab("w2", "t2", "term2", "/r/x/b", "b")
	r := runOrganize(t, organizeCfg(), w)
	if r.err != nil {
		t.Fatalf("err = %v", r.err)
	}
	listOnly(t, r)
	if !strings.Contains(r.stdout, "organized: 2 tabs in 2 workspaces; nothing to do") {
		t.Errorf("stdout = %q", r.stdout)
	}
	if strings.Contains(r.stdout, "--apply") {
		t.Errorf("stdout suggests --apply with nothing pending: %q", r.stdout)
	}
}

func TestHerdrOrganize_DryRun_PendingMoves(t *testing.T) {
	setHerdrSeams(t, inSession)
	w := newWorld(hws("w2", "misc", 1)).
		tab("w2", "t1", "term1", "/r/forge/a", "alpha").
		tab("w2", "t2", "term2", "/r/x/b", "b")
	r := runOrganize(t, organizeCfg(), w)
	if r.err != nil {
		t.Fatalf("err = %v", r.err)
	}
	listOnly(t, r)
	for _, want := range []string{
		`move`, `"alpha" [t1]`, `misc -> forge`, "/r/forge/a",
		"tab order will be rechecked after the moves",
		`1 tab matched no rule and go to "misc"`, `"b" [t2]`, "--explain shows why",
		"re-run with --apply to move them",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "organized") {
		t.Errorf("stdout says organized while a move is pending:\n%s", r.stdout)
	}
}

func TestHerdrOrganize_DryRun_BlockedMove(t *testing.T) {
	setHerdrSeams(t, inSession)
	w := newWorld(hws("w1", "forge", 1), hws("w2", "misc", 2)).
		tab("w1", "t1", "term1", "/r/x/a", "sole").
		tab("w2", "t2", "term2", "/r/x/b", "b")
	r := runOrganize(t, organizeCfg(), w)
	if r.err != nil {
		t.Fatalf("a predicted-blocked move must exit 0, got %v", r.err)
	}
	for _, want := range []string{
		`blocked`, `"sole" [t1]`, "forge -> misc", "it is the only tab in forge, and herdr will not empty a workspace",
		"fix: open another tab in forge, or move it by hand",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "organized") || strings.Contains(r.stdout, "re-run with --apply") {
		t.Errorf("a blocked-only plan has nothing to apply and is not organized:\n%s", r.stdout)
	}
}

func TestHerdrOrganize_DryRun_TabOrder(t *testing.T) {
	setHerdrSeams(t, inSession)
	w := newWorld(hws("w1", "forge", 1)).
		tab("w1", "t1", "term1", "/r/forge/b", "second").
		tab("w1", "t2", "term2", "/r/forge/a", "first")
	cfg := organizeCfg()
	cfg.Herdr.Organize.WorkspaceOrder = []string{"forge"}
	cfg.Herdr.Organize.Default = "forge"
	r := runOrganize(t, cfg, w)
	if r.err != nil {
		t.Fatalf("err = %v", r.err)
	}
	for _, want := range []string{`order    forge: "first" [t2] -> position 1`, "re-run with --apply to reorder them"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "rechecked") || strings.Contains(r.stdout, "organized") {
		t.Errorf("stdout:\n%s", r.stdout)
	}
}

func TestHerdrOrganize_DryRun_WorkspaceOrder(t *testing.T) {
	setHerdrSeams(t, inSession)
	w := newWorld(hws("w1", "misc", 1), hws("w2", "forge", 2)).
		tab("w1", "t1", "term1", "/r/x/a", "a").
		tab("w2", "t2", "term2", "/r/forge/b", "b")
	r := runOrganize(t, organizeCfg(), w)
	for _, want := range []string{"order    workspaces: forge, misc", "re-run with --apply to reorder them"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "organized") {
		t.Errorf("a pending workspace reorder is not organized:\n%s", r.stdout)
	}
}

func TestHerdrOrganize_Explain(t *testing.T) {
	setHerdrSeams(t, inSession)
	w := newWorld(hws("w1", "misc", 1)).
		tab("w1", "t1", "term1", "/r/forge/a", "alpha").
		tab("w1", "t2", "term2", "/r/x/b", "b")
	r := runOrganize(t, organizeCfg(), w, "--explain")
	if r.err != nil {
		t.Fatalf("err = %v", r.err)
	}
	for _, want := range []string{
		`"alpha" [t1]`, `rule 1 "/r/forge/* :: *" -> forge`, "in misc",
		`no rule matched -> misc (default)`, `key: "/r/x/b :: b"`,
		`rule 1 "/r/forge/* :: *" -> forge: 1 tab`,
		`rule 2 "/r/home/* :: *" -> home: 0 tabs`,
		`warning: workspace "home" is named by a rule but is not in workspace_order`,
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, r.stdout)
		}
	}
}

func TestHerdrOrganize_HostileTitleNeverReachesTheTerminalRaw(t *testing.T) {
	setHerdrSeams(t, inSession)
	w := newWorld(hws("w1", "misc", 1)).
		tab("w1", "t1", "term1", "/r/x/a", "evil\x1b]0;pwned\x07title")
	r := runOrganize(t, organizeCfg(), w, "--explain")
	if strings.ContainsAny(r.stdout, "\x1b\x07") {
		t.Errorf("stdout carries a raw control byte: %q", r.stdout)
	}
}

func TestHerdrOrganize_HostileWorkspaceLabelNeverReachesTheTerminalRaw(t *testing.T) {
	setHerdrSeams(t, inSession)
	// The label lands in the blocked reason, which the planner builds as text.
	hostile := "forge\x1b]52;c;AAAA\x07"
	w := newWorld(hws("w1", hostile, 1), hws("w2", "misc", 2)).
		tab("w1", "t1", "term1", "/r/x/a", "sole").
		tab("w2", "t2", "term2", "/r/x/b", "b")
	r := runOrganize(t, organizeCfg(), w)
	if !strings.Contains(r.stdout, "blocked") {
		t.Fatalf("no blocked line was printed; the test would pass vacuously:\n%s", r.stdout)
	}
	if strings.ContainsAny(r.stdout, "\x1b\x07") {
		t.Errorf("stdout carries a raw control byte: %q", r.stdout)
	}
}

func TestHerdrOrganize_LongTitleIsTruncated(t *testing.T) {
	setHerdrSeams(t, inSession)
	long := strings.Repeat("x", 80)
	w := newWorld(hws("w1", "misc", 1)).tab("w1", "t1", "term1", "/r/forge/a", long).tab("w1", "t2", "term2", "/r/x/b", "b")
	r := runOrganize(t, organizeCfg(), w)
	if strings.Contains(r.stdout, long) || !strings.Contains(r.stdout, "…") {
		t.Errorf("title not truncated to 30 columns:\n%s", r.stdout)
	}
}

func TestHerdrOrganize_JSON_Golden(t *testing.T) {
	setHerdrSeams(t, inSession)
	w := newWorld(hws("w2", "misc", 1)).
		tab("w2", "t1", "term1", "/r/forge/a", "a").
		tab("w2", "t2", "term2", "/r/x/b", "b")
	r := runOrganize(t, organizeCfg(), w, "--json")
	if r.err != nil {
		t.Fatalf("err = %v", r.err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "herdr_organize_dryrun.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got, exp any
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, r.stdout)
	}
	if err := json.Unmarshal(want, &exp); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, exp) {
		t.Errorf("--json differs from the golden file\n got: %s\nwant: %s", r.stdout, want)
	}
	if strings.TrimSpace(r.stderr) == "" {
		t.Error("under --json the human text must go to stderr; stderr is empty")
	}
	if strings.Contains(r.stdout, "re-run with") {
		t.Errorf("human text leaked into the JSON stdout:\n%s", r.stdout)
	}
}
