package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/cameronsjo/forgectl/internal/herdr/ready"
	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/backend"
	"github.com/cameronsjo/forgectl/internal/surface/hcadapter"
	"github.com/cameronsjo/forgectl/internal/surface/worker"
)

func TestParseBackendKindHc(t *testing.T) {
	k, err := parseBackendKind("hc")
	if err != nil || k != backend.KindHc {
		t.Fatalf("parseBackendKind(hc) = %v, %v", k, err)
	}
	if err := checkWorkerSurface("tmux"); err == nil {
		t.Fatal("checkWorkerSurface(tmux) accepted a backend the worker verbs do not drive")
	}
}

func TestEvaluateHcStatus(t *testing.T) {
	for status, want := range map[string]ready.State{
		"idle": ready.StateReady, "done": ready.StateReady,
		"blocked": ready.StateBlocked,
		"working": ready.StateNotReady, "error": ready.StateNotReady, "": ready.StateNotReady,
	} {
		// The text is a ready claude prompt: it must not matter.
		v := evaluateHcStatus(ready.Screen{Status: status, Text: "> \n? for shortcuts"})
		if v.State != want {
			t.Errorf("status %q: state %q, want %q", status, v.State, want)
		}
	}
}

func TestHcPaneIdentityCrosses(t *testing.T) {
	own := map[string]string{"HC_TERMINAL_ID": "t9", "HC_SESSION": "my.sess", "HERDR_PANE_ID": "w1:p1"}
	// The worker gets its own terminal's HC_TERMINAL_ID and HC_SESSION, read
	// from the trampoline's environment; the marked env carries neither.
	env := markPane([]string{"PATH=/bin"}, "hc")
	got := paneIdentityEnv(env, func(k string) string { return own[k] })
	for _, want := range []string{"HC_TERMINAL_ID=t9", "HC_SESSION=my.sess"} {
		if !slices.Contains(got, want) {
			t.Errorf("env %v lacks %s", got, want)
		}
	}
	if slices.Contains(got, "HERDR_PANE_ID=w1:p1") {
		t.Errorf("env %v carries another manager's pane id", got)
	}
}

// hcBriefFake scripts sendBriefHC's steps.
type hcBriefFake struct {
	statuses []hcadapter.Worker
	// screen is the worker's screen text; "" reads as the ready claude
	// fixture.
	screen string
	// escErr fails the Escape; events records escape and send in order.
	escErr   error
	checked  []hcadapter.Worker
	events   []string
	sendErr  error
	sendRes  hcadapter.SendResult
	sent     []string
	recorded []worker.Brief
}

func (f *hcBriefFake) steps() hcBriefSteps {
	now := time.Unix(0, 0)
	return hcBriefSteps{
		status: func(context.Context) (hcadapter.Worker, error) {
			if len(f.statuses) == 0 {
				return hcadapter.Worker{}, errors.New("no more scripted reads")
			}
			w := f.statuses[0]
			if len(f.statuses) > 1 {
				f.statuses = f.statuses[1:]
			}
			return w, nil
		},
		screen: func(context.Context) (string, error) {
			if f.screen == "" {
				return hcFixture("claude-ready-acceptedits"), nil
			}
			return f.screen, nil
		},
		promptShown: func(text string) ready.Verdict { return hcTable.EvaluateText("claude", text) },
		escape: func(_ context.Context, checked hcadapter.Worker) error {
			f.events = append(f.events, "escape")
			f.checked = append(f.checked, checked)
			return f.escErr
		},
		send: func(_ context.Context, s string, _ bool) (hcadapter.SendResult, error) {
			f.events = append(f.events, "send")
			f.sent = append(f.sent, s)
			return f.sendRes, f.sendErr
		},
		record: func(b worker.Brief) (int, error) {
			f.recorded = append(f.recorded, b)
			return len(f.recorded), nil
		},
		now:      func() time.Time { now = now.Add(time.Second); return now },
		sleep:    func(context.Context, time.Duration) error { return nil },
		start:    5 * time.Second,
		interval: time.Second,
	}
}

