//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package hermes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestHermesReconciliationRequiresUniqueNativeHistory(test *testing.T) {
	text := "[forgectl-msg] from=coordinator to=worker id=message-1\nbody\n[/forgectl-msg]"
	record := receipt{ID: "message-1", SessionID: "session-1", RunID: "retired-run", Text: text, Digest: digest(text), State: "pending"}
	receiver := &supervisor{config: Config{RunID: "current-run"}, sessionID: record.SessionID, receiptPath: filepath.Join(test.TempDir(), "receipts.jsonl"), receipts: map[string]receipt{}}
	if err := receiver.appendReceiptLocked(record); err != nil {
		test.Fatal(err)
	}
	exact := historyUpdate{SessionID: record.SessionID, Kind: "user_message_chunk", Text: text}
	for _, history := range [][]historyUpdate{
		nil,
		{{SessionID: "other-session", Kind: exact.Kind, Text: text}},
		{{SessionID: record.SessionID, Kind: "agent_message_chunk", Text: text}},
		{{SessionID: record.SessionID, Kind: exact.Kind, Text: text + "different"}},
		{exact, exact},
	} {
		if err := receiver.reconcile(history); err != nil {
			test.Fatal(err)
		}
		response := receiver.receiptResponse(Response{}, record.ID)
		if !response.Unconfirmed || response.Accepted != nil || receiver.receipts[record.ID].State != "pending" {
			test.Fatalf("ambiguous history was confirmed: %#v", response)
		}
	}
	if err := receiver.reconcile([]historyUpdate{exact}); err != nil {
		test.Fatal(err)
	}
	persisted, err := loadReceipts(receiver.receiptPath)
	if err != nil || persisted[record.ID].State != "accepted" || persisted[record.ID].RunID != "current-run" {
		test.Fatalf("restored receipt = %#v, %v", persisted, err)
	}
	receiver.receipts = persisted
	response := receiver.deliverResponse(test.Context(), Request{ID: record.ID, Text: text})
	if !response.OK || response.Accepted == nil || !*response.Accepted || response.Evidence != "history" {
		test.Fatalf("duplicate delivery lacked its existing receipt: %#v", response)
	}
	response = receiver.deliverResponse(test.Context(), Request{ID: record.ID, Text: text + "changed"})
	if response.OK || response.Accepted != nil || !strings.Contains(response.Error, "different text") {
		test.Fatalf("changed duplicate was not refused: %#v", response)
	}
}

func TestHermesReceiptWriteFailureRemainsPending(test *testing.T) {
	record := receipt{ID: "message-1", SessionID: "session-1", Text: "exact text", Digest: digest("exact text"), State: "pending"}
	receiver := &supervisor{sessionID: record.SessionID, receiptPath: test.TempDir(), receipts: map[string]receipt{record.ID: record}}
	err := receiver.reconcile([]historyUpdate{{SessionID: record.SessionID, Kind: "user_message_chunk", Text: record.Text}})
	if err == nil || receiver.receipts[record.ID].State != "pending" {
		test.Fatalf("failed receipt write was not retained as pending: %v, %#v", err, receiver.receipts)
	}
}

func TestHermesHistoryCannotConfirmTwoReceiptIDs(test *testing.T) {
	for _, firstState := range []string{"pending", "accepted"} {
		test.Run(firstState, func(test *testing.T) {
			first := receipt{ID: "first-id", SessionID: "session-1", Text: "same text", Digest: digest("same text"), State: firstState}
			second := first
			second.ID, second.State = "second-id", "pending"
			receiver := &supervisor{sessionID: first.SessionID, receiptPath: filepath.Join(test.TempDir(), "receipts.jsonl"), receipts: map[string]receipt{first.ID: first, second.ID: second}}
			if err := receiver.reconcile([]historyUpdate{{SessionID: first.SessionID, Kind: "user_message_chunk", Text: first.Text}}); err != nil {
				test.Fatal(err)
			}
			if receiver.receipts[first.ID].State != firstState || receiver.receipts[second.ID].State != "pending" {
				test.Fatalf("one history entry confirmed ambiguous receipt IDs: %#v", receiver.receipts)
			}
		})
	}
}

func TestHermesBusyAndStoppingDoNotWriteReceipts(test *testing.T) {
	receiver := &supervisor{busy: true, sessionID: "session-1", receiptPath: filepath.Join(test.TempDir(), "never-written.jsonl"), receipts: map[string]receipt{}}
	request := Request{ID: "message-1", Text: "body"}
	response := receiver.deliverResponse(test.Context(), request)
	if !response.Retry || !strings.Contains(response.Error, "busy") {
		test.Fatalf("busy delivery = %#v", response)
	}
	ctx, cancel := context.WithCancel(test.Context())
	cancel()
	receiver.busy = false
	response = receiver.deliverResponse(ctx, request)
	if !response.Retry || !strings.Contains(response.Error, "stopping") {
		test.Fatalf("stopping delivery = %#v", response)
	}
	if len(receiver.receipts) != 0 {
		test.Fatalf("refused deliveries created receipts: %#v", receiver.receipts)
	}
	if _, err := os.Stat(receiver.receiptPath); !os.IsNotExist(err) {
		test.Fatalf("refused deliveries wrote the receipt file: %v", err)
	}
}

