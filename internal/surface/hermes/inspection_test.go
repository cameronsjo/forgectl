//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package hermes

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHermesInspectionReadsNativeTurnWithoutSendingPrompt(test *testing.T) {
	for _, scenario := range []struct{ stopReason, status string }{
		{"end_turn", "completed"},
		{"refusal", "incomplete"},
		{"max_tokens", "incomplete"},
	} {
		test.Run(scenario.stopReason, func(test *testing.T) {
			test.Setenv("FORGECTL_HERMES_TEST_HELPER", "1")
			test.Setenv("FORGECTL_HERMES_TEST_STOP_REASON", scenario.stopReason)
			ledger := shortHermesTempDir(test)
			command, err := filepath.Abs(os.Args[0])
			if err != nil {
				test.Fatal(err)
			}
			var output hermesTestWriter
			ctx, cancel := context.WithCancel(test.Context())
			result := make(chan error, 1)
			socket := filepath.Join(ledger, "inspect.sock")
			go func() {
				result <- Run(ctx, Config{Command: command, Args: []string{"-test.run=TestHermesACPHelper", "--"}, Model: "openai-codex:test", Ledger: ledger, Member: "worker", Worktree: test.TempDir(), Socket: socket, RunID: "run-inspect", Stdout: &output})
			}()
			test.Cleanup(func() {
				cancel()
				select {
				case err := <-result:
					if err != nil {
						test.Error(err)
					}
				case <-time.After(5 * time.Second):
					test.Error("inspection fixture did not stop")
				}
			})
			waitHermesEvent(test, &output, "hermes-ready", nil)
			request := Request{V: 1, Type: "inspect", RunID: "run-inspect", SessionID: "session-test"}
			initial, err := hermesRequest(socket, request)
			if err != nil || !initial.OK || initial.Inspection == nil || initial.Inspection.Provider != "openai-codex" || initial.Inspection.Model != "test" || initial.Inspection.ModelEvidence != "native-startup-handshake" || initial.Inspection.Turn != nil {
				test.Fatalf("initial inspection = %#v, %v", initial, err)
			}
			if len(hermesEvents(&output, "hermes-turn-start")) != 0 {
				test.Fatal("inspection started a model turn")
			}
			for _, wrong := range []Request{
				{V: 1, Type: "inspect", RunID: "wrong-run", SessionID: "session-test"},
				{V: 1, Type: "inspect", RunID: "run-inspect", SessionID: "wrong-session"},
				{V: 2, Type: "inspect", RunID: "run-inspect", SessionID: "session-test"},
			} {
				response, err := hermesRequest(socket, wrong)
				if err != nil || response.OK || response.Inspection != nil {
					test.Fatalf("wrong identity/version inspection = %#v, %v", response, err)
				}
			}
			delivery, err := hermesRequest(socket, Request{V: 1, Type: "deliver", ID: "message-inspect", RunID: request.RunID, SessionID: request.SessionID, Text: "fixture prompt"})
			if err != nil || !delivery.Unconfirmed {
				test.Fatalf("delivery = %#v, %v", delivery, err)
			}
			var observed Response
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				observed, err = hermesRequest(socket, request)
				if err != nil {
					test.Fatal(err)
				}
				if observed.Inspection != nil && observed.Inspection.Turn != nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if observed.Inspection == nil || observed.Inspection.Turn == nil || observed.Inspection.Turn.Status != scenario.status || observed.Inspection.Turn.MessageID != "message-inspect" || observed.Inspection.Turn.ObservedAt.IsZero() {
				test.Fatalf("turn inspection = %#v", observed.Inspection)
			}
			if len(hermesEvents(&output, "hermes-turn-start")) != 1 {
				test.Fatal("repeated inspection duplicated a model turn")
			}
		})
	}
}