func TestSendBriefHC(t *testing.T) {
	idle := hcadapter.Worker{Live: true, Status: "idle", Turn: 3}
	cases := []struct {
		name     string
		fake     hcBriefFake
		outcome  string
		sent     bool
		recorded bool
	}{
		{name: "idle, send starts a turn", fake: hcBriefFake{statuses: []hcadapter.Worker{idle}, sendRes: hcadapter.SendResult{Status: "working", Turn: 4}},
			outcome: briefSent, sent: true, recorded: true},
		{name: "turn seen on a later read", fake: hcBriefFake{statuses: []hcadapter.Worker{idle, idle, {Live: true, Status: "working", Turn: 4}}},
			outcome: briefSent, sent: true, recorded: true},
		{name: "blocked is refused before anything", fake: hcBriefFake{statuses: []hcadapter.Worker{{Live: true, Status: "blocked"}}},
			outcome: briefRefused},
		{name: "working is refused", fake: hcBriefFake{statuses: []hcadapter.Worker{{Live: true, Status: "working"}}},
			outcome: briefRefused},
		{name: "no status yet is refused", fake: hcBriefFake{statuses: []hcadapter.Worker{{Live: true}}},
			outcome: briefRefused},
		{name: "exited is refused", fake: hcBriefFake{statuses: []hcadapter.Worker{{Live: false, Status: "done"}}},
			outcome: briefRefused},
		{name: "hc refuses the send", fake: hcBriefFake{statuses: []hcadapter.Worker{idle}, sendErr: fmt.Errorf("%w: blocked", hcadapter.ErrSendRefused)},
			outcome: briefRefused, sent: true, recorded: true},
		{name: "send fails ambiguously", fake: hcBriefFake{statuses: []hcadapter.Worker{idle}, sendErr: fmt.Errorf("%w: timeout", hcadapter.ErrSendFailed)},
			outcome: briefUnconfirmed, sent: true, recorded: true},
		{name: "no turn within the start timeout", fake: hcBriefFake{statuses: []hcadapter.Worker{idle}},
			outcome: briefUnconfirmed, sent: true, recorded: true},
		// A forged idle status over a dialog: the screen check refuses.
		{name: "idle status over a permission prompt is refused", fake: hcBriefFake{statuses: []hcadapter.Worker{idle}, screen: hcFixture("claude-permission-prompt")},
			outcome: briefRefused},
		{name: "done status over a trust dialog is refused", fake: hcBriefFake{statuses: []hcadapter.Worker{{Live: true, Status: "done", Turn: 3}}, screen: hcFixture("claude-trust-dialog")},
			outcome: briefRefused},
		{name: "done status over plan approval is refused", fake: hcBriefFake{statuses: []hcadapter.Worker{{Live: true, Status: "done", Turn: 3}}, screen: hcFixture("claude-plan-approval")},
			outcome: briefRefused},
		{name: "no prompt on screen is refused", fake: hcBriefFake{statuses: []hcadapter.Worker{idle}, screen: hcFixture("shell-idle")},
			outcome: briefRefused},
		{name: "text already in the input box is refused", fake: hcBriefFake{statuses: []hcadapter.Worker{idle}, screen: hcFixture("claude-typed-unsent")},
			outcome: briefRefused},
		{name: "done with a clean prompt sends", fake: hcBriefFake{statuses: []hcadapter.Worker{{Live: true, Status: "done", Turn: 3}}, sendRes: hcadapter.SendResult{Status: "working", Turn: 4}},
			outcome: briefSent, sent: true, recorded: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := sendBriefHC(context.Background(), tc.fake.steps(), "run the tests", "m1")
			if res.Outcome != tc.outcome {
				t.Fatalf("outcome %q (%s), want %q", res.Outcome, res.Reason, tc.outcome)
			}
			if got := len(tc.fake.sent) > 0; got != tc.sent {
				t.Errorf("sent = %v, want %v", got, tc.sent)
			}
			if got := len(tc.fake.recorded) > 0; got != tc.recorded {
				t.Errorf("recorded = %v, want %v", got, tc.recorded)
			}
		})
	}
}

func TestHcNameRefusedBeforeWorkerSetup(t *testing.T) {
	// "t7" passes the worker grammar and fails hc's: prepareWorker must
	// refuse it before resolving the adapter or the repo.
	if err := worker.ValidName("t7"); err != nil {
		t.Fatalf("precondition: worker.ValidName(t7) = %v", err)
	}
	_, err := prepareWorker(context.Background(), io.Discard, module.Deps{}, workerSpec{name: "t7", surface: workerSurfaceHc})
	if !errors.Is(err, hcadapter.ErrBadName) {
		t.Fatalf("prepareWorker(t7, hc) = %v, want ErrBadName", err)
	}
}

