package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/exec"
	"github.com/cameronsjo/forgectl/internal/herdr"
)

// applyRun is one `organize --apply` against a stateful fake session.
type applyRun struct {
	organizeRun
	events []string // gate, fork, lock, then every herdr call as "herdr <args>"
}

func runApply(t *testing.T, seams herdrSeams, w *herdrWorld, extra ...string) applyRun {
	t.Helper()
	var events []string
	seams.events = &events
	setHerdrSeams(t, seams)
	inner := w.intercept
	w.intercept = func(args []string) (string, error, bool) {
		events = append(events, "herdr "+strings.Join(args, " "))
		if inner != nil {
			return inner(args)
		}
		return "", nil, false
	}
	r := runOrganize(t, organizeCfg(), w, append([]string{"--apply"}, extra...)...)
	return applyRun{organizeRun: r, events: events}
}

// mutating reports each state-changing herdr call in order.
func (a applyRun) mutating() []string {
	var out []string
	for _, e := range a.events {
		args := strings.TrimPrefix(e, "herdr ")
		if e == args {
			continue
		}
		switch {
		case strings.HasPrefix(args, "tab move"), strings.HasPrefix(args, "workspace move"),
			strings.HasPrefix(args, "tab focus"), strings.HasPrefix(args, "workspace focus"):
			out = append(out, args)
		}
	}
	return out
}

func phaseOf(call string) int {
	switch {
	case strings.HasPrefix(call, "tab move") && strings.Contains(call, "--index"):
		return 3
	case strings.HasPrefix(call, "tab move"):
		return 1
	case strings.HasPrefix(call, "workspace move"):
		return 2
	}
	return 4 // focus
}

// sessionWorld: misc holds two forge-bound tabs, one stays, and home holds one.
// The caller sits in t3.
func sessionWorld() *herdrWorld {
	w := newWorld(hws("w1", "misc", 1), hws("w2", "home", 2)).
		tab("w1", "t1", "term1", "/r/forge/b", "second").
		tab("w1", "t2", "term2", "/r/forge/a", "first").
		tab("w1", "t3", "term3", "/r/x/c", "other").
		tab("w2", "t4", "term4", "/r/home/z", "h")
	w.active = map[string]string{"w1": "t3", "w2": "t4"}
	w.focusedTab = "t3"
	return w
}

func TestApply_HappyPath(t *testing.T) {
	w := sessionWorld()
	a := runApply(t, inSession, w)
	if a.err != nil {
		t.Fatalf("err = %v\nstderr: %s", a.err, a.stderr)
	}

	if want := []string{"gate", "fork", "lock"}; !reflect.DeepEqual(a.events[:3], want) {
		t.Errorf("events start %v, want %v (gate, then fork check, then lock, before any herdr call)", a.events[:3], want)
	}

	last := 0
	for _, call := range a.mutating() {
		p := phaseOf(call)
		if p < last {
			t.Errorf("call %q (phase %d) came after a phase-%d call; want moves, workspace order, tab order, then focus", call, p, last)
		}
		last = p
	}

	forge := w.wsByLabel("forge")
	if forge == "" {
		t.Fatalf("no forge workspace was created; labels = %v", w.labels())
	}
	if got, want := w.order(forge), []string{"term2", "term1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("forge order = %v, want %v", got, want)
	}
	if got, want := w.labels(), []string{"forge", "misc", "home"}; !reflect.DeepEqual(got, want) {
		t.Errorf("workspace order = %v, want %v", got, want)
	}

	if n := len(w.focusLog); n < 2 || w.focusLog[n-1] != "tab:t3" {
		t.Errorf("focusLog = %v, want the caller's tab last", w.focusLog)
	}
	sawBackground := false
	for _, f := range w.focusLog[:len(w.focusLog)-1] {
		if f == "tab:t4" {
			sawBackground = true
		}
	}
	if !sawBackground {
		t.Errorf("focusLog = %v, want home's active tab restored before the caller's", w.focusLog)
	}
	for _, f := range w.focusLog {
		if strings.HasPrefix(f, "workspace:") {
			t.Errorf("focus went through a workspace call %q; restore is tab grain", f)
		}
	}

	for _, want := range []string{"applied: 2 moves", `focus restored to "other" [t3]`} {
		if !strings.Contains(a.stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, a.stdout)
		}
	}
	if strings.Contains(a.stdout, "re-run with --apply") {
		t.Errorf("apply must not suggest --apply:\n%s", a.stdout)
	}
}

