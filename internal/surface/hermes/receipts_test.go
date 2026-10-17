//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package hermes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