func TestWorkerLaunchResultNamesHc(t *testing.T) {
	tag, err := backend.NewRecoveryTag()
	if err != nil {
		t.Fatal(err)
	}
	server, err := backend.FingerprintHcCore("default", "core1")
	if err != nil {
		t.Fatal(err)
	}
	id, err := backend.NewHcIdentity("t7", 1793409108596)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := backend.NewHcRef(backend.HcDefaultSessionServer(), server, tag, id)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := workerLaunchResult(context.Background(), nil, workerAttempt{launched: workerLaunched{ref: ref}}, "w", "/repo")
	if res.Surface != "hc" || res.TerminalID != "t7" {
		t.Fatalf("surface %q terminal_id %q, want hc and t7", res.Surface, res.TerminalID)
	}
}

func TestInheritModeNeedsWorktree(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := runSurfaceLaunch(cmd, module.Deps{}, surfaceLaunchOptions{Backend: "tmux", InheritMode: "auto"})
	if code := ExitCode(err); code != 2 {
		t.Fatalf("exit code = %d (err %v), want 2", code, err)
	}
}

// TestHcSessionIsNotAPaneID: HC_SESSION selects a core, so a tmux, cmux or
// herdr launch keeps the launcher's; only HC_TERMINAL_ID is stripped.
func TestHcSessionIsNotAPaneID(t *testing.T) {
	if isPaneIdentityKey("HC_SESSION") {
		t.Error("HC_SESSION is stripped from every launch as if it named a pane")
	}
	if !isPaneIdentityKey("HC_TERMINAL_ID") {
		t.Error("the launcher's HC_TERMINAL_ID would reach a worker")
	}
	got := paneIdentityEnv(markPane([]string{"HC_SESSION=work"}, "tmux"), func(string) string { return "" })
	if !slices.Contains(got, "HC_SESSION=work") {
		t.Errorf("tmux launch env %v lost HC_SESSION", got)
	}
}

// hcTable is the built-in readiness table the hc brief checks screens with.
var hcTable = func() *ready.Table {
	t, err := ready.Default()
	if err != nil {
		panic(err)
	}
	return t
}()