func TestApply_DoesNotRestoreATabThatMovedAway(t *testing.T) {
	w := sessionWorld()
	w.active["w1"] = "t1" // misc's active tab is a forge-bound tab that will leave
	_ = runApply(t, inSession, w)
	for _, f := range w.focusLog {
		if f != "tab:t4" && f != "tab:t3" {
			t.Errorf("focusLog = %v: a tab that left its workspace must not be re-focused as that workspace's active tab", w.focusLog)
			break
		}
	}
}

func TestApply_ABackgroundWorkspacesActiveTabThatMovedAwayIsNotRefocused(t *testing.T) {
	// forge's active tab tA belongs in misc and leaves; the caller is in misc.
	// Restoring "forge's active tab" would focus tA's new tab, in misc, which
	// is not forge's active tab any more.
	w := newWorld(hws("w1", "misc", 1), hws("w2", "forge", 2)).
		tab("w1", "t9", "term9", "/r/x/z", "caller").
		tab("w2", "tA", "termA", "/r/x/a", "leaver").
		tab("w2", "tB", "termB", "/r/forge/b", "stayer")
	w.active = map[string]string{"w1": "t9", "w2": "tA"}
	w.focusedTab = "t9"
	a := runApply(t, inSession, w)
	if a.err != nil {
		t.Fatalf("err = %v", a.err)
	}
	if want := []string{"tab:t9"}; !reflect.DeepEqual(w.focusLog, want) {
		t.Errorf("focusLog = %v, want only the caller's tab: forge's recorded active tab left forge", w.focusLog)
	}
}

func TestApply_ResolvesEachTabByTerminalRightBeforeItsMove(t *testing.T) {
	w := sessionWorld()
	w.renumberAll = true // moving t1 out renumbers t2 and t3, so the planned ids go stale
	a := runApply(t, inSession, w)
	if a.err != nil {
		t.Fatalf("err = %v (a stale tab id reached herdr: the world reports it)", a.err)
	}
	forge := w.wsByLabel("forge")
	if got := w.order(forge); len(got) != 2 {
		t.Errorf("forge holds %v, want both forge tabs", got)
	}
}

func TestApply_ForkMissing_RefusesBeforeAnyChange(t *testing.T) {
	w := sessionWorld()
	a := runApply(t, herdrSeams{env: inSession.env, forkErr: herdr.ErrForkRequired}, w)
	if ExitCode(a.err) != 2 || a.err == nil {
		t.Fatalf("err = %v (exit %d), want exit 2", a.err, ExitCode(a.err))
	}
	want := `herdr has no "tab move", which organize --apply needs (it is in the cameronsjo/herdr fork, not upstream herdr); see docs/commands/herdr.md#requirements`
	if !strings.Contains(a.err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", a.err, want)
	}
	if len(a.mutating()) != 0 || len(a.runner.Calls) != 0 {
		t.Errorf("herdr was called despite the failed gate: %v", a.events)
	}
	for _, e := range a.events {
		if e == "lock" {
			t.Error("the lock was taken before the gate passed")
		}
	}
}

func TestApply_ALockDirectoryThatCannotBeMadeExitsTwoBeforeAnyChange(t *testing.T) {
	w := sessionWorld()
	var events []string
	setHerdrSeams(t, herdrSeams{env: inSession.env, events: &events})
	herdrLockPath = func() (string, error) { return "", errors.New("read-only config dir") }
	r := runOrganize(t, organizeCfg(), w, "--apply")
	if ExitCode(r.err) != 2 || r.err == nil || !strings.Contains(r.err.Error(), "read-only config dir") {
		t.Errorf("err = %v (exit %d), want exit 2: nothing was changed", r.err, ExitCode(r.err))
	}
	if len(r.runner.Calls) != 0 {
		t.Errorf("herdr was called: %v", r.runner.Calls)
	}
}

