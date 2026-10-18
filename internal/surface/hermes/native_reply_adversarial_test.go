//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package hermes

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func replyFrameForTest(test *testing.T, inputID, body string) string {
	test.Helper()
	encoded, err := json.Marshal(map[string]any{"forgectl_reply": map[string]any{"v": 1, "in_reply_to": inputID, "body": body}})
	if err != nil {
		test.Fatal(err)
	}
	return string(encoded)
}

func TestNativeReplyParserBoundaryControls(test *testing.T) {
	for _, body := range []string{strings.Repeat("a", maxReplyBodyBytes), "literal \\ud800", "paired 😀"} {
		actual, err := parseNativeReplyFrame(replyFrameForTest(test, "input-1", body), "input-1")
		if err != nil || actual != body {
			test.Fatalf("valid body of %d bytes changed or failed: %v", len(body), err)
		}
	}
	for name, frame := range map[string]string{
		"oversized body":      replyFrameForTest(test, "input-1", strings.Repeat("a", maxReplyBodyBytes+1)),
		"oversized frame":     "{" + strings.Repeat(" ", maxReplyFrameBytes) + `"forgectl_reply":{"v":1,"in_reply_to":"input-1","body":"reply"}}`,
		"escaped duplicate":   `{"forgectl_reply":{"v":1,"in_reply_to":"input-1","body":"one","\u0062ody":"two"}}`,
		"case alias":          `{"forgectl_reply":{"v":1,"in_reply_to":"input-1","body":"one","BODY":"two"}}`,
		"second object":       `{"forgectl_reply":{"v":1,"in_reply_to":"input-1","body":"reply"}}{}`,
		"null body":           `{"forgectl_reply":{"v":1,"in_reply_to":"input-1","body":null}}`,
		"string version":      `{"forgectl_reply":{"v":"1","in_reply_to":"input-1","body":"reply"}}`,
		"nonintegral version": `{"forgectl_reply":{"v":1.5,"in_reply_to":"input-1","body":"reply"}}`,
	} {
		test.Run(name, func(test *testing.T) {
			if _, err := parseNativeReplyFrame(frame, "input-1"); err == nil {
				test.Fatal("invalid or oversized reply frame was accepted")
			}
		})
	}
}

func readyReplyForTest(test *testing.T) replyRecord {
	test.Helper()
	return replyRecord{
		ID: "input-1", SessionID: "session-1", RunID: "run-1", Text: "original mailbox input", Digest: digest("original mailbox input"),
		Prompt: "native prompt", PromptDigest: digest("native prompt"), SourceDigest: digest("mailbox source"),
		Recipient: "coordinator", RecipientRunID: "coordinator-run", Body: "native answer",
		Frame: replyFrameForTest(test, "input-1", "native answer"), State: replyReady,
	}
}

func TestNativeReplyRecoveryRefusesUnprovedAndReplacedTurns(test *testing.T) {
	for name, scenario := range map[string]struct {
		state, runID, sessionID, expectedState, expectedStatus string
		publications                                           int
	}{
		"intent":          {replyIntent, "run-1", "session-1", replyUnknown, "interrupted-before-ready", 0},
		"prepared":        {replyPrepared, "run-1", "session-1", replyUnknown, "interrupted-before-ready", 0},
		"ready":           {replyReady, "run-1", "session-1", replyPublished, "queued", 1},
		"replacement run": {replyReady, "run-2", "session-1", replyUnknown, "replacement-run-or-session", 0},
		"other session":   {replyReady, "run-1", "session-2", replyUnknown, "replacement-run-or-session", 0},
	} {
		test.Run(name, func(test *testing.T) {
			record := readyReplyForTest(test)
			record.State = scenario.state
			if record.State != replyReady {
				record.Body, record.Frame = "", ""
			}
			if record.State == replyIntent {
				record.Prompt, record.PromptDigest, record.SourceDigest, record.Recipient, record.RecipientRunID = "", "", "", "", ""
			}
			output := &hermesTestWriter{}
			publications := 0
			receiver := &supervisor{
				config: Config{NativeReplies: true, RunID: scenario.runID, PublishReply: func(_ context.Context, publication ReplyPublication) (ReplyPublicationResult, error) {
					publications++
					if publication.Input.RunID != record.RunID || publication.Input.SessionID != record.SessionID || publication.Input.ID != record.ID || publication.Body != record.Body {
						test.Fatalf("recovery changed original publication: %+v", publication)
					}
					return ReplyPublicationResult{MessageID: "outgoing-1", Status: "queued"}, nil
				}},
				sessionID: scenario.sessionID,
				replyPath: filepath.Join(test.TempDir(), "replies.jsonl"),
				replies:   map[string]replyRecord{},
			}
			receiver.events = eventWriter{writer: output, mu: &receiver.eventMu}
			if err := receiver.appendReplyLocked(record); err != nil {
				test.Fatal(err)
			}
			if err := receiver.recoverReplies(test.Context()); err != nil {
				test.Fatal(err)
			}
			if err := receiver.recoverReplies(test.Context()); err != nil {
				test.Fatal(err)
			}
			loaded, err := loadReplies(receiver.replyPath)
			if err != nil || loaded[record.ID].State != scenario.expectedState || loaded[record.ID].Status != scenario.expectedStatus || publications != scenario.publications {
				test.Fatalf("recovered state=%+v publications=%d error=%v", loaded[record.ID], publications, err)
			}
			events := hermesEvents(output, "hermes-native-reply")
			if len(events) != 1 || events[0]["state"] != scenario.expectedState || events[0]["status"] != scenario.expectedStatus {
				test.Fatalf("reply recovery is not visible exactly once: %+v", events)
			}
			if _, exposed := events[0]["body"]; exposed {
				test.Fatal("reply recovery event exposed body text")
			}
		})
	}
}