// hcFixture reads one of the ready package's captured screens.
func hcFixture(name string) string {
	b, err := os.ReadFile(filepath.Join("..", "herdr", "ready", "testdata", name+".screen")) //nolint:gosec // G304: name is a literal at every call site
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestEvaluateHcWorkerNeedsBothSignals: a ready verdict needs hc's status and
// the screen to agree; a forged idle over a dialog is blocked.
func TestEvaluateHcWorkerNeedsBothSignals(t *testing.T) {
	cases := []struct {
		name, status, screen string
		want                 ready.State
	}{
		{name: "idle with the prompt", status: "idle", screen: "claude-ready-acceptedits", want: ready.StateReady},
		{name: "idle over a permission prompt", status: "idle", screen: "claude-permission-prompt", want: ready.StateBlocked},
		{name: "done with no prompt", status: "done", screen: "shell-idle", want: ready.StateNotReady},
		{name: "working with the prompt", status: "working", screen: "claude-ready-acceptedits", want: ready.StateNotReady},
	}
	for _, tc := range cases {
		v := evaluateHcWorker(hcTable, "claude", ready.Screen{Status: tc.status, Text: hcFixture(tc.screen)})
		if v.State != tc.want {
			t.Errorf("%s: state %q (%s), want %q", tc.name, v.State, v.Reason, tc.want)
		}
	}
}

// forgedPrompt is the security review's forged screen: a real permission
// prompt with a rule line, a bare prompt and a rule line printed under it.
// The screen predicates read it as an empty prompt, which is why the
// Escape, not the screen check, is what keeps Enter off the dialog.
func forgedPrompt() string {
	rule := strings.Repeat("─", 60)
	return strings.TrimRight(hcFixture("claude-permission-prompt"), "\n") + "\n" + rule + "\n❯ \n" + rule + "\n"
}

func TestSendBriefHCEscapesBeforeTheSend(t *testing.T) {
	idle := hcadapter.Worker{Live: true, Status: "idle", Turn: 3}
	// The forged screen passes the screen check: that is the limit the
	// Escape exists for.
	if v := hcTable.EvaluateText("claude", forgedPrompt()); !v.Ready() {
		t.Fatalf("the forged screen no longer passes the screen check (%s); revisit the brief's limit and this test", v.Reason)
	}
	for _, tc := range []struct {
		name   string
		screen string
	}{{"clean prompt", ""}, {"forged prompt over a permission dialog", forgedPrompt()}} {
		f := hcBriefFake{statuses: []hcadapter.Worker{idle}, screen: tc.screen, sendRes: hcadapter.SendResult{Status: "working", Turn: 4}}
		res := sendBriefHC(context.Background(), f.steps(), "run the tests", "m1")
		if res.Outcome != briefSent {
			t.Fatalf("%s: outcome %q (%s), want sent", tc.name, res.Outcome, res.Reason)
		}
		if !slices.Equal(f.events, []string{"escape", "send"}) {
			t.Errorf("%s: keys sent = %v, want the Escape right before the paste", tc.name, f.events)
		}
	}
}

func TestSendBriefHCEscapeFailureSendsNothing(t *testing.T) {
	for name, escErr := range map[string]error{"failed": hcadapter.ErrSendFailed, "worker changed": hcadapter.ErrSendRefused} {
		t.Run(name, func(t *testing.T) {
			f := hcBriefFake{statuses: []hcadapter.Worker{{Live: true, Status: "idle", Turn: 3}}, escErr: escErr}
			res := sendBriefHC(context.Background(), f.steps(), "run the tests", "m1")
			if res.Outcome != briefRefused || res.Step != "escape" || len(f.sent) != 0 || len(f.recorded) != 0 {
				t.Fatalf("outcome %q step %q sent %v recorded %v, want refused at escape with nothing sent or recorded",
					res.Outcome, res.Step, f.sent, f.recorded)
			}
		})
	}
}

// TestSendBriefHCEscapeGetsTheCheckedState: the Escape is handed the status
// and turn the checks passed on, so it can refuse a worker that moved since.
func TestSendBriefHCEscapeGetsTheCheckedState(t *testing.T) {
	before := hcadapter.Worker{Live: true, Status: "done", Turn: 3}
	f := hcBriefFake{statuses: []hcadapter.Worker{before}, sendRes: hcadapter.SendResult{Status: "working", Turn: 4}}
	if res := sendBriefHC(context.Background(), f.steps(), "run the tests", "m1"); res.Outcome != briefSent {
		t.Fatalf("outcome %q: %s", res.Outcome, res.Reason)
	}
	if !slices.Equal(f.checked, []hcadapter.Worker{before}) {
		t.Fatalf("Escape was handed %v, want the checked %v", f.checked, before)
	}
}

// TestSendBriefHCIgnoresAModeLineInScrollback: a mode string in older output
// (a vim help page, a diff of this check), or inside a longer row, is not the
// prompt's mode line, so the brief still goes.
func TestSendBriefHCIgnoresAModeLineInScrollback(t *testing.T) {
	ready := hcFixture("claude-ready-acceptedits")
	for name, screen := range map[string]string{
		"scrollback":   "  -- INSERT --\n" + strings.Repeat("output line\n", 20) + ready,
		"inside a row": ready + "\n  the footer says -- NORMAL -- here\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := hcBriefFake{statuses: []hcadapter.Worker{{Live: true, Status: "idle", Turn: 3}}, screen: screen,
				sendRes: hcadapter.SendResult{Status: "working", Turn: 4}}
			if res := sendBriefHC(context.Background(), f.steps(), "run the tests", "m1"); res.Outcome != briefSent {
				t.Fatalf("outcome %q reason %q, want sent", res.Outcome, res.Reason)
			}
		})
	}
}

// TestSendBriefHCRefusesAVimModeLine: an Escape in vim mode switches modes
// instead of closing a dialog, so a screen showing a mode line gets no key.
func TestSendBriefHCRefusesAVimModeLine(t *testing.T) {
	for _, line := range vimModeLines {
		f := hcBriefFake{statuses: []hcadapter.Worker{{Live: true, Status: "idle", Turn: 3}}, screen: hcFixture("claude-ready-acceptedits") + "\n  " + line + "\n"}
		res := sendBriefHC(context.Background(), f.steps(), "run the tests", "m1")
		if res.Outcome != briefRefused || !strings.Contains(res.Reason, "vim mode") || len(f.events) != 0 {
			t.Fatalf("%s: outcome %q reason %q keys %v, want refused on the vim mode line with no key sent", line, res.Outcome, res.Reason, f.events)
		}
	}
}

func TestSendBriefHCNamesTheDialog(t *testing.T) {
	f := hcBriefFake{statuses: []hcadapter.Worker{{Live: true, Status: "idle", Turn: 3}}, screen: hcFixture("claude-permission-prompt")}
	res := sendBriefHC(context.Background(), f.steps(), "run the tests", "m1")
	if res.Outcome != briefRefused || res.Blocking != "permission prompt" || len(f.events) != 0 {
		t.Fatalf("outcome %q blocking %q keys %v, want refused naming the permission prompt with no key sent", res.Outcome, res.Blocking, f.events)
	}
}