func TestApply_SessionGateFailsBeforeTheForkCheck(t *testing.T) {
	w := sessionWorld()
	a := runApply(t, herdrSeams{sessionErr: herdr.ErrNotInSession}, w)
	if ExitCode(a.err) != 2 {
		t.Fatalf("exit = %d, want 2 (err %v)", ExitCode(a.err), a.err)
	}
	for _, e := range a.events {
		if e == "fork" || e == "lock" {
			t.Errorf("event %q happened after a failed session gate", e)
		}
	}
}

// failMove makes the nth (1-based) cross-workspace tab move fail as herdr does:
// exit 1 with a JSON error on stderr.
func failMove(nth int, seen *int, then func(args []string) (string, error, bool)) func([]string) (string, error, bool) {
	return func(args []string) (string, error, bool) {
		if len(args) >= 4 && args[0] == "tab" && args[1] == "move" && (args[3] == "--workspace" || args[3] == "--new-workspace") {
			*seen++
			if *seen == nth {
				return "", &exec.CommandError{Name: "herdr", ExitCode: 1, Stderr: `{"error":{"code":"boom","message":"nope"}}`}, true
			}
		}
		if then != nil {
			return then(args)
		}
		return "", nil, false
	}
}

func threeMoveWorld() *herdrWorld {
	w := newWorld(hws("w1", "misc", 1), hws("w2", "forge", 2)).
		tab("w1", "t1", "term1", "/r/forge/a", "one").
		tab("w1", "t2", "term2", "/r/forge/b", "two").
		tab("w1", "t3", "term3", "/r/forge/c", "three").
		tab("w1", "t9", "term9", "/r/x/z", "stay").
		tab("w2", "t8", "term8", "/r/forge/k", "resident")
	w.active = map[string]string{"w1": "t9", "w2": "t8"}
	w.focusedTab = "t9"
	return w
}

