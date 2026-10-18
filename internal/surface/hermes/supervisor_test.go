//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package hermes

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type hermesTestWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func TestEventWriterEscapesTerminalControlsWithoutChangingValues(test *testing.T) {
	var output bytes.Buffer
	writer := eventWriter{writer: &output, mu: &sync.Mutex{}}
	original := "native response \u009b31m\u202e café\n"
	writer.write(map[string]any{"type": "native-update", "update": map[string]string{"text": original}})
	if strings.ContainsRune(output.String(), '\u009b') || strings.ContainsRune(output.String(), '\u202e') {
		test.Fatalf("terminal controls reached event output: %q", output.String())
	}
	var decoded struct {
		Update struct {
			Text string `json:"text"`
		} `json:"update"`
	}
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		test.Fatal(err)
	}
	if decoded.Update.Text != original {
		test.Fatalf("event changed native text: %q", decoded.Update.Text)
	}
}

func (outputWriter *hermesTestWriter) Write(data []byte) (int, error) {
	outputWriter.mu.Lock()
	defer outputWriter.mu.Unlock()
	return outputWriter.buf.Write(data)
}

func (outputWriter *hermesTestWriter) lines() []map[string]any {
	outputWriter.mu.Lock()
	defer outputWriter.mu.Unlock()
	var result []map[string]any
	for _, line := range bytes.Split(outputWriter.buf.Bytes(), []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var value map[string]any
		if json.Unmarshal(line, &value) == nil {
			result = append(result, value)
		}
	}
	return result
}

func TestHermesACPHelper(test *testing.T) {
	if os.Getenv("FORGECTL_HERMES_TEST_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), maxLine)
	out := json.NewEncoder(os.Stdout)
	lastPrompt := ""
	lastAssistant := ""
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		respond := func(result any) {
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(request.ID), "result": result})
		}
		respondError := func() {
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(request.ID), "error": map[string]any{"code": -32001, "message": "test prompt failure"}})
		}
		switch request.Method {
		case "initialize":
			respond(map[string]any{
				"agentInfo":         map[string]any{"name": "hermes-agent", "version": "test-native-1"},
				"agentCapabilities": map[string]any{"loadSession": true},
				"authMethods":       []map[string]string{{"id": "openai-codex"}},
			})
		case "authenticate":
			respond(map[string]any{})
		case "session/new", "session/load":
			if request.Method == "session/new" && os.Getenv("FORGECTL_HERMES_TEST_PREEXISTING") == "1" {
				_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
					"sessionId": "session-test",
					"update":    map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]string{"type": "text", "text": "preexisting native prompt"}},
				}})
			}
			if request.Method == "session/load" && lastPrompt != "" {
				if marker := os.Getenv("FORGECTL_HERMES_TEST_POST_HISTORY_MARKER"); marker != "" {
					_ = writeHermesTestFile(marker, []byte("post-history-called\n"))
				}
				if gate := os.Getenv("FORGECTL_HERMES_TEST_POST_HISTORY_GATE"); gate != "" {
					for hermesTestFilePresent(gate) {
						time.Sleep(5 * time.Millisecond)
					}
				}
				_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
					"sessionId": "session-test",
					"update":    map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]string{"type": "text", "text": lastPrompt}},
				}})
				if lastAssistant != "" {
					_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
						"sessionId": "session-test",
						"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": lastAssistant}},
					}})
				}
				lastPrompt = ""
				lastAssistant = ""
			}
			respond(map[string]any{"sessionId": "session-test", "models": map[string]string{"currentModelId": "openai-codex:test"}, "modes": map[string]string{"currentModeId": "default"}})
			if os.Getenv("FORGECTL_HERMES_TEST_CLOSE_STDOUT") == "1" {
				_ = os.Stdout.Close()
			}
		case "session/prompt":
			if marker := os.Getenv("FORGECTL_HERMES_TEST_PROMPT_MARKER"); marker != "" {
				_ = writeHermesTestFile(marker, []byte("prompt-called\n"))
			}
			if os.Getenv("FORGECTL_HERMES_TEST_PROMPT_DISCONNECT") == "1" {
				_ = os.Stdout.Close()
				os.Exit(0)
			}
			if gate := os.Getenv("FORGECTL_HERMES_TEST_PROMPT_GATE"); gate != "" {
				for hermesTestFilePresent(gate) {
					time.Sleep(5 * time.Millisecond)
				}
			}
			if os.Getenv("FORGECTL_HERMES_TEST_PROMPT_ERROR") == "1" {
				respondError()
				continue
			}
			if prompt, ok := request.Params["prompt"].([]any); ok && len(prompt) > 0 {
				if text, ok := prompt[0].(map[string]any)["text"].(string); ok {
					lastPrompt = text
					if os.Getenv("FORGECTL_HERMES_TEST_NATIVE_REPLY") == "1" {
						const marker = `"in_reply_to":"`
						start := strings.Index(text, marker)
						if start >= 0 {
							start += len(marker)
							end := strings.IndexByte(text[start:], '"')
							if end >= 0 {
								inputID := text[start : start+end]
								if os.Getenv("FORGECTL_HERMES_TEST_NATIVE_REPLY_NO_FRAME") != "1" {
									lastAssistant = fmt.Sprintf(`{"forgectl_reply":{"v":1,"in_reply_to":%q,"body":"native answer"}}`, inputID)
									_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
										"sessionId": "session-test",
										"update":    map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": lastAssistant}},
									}})
								}
							}
						}
					}
				}
			}
			if os.Getenv("FORGECTL_HERMES_TEST_PERMISSION") == "1" {
				_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": 99, "method": "session/request_permission", "params": map[string]any{}})
				if !scanner.Scan() {
					os.Exit(1)
				}
				var answer struct {
					Result struct {
						Outcome struct {
							Outcome string `json:"outcome"`
						} `json:"outcome"`
					} `json:"result"`
				}
				if json.Unmarshal(scanner.Bytes(), &answer) != nil || answer.Result.Outcome.Outcome != "cancelled" {
					os.Exit(2)
				}
			}
			reason := os.Getenv("FORGECTL_HERMES_TEST_STOP_REASON")
			if reason == "" {
				reason = "end_turn"
			}
			respond(map[string]any{"stopReason": reason})
		default:
			respond(map[string]any{})
		}
	}
	os.Exit(0)
}