func TestNativeReplyFinalToolSegmentSurvivesJournalReload(test *testing.T) {
	record := readyReplyForTest(test)
	updates := []historyUpdate{
		{Kind: "agent_message_chunk", Text: "I will inspect the requested input."},
		{Kind: "tool_call"},
		{Kind: "tool_call_update"},
		{Kind: "agent_message_chunk", Text: record.Frame[:20]},
		{Kind: "agent_thought_chunk", Text: "not public reply text"},
		{Kind: "agent_message_chunk", Text: record.Frame[20:]},
	}
	record.Frame = liveReplyFrame(updates)
	path := filepath.Join(test.TempDir(), "replies.jsonl")
	receiver := &supervisor{replyPath: path, replies: map[string]replyRecord{}}
	if err := receiver.appendReplyLocked(record); err != nil {
		test.Fatal(err)
	}
	loaded, err := loadReplies(path)
	if err != nil || loaded[record.ID] != record {
		test.Fatalf("validated final frame did not survive journal recovery: %+v, %v", loaded[record.ID], err)
	}
}

func TestNativeReplyHistoryCollectorBounds(test *testing.T) {
	message := nativeMessage{envelope: nativeEnvelope{
		Method: "session/update",
		Params: json.RawMessage(`{"sessionId":"session-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"reply"}}}`),
	}}
	client := &nativeClient{}
	for name, existing := range map[string][]historyUpdate{
		"count": make([]historyUpdate, maxHistoryUpdate),
		"bytes": {{Size: maxHistoryBytes}},
	} {
		test.Run(name, func(test *testing.T) {
			originalCount := len(existing)
			if _, _, err := client.processMessage(message, 1, "session/prompt", &existing); err == nil || len(existing) != originalCount {
				test.Fatalf("history limit failed: count=%d want=%d err=%v", len(existing), originalCount, err)
			}
		})
	}
	var valid []historyUpdate
	if _, _, err := client.processMessage(message, 1, "session/prompt", &valid); err != nil || len(valid) != 1 {
		test.Fatalf("within-bound history update rejected: count=%d err=%v", len(valid), err)
	}
}

func TestNativeReplyCancelledPreparationCannotBecomeReady(test *testing.T) {
	ctx, cancel := context.WithCancel(test.Context())
	defer cancel()
	ready := readyReplyForTest(test)
	record := receipt{ID: ready.ID, SessionID: ready.SessionID, RunID: ready.RunID, Text: ready.Text, Digest: ready.Digest, State: "pending"}
	intent := replyRecord{ID: ready.ID, SessionID: ready.SessionID, RunID: ready.RunID, Text: ready.Text, Digest: ready.Digest, State: replyIntent}
	output := &hermesTestWriter{}
	publications := 0
	directory := test.TempDir()
	receiver := &supervisor{
		config: Config{
			NativeReplies: true, RunID: ready.RunID,
			PrepareReply: func(context.Context, Request) (ReplyPreparation, error) {
				cancel()
				return ReplyPreparation{Prompt: ready.Prompt, SourceDigest: ready.SourceDigest, Recipient: ready.Recipient, RecipientRunID: ready.RecipientRunID}, nil
			},
			PublishReply: func(context.Context, ReplyPublication) (ReplyPublicationResult, error) {
				publications++
				return ReplyPublicationResult{MessageID: "forbidden-publication", Status: "queued"}, nil
			},
		},
		native: &nativeClient{}, sessionID: ready.SessionID,
		receiptPath: filepath.Join(directory, "receipts.jsonl"), receipts: map[string]receipt{},
		replyPath: filepath.Join(directory, "replies.jsonl"), replies: map[string]replyRecord{},
	}
	receiver.events = eventWriter{writer: output, mu: &receiver.eventMu}
	if err := receiver.appendReplyLocked(intent); err != nil {
		test.Fatal(err)
	}
	if err := receiver.appendReceiptLocked(record); err != nil {
		test.Fatal(err)
	}
	receiver.forwards.Add(1)
	receiver.forward(ctx, record)
	loaded, err := loadReplies(receiver.replyPath)
	if err != nil || loaded[record.ID].State != replyUnknown || loaded[record.ID].Body != "" || publications != 0 {
		test.Fatalf("cancelled preparation became publishable: state=%+v publications=%d err=%v", loaded[record.ID], publications, err)
	}
	events := hermesEvents(output, "hermes-native-reply")
	if len(events) != 1 || events[0]["state"] != replyUnknown {
		test.Fatalf("cancelled reply did not retain visible unknown state: %+v", events)
	}
}
