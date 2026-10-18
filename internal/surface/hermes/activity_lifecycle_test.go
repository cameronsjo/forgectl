//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package hermes

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/activity"
)

func TestHermesActivityClosesAtPromptBeforeBlockedHistoryAndResolvesFinished(test *testing.T) {
	test.Setenv("FORGECTL_HERMES_TEST_HELPER", "1")
	test.Setenv("FORGECTL_HERMES_TEST_NATIVE_REPLY", "1")
	worktree, ledger := test.TempDir(), shortHermesTempDir(test)
	gate := filepath.Join(ledger, "post-history-gate")
	marker := filepath.Join(ledger, "post-history-marker")
	if err := writeHermesTestFile(gate, []byte("hold\n")); err != nil {
		test.Fatal(err)
	}
	test.Setenv("FORGECTL_HERMES_TEST_POST_HISTORY_GATE", gate)
	test.Setenv("FORGECTL_HERMES_TEST_POST_HISTORY_MARKER", marker)
	command, err := filepath.Abs(os.Args[0])
	if err != nil {
		test.Fatal(err)
	}
	output := &hermesTestWriter{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	runID := "run-activity-blocked"
	go func() {
		result <- Run(ctx, Config{
			Command: command, Args: []string{"-test.run=TestHermesACPHelper", "--"}, Model: "openai-codex:test", Ledger: ledger,
			Member: "activity-worker", Worktree: worktree, Socket: filepath.Join(ledger, "route.sock"), ListenSocket: filepath.Join(ledger, "listen.sock"), RunID: runID,
			NativeReplies: true,
			PrepareReply: func(context.Context, Request) (ReplyPreparation, error) {
				return ReplyPreparation{Prompt: `Reply with {"forgectl_reply":{"v":1,"in_reply_to":"message-activity","body":"native answer"}}`, SourceDigest: digest("activity-source"), Recipient: "coordinator", RecipientRunID: "coordinator-run"}, nil
			},
			PublishReply: func(context.Context, ReplyPublication) (ReplyPublicationResult, error) {
				return ReplyPublicationResult{MessageID: "outgoing-activity", Status: "queued"}, nil
			},
			Stdout: output, Stderr: output,
		})
	}()
	ready := waitHermesEvent(test, output, "hermes-ready", result)
	sessionID, _ := ready["session_id"].(string)
	if delivery, err := hermesRequest(filepath.Join(ledger, "listen.sock"), Request{V: 1, Type: "deliver", ID: "message-activity", RunID: runID, SessionID: sessionID, Text: "hello Hermes"}); err != nil || !delivery.Unconfirmed {
		test.Fatalf("delivery = %#v, %v", delivery, err)
	}
	waitHermesActivityFile(test, marker)
	blocked := waitHermesActivityInspection(test, filepath.Join(ledger, "listen.sock"), runID, sessionID, func(response Response) bool {
		return response.Inspection != nil && response.Inspection.Activity != nil && len(response.Inspection.Activity.Spans) == 1 && response.Inspection.Activity.Spans[0].State == activity.Unknown && response.Inspection.Activity.Spans[0].EndedAt != nil
	})
	if blocked.Inspection.Idle {
		test.Fatal("blocked post-history turn was reported idle")
	}
	blockedEnd := *blocked.Inspection.Activity.Spans[0].EndedAt
	if err := removeHermesTestFile(gate); err != nil {
		test.Fatal(err)
	}
	waitHermesEvent(test, output, "hermes-native-reply", nil)
	waitHermesEvent(test, output, "hermes-turn-end", nil)
	settled := waitHermesActivityInspection(test, filepath.Join(ledger, "listen.sock"), runID, sessionID, func(response Response) bool {
		return response.Inspection != nil && response.Inspection.Activity != nil && len(response.Inspection.Activity.Spans) == 1 && response.Inspection.Activity.Spans[0].State == activity.Finished
	})
	span := settled.Inspection.Activity.Spans[0]
	if span.EndedAt == nil || !span.EndedAt.Equal(blockedEnd) {
		test.Fatalf("prompt end changed while resolving proof: blocked=%s settled=%v", blockedEnd, span.EndedAt)
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			test.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("supervisor did not stop")
	}
}

func TestHermesActivityProofFailureRemainsClosedUnknown(test *testing.T) {
	test.Setenv("FORGECTL_HERMES_TEST_HELPER", "1")
	test.Setenv("FORGECTL_HERMES_TEST_NATIVE_REPLY", "1")
	test.Setenv("FORGECTL_HERMES_TEST_NATIVE_REPLY_NO_FRAME", "1")
	worktree, ledger := test.TempDir(), shortHermesTempDir(test)
	command, err := filepath.Abs(os.Args[0])
	if err != nil {
		test.Fatal(err)
	}
	output := &hermesTestWriter{}
	publicationCount := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	runID := "run-activity-proof-error"
	go func() {
		result <- Run(ctx, Config{
			Command: command, Args: []string{"-test.run=TestHermesACPHelper", "--"}, Model: "openai-codex:test", Ledger: ledger,
			Member: "activity-worker", Worktree: worktree, Socket: filepath.Join(ledger, "route.sock"), ListenSocket: filepath.Join(ledger, "listen.sock"), RunID: runID,
			NativeReplies: true,
			PrepareReply: func(context.Context, Request) (ReplyPreparation, error) {
				return ReplyPreparation{Prompt: `Reply with {"forgectl_reply":{"v":1,"in_reply_to":"message-proof-error","body":"native answer"}}`, SourceDigest: digest("activity-source"), Recipient: "coordinator", RecipientRunID: "coordinator-run"}, nil
			},
			PublishReply: func(context.Context, ReplyPublication) (ReplyPublicationResult, error) {
				publicationCount++
				return ReplyPublicationResult{MessageID: "forbidden", Status: "queued"}, nil
			},
			Stdout: output, Stderr: output,
		})
	}()
	ready := waitHermesEvent(test, output, "hermes-ready", result)
	sessionID, _ := ready["session_id"].(string)
	if delivery, err := hermesRequest(filepath.Join(ledger, "listen.sock"), Request{V: 1, Type: "deliver", ID: "message-proof-error", RunID: runID, SessionID: sessionID, Text: "hello Hermes"}); err != nil || !delivery.Unconfirmed {
		test.Fatalf("delivery = %#v, %v", delivery, err)
	}
	waitHermesEvent(test, output, "hermes-native-reply", nil)
	waitHermesEvent(test, output, "hermes-turn-end", nil)
	inspection, err := hermesRequest(filepath.Join(ledger, "listen.sock"), Request{V: 1, Type: "inspect", RunID: runID, SessionID: sessionID})
	if err != nil || inspection.Inspection == nil || inspection.Inspection.Activity == nil || len(inspection.Inspection.Activity.Spans) != 1 {
		test.Fatalf("proof-error activity inspection = %#v, %v", inspection, err)
	}
	span := inspection.Inspection.Activity.Spans[0]
	if span.State != activity.Unknown || span.EndedAt == nil || publicationCount != 0 {
		test.Fatalf("proof-error activity = %#v, publications=%d", span, publicationCount)
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			test.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("supervisor did not stop")
	}
}

func TestHermesActivityPromptFailuresNeverRemainRunning(test *testing.T) {
	test.Setenv("FORGECTL_HERMES_TEST_HELPER", "1")
	for _, scenario := range []struct {
		name      string
		configure func(*testing.T, string, string)
		cancel    bool
	}{
		{name: "rpc error", configure: func(test *testing.T, _, _ string) { test.Setenv("FORGECTL_HERMES_TEST_PROMPT_ERROR", "1") }},
		{name: "timeout", configure: func(test *testing.T, gate, marker string) {
			if err := writeHermesTestFile(gate, []byte("hold\n")); err != nil {
				test.Fatal(err)
			}
			test.Setenv("FORGECTL_HERMES_TEST_PROMPT_GATE", gate)
			test.Setenv("FORGECTL_HERMES_TEST_PROMPT_MARKER", marker)
		}},
		{name: "cancel", configure: func(test *testing.T, gate, marker string) {
			if err := writeHermesTestFile(gate, []byte("hold\n")); err != nil {
				test.Fatal(err)
			}
			test.Setenv("FORGECTL_HERMES_TEST_PROMPT_GATE", gate)
			test.Setenv("FORGECTL_HERMES_TEST_PROMPT_MARKER", marker)
		}, cancel: true},
		{name: "native disconnect", configure: func(test *testing.T, _, marker string) {
			test.Setenv("FORGECTL_HERMES_TEST_PROMPT_DISCONNECT", "1")
			test.Setenv("FORGECTL_HERMES_TEST_PROMPT_MARKER", marker)
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			gate := filepath.Join(test.TempDir(), "prompt-gate")
			marker := filepath.Join(test.TempDir(), "prompt-marker")
			for _, key := range []string{"FORGECTL_HERMES_TEST_PROMPT_ERROR", "FORGECTL_HERMES_TEST_PROMPT_GATE", "FORGECTL_HERMES_TEST_PROMPT_MARKER", "FORGECTL_HERMES_TEST_PROMPT_DISCONNECT"} {
				test.Setenv(key, "")
			}
			scenario.configure(test, gate, marker)
			ledger, worktree := shortHermesTempDir(test), test.TempDir()
			receiver, stopNative := newActivitySupervisor(test, ledger, worktree, "run-activity-"+scenario.name)
			record := receipt{ID: "message-failure", SessionID: receiver.sessionID, RunID: receiver.config.RunID, Text: "hello Hermes", Digest: digest("hello Hermes"), State: "pending"}
			var turnContext context.Context
			var cancelTurn context.CancelFunc
			if scenario.cancel {
				turnContext, cancelTurn = context.WithCancel(context.Background())
			} else {
				turnContext, cancelTurn = context.WithTimeout(context.Background(), 150*time.Millisecond)
			}
			defer cancelTurn()
			receiver.forwards.Add(1)
			done := make(chan struct{})
			go func() {
				receiver.forward(turnContext, record)
				close(done)
			}()
			if os.Getenv("FORGECTL_HERMES_TEST_PROMPT_MARKER") != "" {
				waitHermesActivityFile(test, os.Getenv("FORGECTL_HERMES_TEST_PROMPT_MARKER"))
			}
			if scenario.cancel {
				cancelTurn()
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				test.Fatal("forward did not finish after prompt failure")
			}
			cancelTurn()
			if hermesTestFilePresent(gate) {
				_ = removeHermesTestFile(gate)
			}
			inspection := activitySnapshotForTest(receiver, time.Now().UTC())
			if inspection == nil || len(inspection.Spans) != 1 || inspection.Spans[0].State == activity.Running || inspection.Spans[0].EndedAt == nil {
				test.Fatalf("failure activity = %#v", inspection)
			}
			stopNative()
		})
	}
}

func newActivitySupervisor(test *testing.T, ledger, worktree, runID string) (*supervisor, func()) {
	test.Helper()
	command, err := filepath.Abs(os.Args[0])
	if err != nil {
		test.Fatal(err)
	}
	config := Config{Command: command, Args: []string{"-test.run=TestHermesACPHelper", "--"}, Model: "openai-codex:test", Ledger: ledger, Member: "activity-worker", Worktree: worktree, RunID: runID}
	native, err := startNative(config)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(native.stop)
	receiver := &supervisor{config: config, native: native, receipts: map[string]receipt{}, replies: map[string]replyRecord{}}
	receiver.events = eventWriter{writer: io.Discard, mu: &receiver.eventMu}
	session, model, mode, version, _, err := receiver.prepare(context.Background())
	if err != nil {
		native.stop()
		test.Fatal(err)
	}
	receiver.setNativeSession(session, model, mode, version)
	return receiver, native.stop
}

func activitySnapshotForTest(receiver *supervisor, observedAt time.Time) *activity.Snapshot {
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	return receiver.activitySnapshotLocked(observedAt)
}

func waitHermesActivityFile(test *testing.T, path string) {
	test.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hermesTestFilePresent(path) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	test.Fatalf("activity test marker was not written: %s", path)
}

func waitHermesActivityInspection(test *testing.T, socket, runID, sessionID string, ready func(Response) bool) Response {
	test.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := hermesRequest(socket, Request{V: 1, Type: "inspect", RunID: runID, SessionID: sessionID})
		if err == nil && ready(response) {
			return response
		}
		time.Sleep(10 * time.Millisecond)
	}
	test.Fatal("activity inspection did not reach the expected lifecycle state")
	return Response{}
}