func TestApply_HerdrErrorOnTheSecondOfThreeMoves(t *testing.T) {
	w := threeMoveWorld()
	seen := 0
	w.intercept = failMove(2, &seen, nil)
	a := runApply(t, inSession, w)
	if a.err == nil || ExitCode(a.err) != 1 {
		t.Fatalf("err = %v (exit %d), want exit 1", a.err, ExitCode(a.err))
	}
	lines := strings.Split(strings.TrimSpace(a.err.Error()), "\n")
	if len(lines) != 4 {
		t.Fatalf("failure summary has %d lines, want 4:\n%s", len(lines), a.err)
	}
	if !strings.Contains(lines[0], "boom") || !strings.Contains(lines[0], `"two" [t2]`) {
		t.Errorf("line 1 = %q, want the failing move and why", lines[0])
	}
	if !strings.HasPrefix(lines[1], "applied: 1 of 3 moves; not run:") || !strings.Contains(lines[1], `"two"`) || !strings.Contains(lines[1], `"three"`) {
		t.Errorf("line 2 = %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], `focus restored to "stay" [t9]`) {
		t.Errorf("line 3 = %q", lines[2])
	}
	if !strings.Contains(lines[3], "the plan is recomputed on every run") || !strings.Contains(lines[3], "forgectl herdr organize --apply") {
		t.Errorf("line 4 = %q", lines[3])
	}
	if n := len(w.focusLog); n == 0 || w.focusLog[n-1] != "tab:t9" {
		t.Errorf("focusLog = %v, want focus restored to the caller's tab after the failure", w.focusLog)
	}
	if got := w.order("w2"); len(got) != 2 {
		t.Errorf("forge = %v, want the resident plus the one tab moved before the failure", got)
	}
}

func TestApply_UnexpectedDeclinedExitsOneAndRestoresFocus(t *testing.T) {
	w := threeMoveWorld()
	w.intercept = func(args []string) (string, error, bool) {
		if len(args) >= 3 && args[0] == "tab" && args[1] == "move" && args[2] == "t1" {
			return `{"id":"x","result":{"move_result":{"changed":false,"reason":"something_new"}}}`, nil, true
		}
		return "", nil, false
	}
	a := runApply(t, inSession, w)
	if a.err == nil || ExitCode(a.err) != 1 {
		t.Fatalf("err = %v (exit %d), want exit 1", a.err, ExitCode(a.err))
	}
	if !strings.Contains(a.err.Error(), "something_new") {
		t.Errorf("error = %q, want herdr's reason", a.err)
	}
	if n := len(w.focusLog); n == 0 || w.focusLog[n-1] != "tab:t9" {
		t.Errorf("focusLog = %v, want focus restored after the decline", w.focusLog)
	}
}

func TestApply_ADeclinedMoveIntoAnExistingWorkspaceDoesNotAbandonTheRest(t *testing.T) {
	w := threeMoveWorld()
	w.intercept = func(args []string) (string, error, bool) {
		if len(args) >= 3 && args[0] == "tab" && args[1] == "move" && args[2] == "t1" && len(args) > 3 && args[3] == "--workspace" {
			return `{"id":"x","result":{"move_result":{"changed":false,"reason":"last_tab_in_workspace"}}}`, nil, true
		}
		return "", nil, false
	}
	a := runApply(t, inSession, w)
	if a.err == nil || ExitCode(a.err) != 1 || !strings.Contains(a.err.Error(), "last_tab_in_workspace") {
		t.Fatalf("err = %v (exit %d), want exit 1 reporting the decline", a.err, ExitCode(a.err))
	}
	if got := w.order("w2"); len(got) != 3 { // resident + t2 + t3
		t.Errorf("forge holds %v, want the resident and the two tabs that could move", got)
	}
	if n := len(w.focusLog); n == 0 || w.focusLog[n-1] != "tab:t9" {
		t.Errorf("focusLog = %v, want focus restored", w.focusLog)
	}
}

func TestApply_AMoveThatAPeerAlreadyMadeIsNotCountedAsApplied(t *testing.T) {
	w := threeMoveWorld()
	done := false
	w.intercept = func(args []string) (string, error, bool) {
		if !done && len(args) >= 4 && args[0] == "tab" && args[1] == "move" && args[3] == "--workspace" {
			done = true
			// While the first move is issued, a peer puts t3 into forge itself.
			w.moveTab(t, []string{"tab", "move", "t3", "--workspace", "w2"})
		}
		return "", nil, false
	}
	a := runApply(t, inSession, w)
	if a.err != nil {
		t.Fatalf("err = %v", a.err)
	}
	if !strings.Contains(a.stdout, "applied: 2 moves") || !strings.Contains(a.stdout, "in place") {
		t.Errorf("stdout = %q, want 2 applied moves and the peer's move reported as already in place", a.stdout)
	}
}

func TestApply_WorkspaceReordersAreReported(t *testing.T) {
	w := sessionWorld()
	a := runApply(t, inSession, w)
	if !strings.Contains(a.stdout, `ordered  workspace "forge" -> position 1`) || !strings.Contains(a.stdout, "1 reorder") && !strings.Contains(a.stdout, "reorders") {
		t.Errorf("stdout = %q, want the workspace move listed and counted", a.stdout)
	}
}

func TestApply_CallerTabGoneLeavesTheUIInItsWorkspace(t *testing.T) {
	w := sessionWorld()
	done := false
	w.intercept = func(args []string) (string, error, bool) {
		if !done && len(args) >= 2 && args[0] == "tab" && args[1] == "move" {
			done = true
			// The caller's tab is closed mid-run.
			w.tabs["w1"] = w.tabs["w1"][:2]
			w.panes = append(w.panes[:2], w.panes[3:]...)
			w.focusedTab = ""
		}
		return "", nil, false
	}
	_ = runApply(t, inSession, w)
	if n := len(w.focusLog); n == 0 || w.focusLog[n-1] != "workspace:w1" {
		t.Errorf("focusLog = %v, want the caller's workspace focused last", w.focusLog)
	}
}

func TestApply_IndexMovesThatNeverTakeEffectFailInsteadOfLooping(t *testing.T) {
	w := newWorld(hws("w1", "forge", 1)).
		tab("w1", "t1", "term1", "/r/forge/b", "b").
		tab("w1", "t2", "term2", "/r/forge/a", "a")
	w.active = map[string]string{"w1": "t1"}
	w.focusedTab = "t1"
	// herdr acknowledges every index move and changes nothing.
	w.intercept = func(args []string) (string, error, bool) {
		if len(args) >= 4 && args[0] == "tab" && args[1] == "move" && args[3] == "--index" {
			return reply(t, map[string]any{"tabs": w.tabs["w1"]}), nil, true
		}
		return "", nil, false
	}
	cfg := organizeCfg()
	cfg.Herdr.Organize.WorkspaceOrder = []string{"forge"}
	cfg.Herdr.Organize.Default = "forge"
	setHerdrSeams(t, inSession)
	r := runOrganize(t, cfg, w, "--apply")
	if r.err == nil || !strings.Contains(r.err.Error(), "did not settle") {
		t.Fatalf("err = %v, want the tab order reported as not settling", r.err)
	}
	if n := len(w.focusLog); n == 0 || w.focusLog[n-1] != "tab:t1" {
		t.Errorf("focusLog = %v, want focus restored after the failure", w.focusLog)
	}
}

func TestApply_DeclinedCreateMakesTheNextTabTheCreator(t *testing.T) {
	w := sessionWorld() // forge does not exist; t1 and t2 both belong there
	declined := false
	w.intercept = func(args []string) (string, error, bool) {
		if !declined && len(args) >= 4 && args[0] == "tab" && args[1] == "move" && args[3] == "--new-workspace" {
			declined = true
			return `{"id":"x","result":{"move_result":{"changed":false,"reason":"create_refused"}}}`, nil, true
		}
		return "", nil, false
	}
	a := runApply(t, inSession, w)
	creates, joins := 0, 0
	for _, c := range a.mutating() {
		if strings.Contains(c, "--new-workspace") {
			creates++
		}
		if strings.HasPrefix(c, "tab move") && strings.Contains(c, "--workspace") {
			joins++
		}
	}
	if creates != 2 || joins != 0 {
		t.Errorf("creates=%d joins=%d, want 2 creates and no join: the next tab must try to create when the first create was declined\n%v", creates, joins, a.mutating())
	}
	if a.err == nil || !strings.Contains(a.err.Error(), "create_refused") {
		t.Errorf("err = %v, want the declined create reported", a.err)
	}
	if forge := w.wsByLabel("forge"); forge == "" || len(w.order(forge)) != 1 {
		t.Errorf("forge = %q holding %v, want it created holding the second tab", forge, w.order(forge))
	}
}

func TestApply_FocusRestoreFailureIsReportedWithoutMaskingTheRunError(t *testing.T) {
	w := threeMoveWorld()
	seen := 0
	w.intercept = failMove(2, &seen, func(args []string) (string, error, bool) {
		if len(args) >= 2 && args[0] == "tab" && args[1] == "focus" {
			return "", &exec.CommandError{Name: "herdr", ExitCode: 1, Stderr: `{"error":{"code":"focus_broke","message":"no"}}`}, true
		}
		return "", nil, false
	})
	a := runApply(t, inSession, w)
	if a.err == nil {
		t.Fatal("err = nil")
	}
	for _, want := range []string{"boom", "could not restore focus", "focus_broke"} {
		if !strings.Contains(a.err.Error(), want) {
			t.Errorf("error missing %q (the run error and the focus failure must both show):\n%s", want, a.err)
		}
	}
}

func TestApply_TabOrderMovesOnlyWhatDiffers(t *testing.T) {
	w := newWorld(hws("w1", "forge", 1)).
		tab("w1", "t1", "term1", "/r/forge/b", "b").
		tab("w1", "t2", "term2", "/r/forge/a", "a").
		tab("w1", "t3", "term3", "/r/forge/c", "c")
	w.active = map[string]string{"w1": "t1"}
	w.focusedTab = "t1"
	cfg := organizeCfg()
	cfg.Herdr.Organize.WorkspaceOrder = []string{"forge"}
	cfg.Herdr.Organize.Default = "forge"
	setHerdrSeams(t, inSession)
	r := runOrganize(t, cfg, w, "--apply")
	if r.err != nil {
		t.Fatalf("err = %v", r.err)
	}
	if got, want := w.order("w1"), []string{"term2", "term1", "term3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	idx := 0
	for _, c := range r.runner.Calls {
		if len(c.Args) >= 2 && c.Args[0] == "tab" && c.Args[1] == "move" {
			idx++
			if c.Args[2] != "t2" {
				t.Errorf("index move of %q, want the tab resolved from terminal term2 (t2)", c.Args[2])
			}
		}
	}
	if idx != 1 {
		t.Errorf("%d index moves, want exactly 1 (only the tab out of place)", idx)
	}
}

func TestApply_TabOrderSettlesInFewMovesFromAReversal(t *testing.T) {
	w := newWorld(hws("w1", "forge", 1)).
		tab("w1", "t1", "term1", "/r/forge/d", "d").
		tab("w1", "t2", "term2", "/r/forge/c", "c").
		tab("w1", "t3", "term3", "/r/forge/b", "b").
		tab("w1", "t4", "term4", "/r/forge/a", "a")
	w.active = map[string]string{"w1": "t1"}
	w.focusedTab = "t1"
	cfg := organizeCfg()
	cfg.Herdr.Organize.WorkspaceOrder = []string{"forge"}
	cfg.Herdr.Organize.Default = "forge"
	setHerdrSeams(t, inSession)
	r := runOrganize(t, cfg, w, "--apply")
	if r.err != nil {
		t.Fatalf("err = %v", r.err)
	}
	if got, want := w.order("w1"), []string{"term4", "term3", "term2", "term1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	moves := 0
	for _, c := range r.runner.Calls {
		if len(c.Args) >= 2 && c.Args[0] == "tab" && c.Args[1] == "move" {
			moves++
		}
	}
	if moves != 3 {
		t.Errorf("%d index moves to reverse four tabs, want 3", moves)
	}
}

func TestApply_RunErrorTextIsTerminalSafe(t *testing.T) {
	w := threeMoveWorld()
	w.intercept = func(args []string) (string, error, bool) {
		if len(args) >= 4 && args[0] == "tab" && args[1] == "move" && args[3] == "--workspace" {
			return "", &exec.CommandError{Name: "herdr", ExitCode: -1, Stderr: "\x1b]0;pwned\x07 killed"}, true
		}
		return "", nil, false
	}
	a := runApply(t, inSession, w)
	if a.err == nil {
		t.Fatal("err = nil")
	}
	if strings.ContainsAny(a.err.Error(), "\x1b\x07") {
		t.Errorf("the failure summary carries a raw control byte: %q", a.err.Error())
	}
}

func TestApply_NothingToDoTouchesNothing(t *testing.T) {
	w := newWorld(hws("w1", "forge", 1), hws("w2", "misc", 2)).
		tab("w1", "t1", "term1", "/r/forge/a", "a").
		tab("w2", "t2", "term2", "/r/x/b", "b")
	w.focusedTab = "t2"
	a := runApply(t, inSession, w)
	if a.err != nil {
		t.Fatalf("err = %v", a.err)
	}
	if got := a.mutating(); len(got) != 0 {
		t.Errorf("mutating calls on an organized session: %v", got)
	}
	if !strings.Contains(a.stdout, "organized: 2 tabs in 2 workspaces; nothing to do") {
		t.Errorf("stdout = %q", a.stdout)
	}
}

func TestApply_LockWaitIsAnnounced(t *testing.T) {
	w := sessionWorld()
	a := runApply(t, herdrSeams{env: inSession.env, lockContended: true}, w)
	if a.err != nil {
		t.Fatalf("err = %v", a.err)
	}
	if !strings.Contains(a.stderr, "waiting for another forgectl herdr organize (Ctrl-C to cancel)") {
		t.Errorf("stderr = %q, want the wait notice", a.stderr)
	}
}

func TestApply_UncontendedLockIsSilent(t *testing.T) {
	w := sessionWorld()
	a := runApply(t, inSession, w)
	if strings.Contains(a.stderr, "waiting for another") {
		t.Errorf("stderr announces a wait with no contention: %q", a.stderr)
	}
}

func TestApply_JSONResult(t *testing.T) {
	w := sessionWorld()
	a := runApply(t, inSession, w, "--json")
	if a.err != nil {
		t.Fatalf("err = %v", a.err)
	}
	var doc struct {
		Result struct {
			Applied []map[string]any `json:"applied"`
			NotRun  []map[string]any `json:"not_run"`
			Error   string           `json:"error"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(a.stdout), &doc); err != nil {
		t.Fatalf("stdout is not one JSON object: %v\n%s", err, a.stdout)
	}
	if len(doc.Result.Applied) != 2 || len(doc.Result.NotRun) != 0 || doc.Result.Error != "" {
		t.Errorf("result = %+v, want two applied moves and no error", doc.Result)
	}
}

func TestApply_JSONResultOnFailure(t *testing.T) {
	w := threeMoveWorld()
	seen := 0
	w.intercept = failMove(2, &seen, nil)
	a := runApply(t, inSession, w, "--json")
	if ExitCode(a.err) != 1 {
		t.Fatalf("exit = %d, want 1", ExitCode(a.err))
	}
	// The object's result.error carries the failure, so the exit is silent
	// (forgectl#862): fang renders nothing on top of stdout.
	if _, ok := a.err.(*silentCodedError); !ok {
		t.Errorf("err = %T %v, want a silentCodedError under --json", a.err, a.err)
	}
	var doc struct {
		Result struct {
			Applied []map[string]any `json:"applied"`
			NotRun  []map[string]any `json:"not_run"`
			Error   string           `json:"error"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(a.stdout), &doc); err != nil {
		t.Fatalf("stdout must carry the JSON object even on exit 1: %v\n%s", err, a.stdout)
	}
	if len(doc.Result.Applied) != 1 || len(doc.Result.NotRun) != 2 || !strings.Contains(doc.Result.Error, "boom") {
		t.Errorf("result = %+v, want 1 applied, 2 not run, and the error", doc.Result)
	}
}

func TestOrganizeLockPath_LivesBesideTheConfigNotOnIt(t *testing.T) {
	base := t.TempDir()
	t.Setenv("HOME", base)
	t.Setenv("XDG_CONFIG_HOME", base)
	got, err := organizeLockPath()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "herdr-organize" || filepath.Base(filepath.Dir(got)) != "forgectl" {
		t.Errorf("lock path = %q, want <config dir>/herdr-organize", got)
	}
	if strings.HasSuffix(got, "config.toml") {
		t.Errorf("lock path %q would contend with config writers", got)
	}
	if info, err := os.Stat(filepath.Dir(got)); err != nil || !info.IsDir() {
		t.Errorf("the lock directory must exist: %v", err)
	}
}

// countBetween counts the herdr calls with prefix among events after the first
// event equal to from and before the next "herdr tab move" call.
func countBetween(events []string, from, prefix string) (int, bool) {
	start := -1
	for i, e := range events {
		if e == from {
			start = i
			break
		}
	}
	if start < 0 {
		return 0, false
	}
	n := 0
	for _, e := range events[start+1:] {
		if strings.HasPrefix(e, "herdr tab move") {
			break
		}
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n, true
}

func TestApply_MovesReReadOnlyAfterACallThatChangedTheSession(t *testing.T) {
	w := threeMoveWorld()
	w.intercept = func(args []string) (string, error, bool) {
		if len(args) >= 4 && args[0] == "tab" && args[1] == "move" && args[2] == "t1" && args[3] == "--workspace" {
			return `{"id":"x","result":{"move_result":{"changed":false,"reason":"last_tab_in_workspace"}}}`, nil, true
		}
		return "", nil, false
	}
	a := runApply(t, inSession, w)
	if a.err == nil || !strings.Contains(a.err.Error(), "last_tab_in_workspace") {
		t.Fatalf("err = %v, want the decline reported", a.err)
	}
	// The declined move changed nothing, so the next move reuses the last read.
	for _, list := range []string{"herdr pane list", "herdr workspace list"} {
		n, ok := countBetween(a.events, "herdr tab move t1 --workspace w2", list)
		if !ok || n != 0 {
			t.Errorf("%d %q calls between the declined move and the next (found=%v), want 0\n%v", n, list, ok, a.events)
		}
		// t2's move was applied and renumbers ids, so t3's move re-reads first.
		n, ok = countBetween(a.events, "herdr tab move t2 --workspace w2", list)
		if !ok || n != 1 {
			t.Errorf("%d %q calls between an applied move and the next (found=%v), want 1\n%v", n, list, ok, a.events)
		}
	}
}

func TestApply_TabOrderReadsTheSessionOncePerWorkspace(t *testing.T) {
	w := newWorld(hws("w1", "forge", 1)).
		tab("w1", "t1", "term1", "/r/forge/d", "d").
		tab("w1", "t2", "term2", "/r/forge/c", "c").
		tab("w1", "t3", "term3", "/r/forge/b", "b").
		tab("w1", "t4", "term4", "/r/forge/a", "a")
	w.active = map[string]string{"w1": "t1"}
	w.focusedTab = "t1"
	var calls []string
	w.intercept = func(args []string) (string, error, bool) {
		calls = append(calls, strings.Join(args, " "))
		return "", nil, false
	}
	cfg := organizeCfg()
	cfg.Herdr.Organize.WorkspaceOrder = []string{"forge"}
	cfg.Herdr.Organize.Default = "forge"
	setHerdrSeams(t, inSession)
	r := runOrganize(t, cfg, w, "--apply")
	if r.err != nil {
		t.Fatalf("err = %v", r.err)
	}
	if got, want := w.order("w1"), []string{"term4", "term3", "term2", "term1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	// Three index moves; each reply carries the new tab list, so the stage lists
	// tabs and panes once, not once per move.
	count := func(prefix string) int {
		n := 0
		for _, c := range calls {
			if strings.HasPrefix(c, prefix) {
				n++
			}
		}
		return n
	}
	// tab list: the snapshot, then the tab-order stage.
	if n := count("tab list"); n != 2 {
		t.Errorf("%d tab list calls, want 2 (snapshot + one for the tab-order stage)\n%v", n, calls)
	}
	// pane list: the snapshot, the tab-order stage, and the focus restore.
	if n := count("pane list"); n != 3 {
		t.Errorf("%d pane list calls, want 3 (snapshot, tab-order stage, focus restore)\n%v", n, calls)
	}
}

// TestApply_DuplicateLabelWorkspaceOrderMatchesTheDryRun: the dry run must
// report the workspace reorder --apply performs when two workspaces share a
// label, and a second run must find nothing left to reorder (#732).
func TestApply_DuplicateLabelWorkspaceOrderMatchesTheDryRun(t *testing.T) {
	w := newWorld(hws("w1", "forge", 1), hws("w2", "forge", 2), hws("w3", "misc", 3)).
		tab("w1", "t1", "term1", "/r/forge/a", "a").
		tab("w2", "t2", "term2", "/r/forge/b", "b").
		tab("w3", "t3", "term3", "/r/x/c", "c").
		tab("w3", "t4", "term4", "/r/x/d", "d").
		tab("w3", "t5", "term5", "/r/forge/e", "e")
	w.active = map[string]string{"w1": "t1", "w2": "t2", "w3": "t3"}
	w.focusedTab = "t3"
	setHerdrSeams(t, inSession)

	dry := runOrganize(t, organizeCfg(), w)
	if dry.err != nil {
		t.Fatalf("dry run err = %v", dry.err)
	}
	const wantLine = "order    workspaces: forge, misc, forge"
	if !strings.Contains(dry.stdout, wantLine) {
		t.Fatalf("dry run missing %q:\n%s", wantLine, dry.stdout)
	}

	// The duplicate's lone tab is blocked; the rest applies cleanly.
	a := runOrganize(t, organizeCfg(), w, "--apply")
	if a.err != nil {
		t.Fatalf("apply err = %v\n%s", a.err, a.stdout)
	}
	if !strings.Contains(a.stdout, `ordered  workspace "misc" -> position 2`) {
		t.Errorf("apply did not report the workspace reorder:\n%s", a.stdout)
	}
	if got, want := w.labels(), []string{"forge", "misc", "forge"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels after apply = %v, want %v (what the dry run reported)", got, want)
	}

	again := runOrganize(t, organizeCfg(), w)
	if strings.Contains(again.stdout, "order    workspaces") {
		t.Errorf("a second dry run still reports a workspace reorder; apply and the dry run disagree:\n%s", again.stdout)
	}
}