func writeHermesTestFile(path string, contents []byte) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.WriteFile(filepath.Base(path), contents, 0o600)
}

func removeHermesTestFile(path string) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return root.Remove(filepath.Base(path))
}

func hermesTestFilePresent(path string) bool {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	_, err = root.Stat(filepath.Base(path))
	return err == nil
}

func TestHermesSupervisorLifecycleAndReceipts(test *testing.T) {
	test.Setenv("FORGECTL_HERMES_TEST_HELPER", "1")
	test.Setenv("FORGECTL_HERMES_TEST_PERMISSION", "1")
	test.Setenv("FORGECTL_HERMES_TEST_STOP_REASON", "refusal")
	worktree, ledger := test.TempDir(), shortHermesTempDir(test)
	routeSocket := filepath.Join(ledger, "route.sock")
	listenSocket := filepath.Join(ledger, "listen.sock")
	command, err := filepath.Abs(os.Args[0])
	if err != nil {
		test.Fatal(err)
	}
	var output hermesTestWriter
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- Run(ctx, Config{Command: command, Args: []string{"-test.run=TestHermesACPHelper", "--"}, Model: "openai-codex:test", Ledger: ledger, Member: "hermes-worker", Worktree: worktree, Socket: routeSocket, ListenSocket: listenSocket, RunID: "run-1", Stdout: &output, Stderr: &output})
	}()
	ready := waitHermesEvent(test, &output, "hermes-ready", result)
	if ready["route_socket"] != routeSocket || ready["listen_socket"] != listenSocket || ready["mode"] != "default" {
		test.Fatalf("ready event = %#v", ready)
	}
	sessionID, _ := ready["session_id"].(string)
	if sessionID != "session-test" {
		test.Fatalf("session id = %q", sessionID)
	}
	idleInspection, err := hermesRequest(listenSocket, Request{V: 1, Type: "inspect", RunID: "run-1", SessionID: sessionID})
	if err != nil || !idleInspection.OK || idleInspection.Inspection == nil || idleInspection.Inspection.Activity == nil || len(idleInspection.Inspection.Activity.Spans) != 0 {
		test.Fatalf("idle activity inspection = %#v, %v", idleInspection, err)
	}
	wrong, err := hermesRequest(listenSocket, Request{V: 1, Type: "state", RunID: "run-1", SessionID: "wrong-session"})
	if err != nil || wrong.OK || wrong.Error == "" {
		test.Fatalf("wrong-session response = %#v, %v", wrong, err)
	}
	delivery, err := hermesRequest(listenSocket, Request{V: 1, Type: "deliver", ID: "message-1", RunID: "run-1", SessionID: sessionID, Text: "hello Hermes"})
	if err != nil || !delivery.Unconfirmed {
		test.Fatalf("delivery = %#v, %v", delivery, err)
	}
	receipt := waitHermesReceipt(test, listenSocket, sessionID, "message-1")
	if !receipt.OK || !receipt.Known || receipt.Evidence != "history" || receipt.Accepted == nil || !*receipt.Accepted {
		test.Fatalf("receipt = %#v", receipt)
	}
	permission := waitHermesEvent(test, &output, "hermes-permission-denied", nil)
	if permission["native_request_id"] != "99" || permission["session_id"] != sessionID {
		test.Fatalf("permission event = %#v", permission)
	}
	turn := waitHermesEvent(test, &output, "hermes-turn-end", nil)
	if turn["stop_reason"] != "refusal" || turn["error"] != false {
		test.Fatalf("native stop reason was not preserved: %#v", turn)
	}
	settledInspection, err := hermesRequest(listenSocket, Request{V: 1, Type: "inspect", RunID: "run-1", SessionID: sessionID})
	if err != nil || !settledInspection.OK || settledInspection.Inspection == nil || settledInspection.Inspection.Activity == nil || len(settledInspection.Inspection.Activity.Spans) != 1 {
		test.Fatalf("settled activity inspection = %#v, %v", settledInspection, err)
	}
	span := settledInspection.Inspection.Activity.Spans[0]
	if span.State != "unknown" || span.EndedAt == nil || span.ID != "message-1" || span.TurnID != "message-1" {
		test.Fatalf("permission/refusal activity = %#v", span)
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
	exits := hermesEvents(&output, "hermes-native-exit")
	if len(exits) != 1 || exits[0]["run_id"] != "run-1" || exits[0]["session_id"] != sessionID || exits[0]["exitcode"] != float64(0) {
		test.Fatalf("native exits = %#v", exits)
	}
	if _, err := os.Stat(filepath.Join(ledger, "hermes-receipts-"+sessionKey("hermes-worker", sessionID)+".jsonl")); err != nil {
		test.Fatalf("receipt journal missing: %v", err)
	}
}

