package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
	"time"

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
	own := map[string]string{"HC_TERMINAL_ID": "t9", "HC_SESSION": "default", "HERDR_PANE_ID": "w1:p1"}
	// The launcher's own HC_TERMINAL_ID must not reach the worker.
	env := markPane([]string{"PATH=/bin"}, "hc")
	got := paneIdentityEnv(env, func(k string) string { return own[k] })
	for _, want := range []string{"HC_TERMINAL_ID=t9", "HC_SESSION=default"} {
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
		send: func(_ context.Context, s string) (hcadapter.SendResult, error) {
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