func TestHermesNativeReplyIntentPrecedesReceiptOnWriteFailure(test *testing.T) {
	receiptDirectory := test.TempDir()
	receiptPath := filepath.Join(receiptDirectory, "receipts.jsonl")
	replyDirectory := test.TempDir()
	replyPath := filepath.Join(replyDirectory, "replies.jsonl")
	request := Request{ID: "message-1", Text: "exact text"}
	prepareCalls := 0
	receiver := &supervisor{
		config: Config{
			RunID:         "run-1",
			NativeReplies: true,
			PrepareReply: func(context.Context, Request) (ReplyPreparation, error) {
				prepareCalls++
				return ReplyPreparation{}, errors.New("test preparation stop")
			},
		},
		sessionID:   "session-1",
		receiptPath: receiptPath,
		receipts:    map[string]receipt{},
		replyPath:   replyDirectory,
		replies:     map[string]replyRecord{},
		events:      eventWriter{writer: io.Discard, mu: &sync.Mutex{}},
	}
	first := receiver.deliverResponse(test.Context(), request)
	if !first.Retry || len(receiver.receipts) != 0 || len(receiver.replies) != 0 || prepareCalls != 0 {
		test.Fatalf("failed intent write left replay state: response=%#v receipts=%#v replies=%#v prepares=%d", first, receiver.receipts, receiver.replies, prepareCalls)
	}
	if _, err := os.Stat(receiptPath); !errors.Is(err, os.ErrNotExist) {
		test.Fatalf("failed intent write created receipt journal: %v", err)
	}
	receiver.replyPath = replyPath
	second := receiver.deliverResponse(test.Context(), request)
	if second.Retry || !second.Unconfirmed || receiver.receipts[request.ID].Text != request.Text {
		test.Fatalf("same-id retry was not accepted after intent recovery: %#v receipts=%#v", second, receiver.receipts)
	}
	receiver.forwards.Wait()
	if prepareCalls != 1 {
		test.Fatalf("retry replayed native prompt unexpectedly: preparation calls=%d", prepareCalls)
	}
}

func TestHermesNativeReplyRequiresAResult(test *testing.T) {
	client := &nativeClient{}
	for _, response := range []nativeEnvelope{
		{JSONRPC: "2.0", ID: json.RawMessage("1")},
		{JSONRPC: "2.0", ID: json.RawMessage("1"), Error: json.RawMessage(`{"code":-1,"message":"failed"}`)},
	} {
		_, done, err := client.processMessage(nativeMessage{envelope: response}, 1, "session/prompt", nil)
		if !done || err == nil {
			test.Fatalf("invalid native reply accepted: %#v, %t, %v", response, done, err)
		}
	}
	result, done, err := client.processMessage(nativeMessage{envelope: nativeEnvelope{JSONRPC: "2.0", ID: json.RawMessage("1"), Result: json.RawMessage(`{"stopReason":"end_turn"}`)}}, 1, "session/prompt", nil)
	if err != nil || !done || string(result) != `{"stopReason":"end_turn"}` {
		test.Fatalf("valid reply rejected: %s, %t, %v", result, done, err)
	}
}

func TestHermesNativeResponseIDsFailClosed(test *testing.T) {
	for name, response := range map[string]nativeEnvelope{
		"missing": {JSONRPC: "2.0", Result: json.RawMessage(`{"stopReason":"end_turn"}`)},
		"invalid": {JSONRPC: "2.0", ID: json.RawMessage(`"1"`), Result: json.RawMessage(`{"stopReason":"end_turn"}`)},
		"foreign": {JSONRPC: "2.0", ID: json.RawMessage(`2`), Result: json.RawMessage(`{"stopReason":"end_turn"}`)},
	} {
		test.Run(name, func(test *testing.T) {
			if _, done, err := (&nativeClient{}).processMessage(nativeMessage{envelope: response}, 1, "session/prompt", nil); err == nil || done {
				test.Fatalf("malformed response id was not rejected: done=%t err=%v", done, err)
			}
		})
	}
}

func TestHermesNativeReadRejectsInvalidJSONText(test *testing.T) {
	for name, line := range map[string][]byte{
		"invalid UTF-8":      []byte{'{', '"', 'j', 's', 'o', 'n', 'r', 'p', 'c', '"', ':', '"', '2', '.', '0', '"', ',', '"', 'x', '"', ':', '"', 0xff, '"', '}', '\n'},
		"unpaired surrogate": []byte(`{"jsonrpc":"2.0","x":"\ud800"}` + "\n"),
	} {
		test.Run(name, func(test *testing.T) {
			client := &nativeClient{events: make(chan nativeMessage, 1), readErr: make(chan struct{})}
			client.read(bytes.NewReader(line))
			if client.readError() == nil {
				test.Fatal("invalid native JSON text was accepted")
			}
			if len(client.events) != 0 {
				test.Fatal("invalid native JSON text reached the RPC dispatcher")
			}
		})
	}
}