func TestHermesNativeReplyPublishesAndRecoversWithoutReprompt(test *testing.T) {
	test.Setenv("FORGECTL_HERMES_TEST_HELPER", "1")
	test.Setenv("FORGECTL_HERMES_TEST_NATIVE_REPLY", "1")
	worktree, ledger := test.TempDir(), shortHermesTempDir(test)
	command, err := filepath.Abs(os.Args[0])
	if err != nil {
		test.Fatal(err)
	}
	start := func(publish func(ReplyPublication) (ReplyPublicationResult, error), output *hermesTestWriter, runID string) (<-chan error, string, context.CancelFunc) {
		test.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			result <- Run(ctx, Config{
				Command: command, Args: []string{"-test.run=TestHermesACPHelper", "--"}, Model: "openai-codex:test", Ledger: ledger,
				Member: "hermes-worker", Worktree: worktree, Socket: filepath.Join(ledger, runID+"-route.sock"), ListenSocket: filepath.Join(ledger, runID+"-listen.sock"), RunID: runID,
				NativeReplies: true,
				PrepareReply: func(context.Context, Request) (ReplyPreparation, error) {
					return ReplyPreparation{Prompt: `Reply with {"forgectl_reply":{"v":1,"in_reply_to":"message-1","body":"native answer"}}`, SourceDigest: digest("source"), Recipient: "coordinator", RecipientRunID: "coordinator-run"}, nil
				},
				PublishReply: func(_ context.Context, publication ReplyPublication) (ReplyPublicationResult, error) {
					return publish(publication)
				},
				Stdout: output, Stderr: output,
			})
		}()
		return result, "session-test", cancel
	}
	firstOutput := &hermesTestWriter{}
	firstResult, sessionID, cancelFirst := start(func(ReplyPublication) (ReplyPublicationResult, error) {
		return ReplyPublicationResult{}, errors.New("simulated publication transport loss")
	}, firstOutput, "run-reply")
	waitHermesEvent(test, firstOutput, "hermes-ready", firstResult)
	delivery, err := hermesRequest(filepath.Join(ledger, "run-reply-listen.sock"), Request{V: 1, Type: "deliver", ID: "message-1", RunID: "run-reply", SessionID: sessionID, Text: "hello Hermes"})
	if err != nil || !delivery.Unconfirmed {
		test.Fatalf("native reply delivery = %#v, %v", delivery, err)
	}
	waitHermesReceiptForRun(test, filepath.Join(ledger, "run-reply-listen.sock"), sessionID, "message-1", "run-reply")
	turn := waitHermesEvent(test, firstOutput, "hermes-turn-end", nil)
	if turn["error"] != false {
		test.Fatalf("native turn was not complete: %#v", turn)
	}
	inspection, err := hermesRequest(filepath.Join(ledger, "run-reply-listen.sock"), Request{V: 1, Type: "inspect", RunID: "run-reply", SessionID: sessionID})
	if err != nil || inspection.Inspection == nil || inspection.Inspection.Activity == nil || len(inspection.Inspection.Activity.Spans) != 1 || inspection.Inspection.Activity.Spans[0].State != "finished" || inspection.Inspection.Activity.Spans[0].EndedAt == nil {
		test.Fatalf("successful native activity = %#v, %v", inspection, err)
	}
	firstReply := waitHermesEvent(test, firstOutput, "hermes-native-reply", nil)
	if firstReply["state"] != "ready" || firstReply["error"] != true {
		test.Fatalf("failed publication did not retain ready state: %#v", firstReply)
	}
	cancelFirst()
	select {
	case err := <-firstResult:
		if err != nil {
			test.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("first supervisor did not stop")
	}
	secondOutput := &hermesTestWriter{}
	publicationCount := 0
	secondResult, _, cancelSecond := start(func(publication ReplyPublication) (ReplyPublicationResult, error) {
		publicationCount++
		if publication.Input.ID != "message-1" || publication.Body != "native answer" {
			test.Fatalf("recovered publication = %#v", publication)
		}
		return ReplyPublicationResult{MessageID: "outgoing-1", Status: "queued"}, nil
	}, secondOutput, "run-reply")
	defer cancelSecond()
	waitHermesEvent(test, secondOutput, "hermes-native-reply", nil)
	if publicationCount != 1 {
		test.Fatalf("recovered publication count = %d", publicationCount)
	}
	cancelSecond()
	select {
	case err := <-secondResult:
		if err != nil {
			test.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("second supervisor did not stop")
	}
}

func TestHermesNativeReplyRejectsPermissionAndNoFrameSeparately(test *testing.T) {
	test.Setenv("FORGECTL_HERMES_TEST_HELPER", "1")
	test.Setenv("FORGECTL_HERMES_TEST_NATIVE_REPLY", "1")
	worktree := test.TempDir()
	command, err := filepath.Abs(os.Args[0])
	if err != nil {
		test.Fatal(err)
	}
	run := func(test *testing.T, runID string, noFrame, permission bool) {
		test.Helper()
		ledger := shortHermesTempDir(test)
		if noFrame {
			test.Setenv("FORGECTL_HERMES_TEST_NATIVE_REPLY_NO_FRAME", "1")
		} else {
			test.Setenv("FORGECTL_HERMES_TEST_NATIVE_REPLY_NO_FRAME", "")
		}
		if permission {
			test.Setenv("FORGECTL_HERMES_TEST_PERMISSION", "1")
		} else {
			test.Setenv("FORGECTL_HERMES_TEST_PERMISSION", "")
		}
		output := &hermesTestWriter{}
		publicationCount := 0
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			result <- Run(ctx, Config{
				Command: command, Args: []string{"-test.run=TestHermesACPHelper", "--"}, Model: "openai-codex:test", Ledger: ledger,
				Member: "reply-worker", Worktree: worktree, Socket: filepath.Join(ledger, runID+"-route.sock"), ListenSocket: filepath.Join(ledger, runID+"-listen.sock"), RunID: runID,
				NativeReplies: true,
				PrepareReply: func(context.Context, Request) (ReplyPreparation, error) {
					return ReplyPreparation{Prompt: `Reply with {"forgectl_reply":{"v":1,"in_reply_to":"message-2","body":"native answer"}}`, SourceDigest: digest("source"), Recipient: "coordinator", RecipientRunID: "coordinator-run"}, nil
				},
				PublishReply: func(context.Context, ReplyPublication) (ReplyPublicationResult, error) {
					publicationCount++
					return ReplyPublicationResult{MessageID: "outgoing-2", Status: "queued"}, nil
				},
				Stdout: output, Stderr: output,
			})
		}()
		ready := waitHermesEvent(test, output, "hermes-ready", result)
		sessionID, _ := ready["session_id"].(string)
		socket := filepath.Join(ledger, runID+"-listen.sock")
		if delivery, err := hermesRequest(socket, Request{V: 1, Type: "deliver", ID: "message-2", RunID: runID, SessionID: sessionID, Text: "hello Hermes"}); err != nil || !delivery.Unconfirmed {
			test.Fatalf("delivery = %#v, %v", delivery, err)
		}
		waitHermesReceiptForRun(test, socket, sessionID, "message-2", runID)
		turn := waitHermesEvent(test, output, "hermes-turn-end", nil)
		if turn["error"] != false {
			test.Fatalf("native turn error = %#v", turn)
		}
		reply := waitHermesEvent(test, output, "hermes-native-reply", nil)
		if reply["state"] != "unknown" || reply["status"] != "proof-error" || publicationCount != 0 {
			test.Fatalf("reply proof status = %#v, publications=%d", reply, publicationCount)
		}
		inspection, err := hermesRequest(socket, Request{V: 1, Type: "inspect", RunID: runID, SessionID: sessionID})
		if err != nil || inspection.Inspection == nil || inspection.Inspection.Turn == nil || inspection.Inspection.Turn.Status != "unknown" {
			test.Fatalf("unproven reply became completed turn evidence: %#v, %v", inspection, err)
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
	test.Run("permission", func(test *testing.T) { run(test, "run-permission", false, true) })
	test.Run("no-frame", func(test *testing.T) { run(test, "run-no-frame", true, false) })
}

func TestHermesNativeReplyRejectsPreexistingInputBeforePrompt(test *testing.T) {
	test.Setenv("FORGECTL_HERMES_TEST_HELPER", "1")
	test.Setenv("FORGECTL_HERMES_TEST_PREEXISTING", "1")
	worktree, ledger := test.TempDir(), shortHermesTempDir(test)
	marker := filepath.Join(ledger, "prompt-called")
	test.Setenv("FORGECTL_HERMES_TEST_PROMPT_MARKER", marker)
	command, err := filepath.Abs(os.Args[0])
	if err != nil {
		test.Fatal(err)
	}
	output := &hermesTestWriter{}
	publicationCount := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- Run(ctx, Config{
			Command: command, Args: []string{"-test.run=TestHermesACPHelper", "--"}, Model: "openai-codex:test", Ledger: ledger,
			Member: "reply-worker", Worktree: worktree, Socket: filepath.Join(ledger, "route.sock"), ListenSocket: filepath.Join(ledger, "listen.sock"), RunID: "run-preexisting",
			NativeReplies: true,
			PrepareReply: func(context.Context, Request) (ReplyPreparation, error) {
				return ReplyPreparation{Prompt: "preexisting native prompt", SourceDigest: digest("source"), Recipient: "coordinator", RecipientRunID: "coordinator-run"}, nil
			},
			PublishReply: func(context.Context, ReplyPublication) (ReplyPublicationResult, error) {
				publicationCount++
				return ReplyPublicationResult{MessageID: "outgoing", Status: "queued"}, nil
			},
			Stdout: output, Stderr: output,
		})
	}()
	ready := waitHermesEvent(test, output, "hermes-ready", result)
	sessionID, _ := ready["session_id"].(string)
	delivery, err := hermesRequest(filepath.Join(ledger, "listen.sock"), Request{V: 1, Type: "deliver", ID: "message-preexisting", RunID: "run-preexisting", SessionID: sessionID, Text: "hello Hermes"})
	if err != nil || !delivery.Unconfirmed {
		test.Fatalf("delivery = %#v, %v", delivery, err)
	}
	reply := waitHermesEvent(test, output, "hermes-native-reply", nil)
	if reply["state"] != replyUnknown || reply["status"] != "turn-error" || publicationCount != 0 {
		test.Fatalf("preexisting input proof = %#v, publications=%d", reply, publicationCount)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		test.Fatalf("native prompt was called, marker error=%v", err)
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

func TestHermesSupervisorClosedTransportWaitsForNativeExit(test *testing.T) {
	test.Setenv("FORGECTL_HERMES_TEST_HELPER", "1")
	test.Setenv("FORGECTL_HERMES_TEST_CLOSE_STDOUT", "1")
	worktree, ledger := test.TempDir(), shortHermesTempDir(test)
	routeSocket, listenSocket := filepath.Join(ledger, "route.sock"), filepath.Join(ledger, "listen.sock")
	command, _ := filepath.Abs(os.Args[0])
	var output hermesTestWriter
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- Run(ctx, Config{Command: command, Args: []string{"-test.run=TestHermesACPHelper", "--"}, Model: "openai-codex:test", Ledger: ledger, Member: "worker", Worktree: worktree, Socket: routeSocket, ListenSocket: listenSocket, RunID: "run-eof", Stdout: &output, Stderr: &output})
	}()
	waitHermesEvent(test, &output, "hermes-ready", result)
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "EOF") {
			test.Fatalf("closed native transport error = %v", err)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("supervisor did not wait for native process")
	}
	exits := hermesEvents(&output, "hermes-native-exit")
	if len(exits) != 1 || exits[0]["exitcode"] != float64(0) {
		test.Fatalf("native EOF exits = %#v", exits)
	}
}

func TestHermesSupervisorSessionLockRejectsMemberAlias(test *testing.T) {
	test.Setenv("FORGECTL_HERMES_TEST_HELPER", "1")
	worktree, ledger := test.TempDir(), shortHermesTempDir(test)
	command, err := filepath.Abs(os.Args[0])
	if err != nil {
		test.Fatal(err)
	}
	firstOutput := &hermesTestWriter{}
	firstContext, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstResult := make(chan error, 1)
	go func() {
		firstResult <- Run(firstContext, Config{Command: command, Args: []string{"-test.run=TestHermesACPHelper", "--"}, Model: "openai-codex:test", Ledger: ledger, Member: "member-a", Worktree: worktree, Socket: filepath.Join(ledger, "route-a.sock"), ListenSocket: filepath.Join(ledger, "listen-a.sock"), RunID: "run-a", SessionID: "session-test", Stdout: firstOutput, Stderr: firstOutput})
	}()
	waitHermesEvent(test, firstOutput, "hermes-ready", firstResult)
	secondResult := make(chan error, 1)
	go func() {
		secondResult <- Run(context.Background(), Config{Command: command, Args: []string{"-test.run=TestHermesACPHelper", "--"}, Model: "openai-codex:test", Ledger: ledger, Member: "member-b", Worktree: worktree, Socket: filepath.Join(ledger, "route-b.sock"), ListenSocket: filepath.Join(ledger, "listen-b.sock"), RunID: "run-b", SessionID: "session-test", Stdout: &hermesTestWriter{}})
	}()
	select {
	case err := <-secondResult:
		if err == nil || err.Error() != "another Hermes supervisor owns this native session" {
			test.Fatalf("second supervisor error = %v", err)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("second supervisor did not contend for session lock")
	}
	cancelFirst()
	select {
	case err := <-firstResult:
		if err != nil {
			test.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("first supervisor did not stop")
	}
}

func TestHermesCloseListenerPreservesReplacement(test *testing.T) {
	path := filepath.Join(shortHermesTempDir(test), "listen.sock")
	first, info, err := listen(test.Context(), path)
	if err != nil {
		test.Fatal(err)
	}
	if err := first.Close(); err != nil {
		test.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		test.Fatal(err)
	}
	replacement, replacementInfo, err := listen(test.Context(), path)
	if err != nil {
		test.Fatal(err)
	}
	defer func() { _ = replacement.Close() }()
	if os.SameFile(info, replacementInfo) {
		test.Fatal("replacement unexpectedly reused listener identity")
	}
	receiver := &supervisor{config: Config{ListenSocket: path}, listener: first, listenerInfo: info}
	receiver.closeListener()
	if _, err := os.Stat(path); err != nil {
		test.Fatalf("replacement listener was removed: %v", err)
	}
}

func waitHermesEvent(test *testing.T, output *hermesTestWriter, typ string, result <-chan error) map[string]any {
	test.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, event := range output.lines() {
			if event["type"] == typ {
				return event
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if result != nil {
		select {
		case err := <-result:
			test.Fatalf("supervisor returned before %s: %v", typ, err)
		default:
		}
	}
	test.Fatalf("event %q not emitted: %#v", typ, output.lines())
	return nil
}

func shortHermesTempDir(test *testing.T) string {
	test.Helper()
	directory, err := os.MkdirTemp("/tmp", "hermes-")
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func hermesEvents(output *hermesTestWriter, typ string) []map[string]any {
	var result []map[string]any
	for _, event := range output.lines() {
		if event["type"] == typ {
			result = append(result, event)
		}
	}
	return result
}

func hermesRequest(socket string, request Request) (Response, error) {
	connection, err := (&net.Dialer{Timeout: time.Second}).DialContext(context.Background(), "unix", socket)
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	data, _ := json.Marshal(request)
	if _, err := connection.Write(append(data, '\n')); err != nil {
		return Response{}, err
	}
	var response Response
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		return Response{}, err
	}
	return response, nil
}

func waitHermesReceipt(test *testing.T, socket, sessionID, id string) Response {
	return waitHermesReceiptForRun(test, socket, sessionID, id, "run-1")
}

func waitHermesReceiptForRun(test *testing.T, socket, sessionID, id, runID string) Response {
	test.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := hermesRequest(socket, Request{V: 1, Type: "receipt", ID: id, RunID: runID, SessionID: sessionID})
		if err == nil && response.Accepted != nil && *response.Accepted {
			return response
		}
		time.Sleep(20 * time.Millisecond)
	}
	test.Fatal("receipt was not accepted")
	return Response{}
}
