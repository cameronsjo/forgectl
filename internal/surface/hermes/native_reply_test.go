//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package hermes

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeHistoryNotificationsFailClosed(test *testing.T) {
	client := &nativeClient{}
	for name, message := range map[string]nativeMessage{
		"malformed session update": {envelope: nativeEnvelope{Method: "session/update", Params: json.RawMessage(`{"sessionId":`)}},
		"unknown notification":     {envelope: nativeEnvelope{Method: "session/custom"}},
	} {
		test.Run(name, func(test *testing.T) {
			if _, _, err := client.processMessage(message, 1, "", nil); err == nil {
				test.Fatal("malformed or unknown notification was accepted")
			}
		})
	}
}

func TestNativeHistoryNotificationsAcceptStructuredToolReplay(test *testing.T) {
	client := &nativeClient{}
	updates := make([]historyUpdate, 0, 3)
	for _, raw := range []string{
		`{"sessionId":"session-1","update":{"sessionUpdate":"tool_call","kind":"read","title":"read: mailbox-fixture.jsonl","toolCallId":"call-1"}}`,
		`{"sessionId":"session-1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"Read mailbox-fixture.jsonl"}}]}}`,
		`{"sessionId":"session-1","update":{"sessionUpdate":"plan","entries":[{"content":"Ship it","status":"in_progress","priority":"high"}]}}`,
	} {
		message := nativeMessage{envelope: nativeEnvelope{Method: "session/update", Params: json.RawMessage(raw)}}
		if _, _, err := client.processMessage(message, 1, "session/load", &updates); err != nil {
			test.Fatalf("structured replay update rejected: %v", err)
		}
	}
	if len(updates) != 3 || updates[0].Kind != "tool_call" || updates[1].Kind != "tool_call_update" || updates[2].Kind != "plan" {
		test.Fatalf("structured replay updates = %#v", updates)
	}
	if updates[0].ContentType != "" || updates[1].ContentType != "structured" {
		test.Fatalf("structured replay content types = %#v", updates[:2])
	}
	for _, update := range updates[:2] {
		if update.MetadataDigest == "" || update.Size == 0 {
			test.Fatalf("structured replay metadata was not retained: %#v", update)
		}
	}
}

func TestNativeHistoryNotificationsRejectMalformedTextContent(test *testing.T) {
	client := &nativeClient{}
	for name, raw := range map[string]string{
		"array":        `{"sessionId":"session-1","update":{"sessionUpdate":"agent_message_chunk","content":[]}}`,
		"null":         `{"sessionId":"session-1","update":{"sessionUpdate":"user_message_chunk","content":null}}`,
		"image":        `{"sessionId":"session-1","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"image","text":"no"}}}`,
		"scalar":       `{"sessionId":"session-1","update":{"sessionUpdate":"agent_message_chunk","content":"reply"}}`,
		"missing text": `{"sessionId":"session-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text"}}}`,
		"null text":    `{"sessionId":"session-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":null}}}`,
	} {
		test.Run(name, func(test *testing.T) {
			message := nativeMessage{envelope: nativeEnvelope{Method: "session/update", Params: json.RawMessage(raw)}}
			if _, _, err := client.processMessage(message, 1, "session/load", nil); err == nil {
				test.Fatal("malformed text content was accepted")
			}
		})
	}
}

func TestNativeHistoryNotificationsRejectMalformedToolContent(test *testing.T) {
	for _, content := range []string{`{}`, `"text"`, `1`, `true`, `[null]`, `["text"]`, `[[]]`} {
		test.Run(content, func(test *testing.T) {
			raw := `{"sessionId":"session-1","update":{"sessionUpdate":"tool_call_update","content":` + content + `}}`
			message := nativeMessage{envelope: nativeEnvelope{Method: "session/update", Params: json.RawMessage(raw)}}
			client := &nativeClient{}
			if _, _, err := client.processMessage(message, 1, "session/load", nil); err == nil {
				test.Fatal("malformed tool content was accepted")
			}
		})
	}
}

func TestParseNativeReplyFrameStrict(test *testing.T) {
	frame := `{"forgectl_reply":{"v":1,"in_reply_to":"input-1","body":"reply"}}`
	body, err := parseNativeReplyFrame(frame, "input-1")
	if err != nil || body != "reply" {
		test.Fatalf("valid frame = %q, %v", body, err)
	}
	for _, invalid := range []string{
		` {"forgectl_reply":{"v":1,"in_reply_to":"input-1","body":"reply"}}`,
		`{"forgectl_reply":{"v":1,"in_reply_to":"input-1","body":"reply"}} `,
		`{"forgectl_reply":{"v":1,"in_reply_to":"input-1","body":"reply","body":"again"}}`,
		`{"forgectl_reply":{"v":1,"in_reply_to":"input-1","Body":"reply"}}`,
		`{"forgectl_reply":{"v":1,"in_reply_to":"input-2","body":"reply"}}`,
		`{"forgectl_reply":{"v":1,"in_reply_to":"input-1","body":"reply"},"extra":true}`,
		`{"forgectl_reply":{"v":1,"in_reply_to":"input-1","body":""}}`,
		`{"forgectl_reply":{"v":1,"in_reply_to":"input-1","body":"\ud800"}}`,
		`{"forgectl_reply":{"v":1,"in_reply_to":"input-1","body":"\udc00"}}`,
	} {
		if _, err := parseNativeReplyFrame(invalid, "input-1"); err == nil {
			test.Fatalf("invalid frame accepted: %s", invalid)
		}
	}
}

func TestProveNativeReplyRequiresLiveAndDurableAgreement(test *testing.T) {
	record := receipt{ID: "input-1", SessionID: "session-1", NativePrompt: "prompt"}
	old := historyUpdate{SessionID: "session-1", Kind: "user_message_chunk", Text: "old", ContentType: "text", MetadataDigest: "old"}
	user := historyUpdate{SessionID: "session-1", Kind: "user_message_chunk", Text: "prompt", ContentType: "text", MetadataDigest: "user"}
	tool := historyUpdate{SessionID: "session-1", Kind: "tool_call", ContentType: "tool", MetadataDigest: "tool"}
	frame := `{"forgectl_reply":{"v":1,"in_reply_to":"input-1","body":"reply"}}`
	assistant := historyUpdate{SessionID: "session-1", Kind: "agent_message_chunk", Text: frame, ContentType: "text", MetadataDigest: "assistant"}
	pre := []historyUpdate{old}
	post := []historyUpdate{old, user, tool, assistant}
	live := []historyUpdate{tool, assistant}
	preexistingPost := append([]historyUpdate{}, pre...)
	preexistingPost = append(preexistingPost, user)
	preexistingPost = append(preexistingPost, post[1:]...)
	body, err := proveNativeReply(record, pre, live, post, "end_turn", 0, 0)
	if err != nil || body != "reply" {
		test.Fatalf("valid proof = %q, %v", body, err)
	}
	for name, scenario := range map[string]struct {
		live        []historyUpdate
		post        []historyUpdate
		stopReason  string
		permissions uint64
		unsupported uint64
	}{
		"transformed": {live: []historyUpdate{tool, assistant, {SessionID: "session-1", Kind: "agent_message_chunk", Text: "different", ContentType: "text", MetadataDigest: "different"}}, post: post, stopReason: "end_turn"},
		"refusal":     {live: live, post: post, stopReason: "refusal"},
		"permission":  {live: live, post: post, stopReason: "end_turn", permissions: 1},
		"unsupported": {live: live, post: post, stopReason: "end_turn", unsupported: 1},
		"extra-user":  {live: live, post: append(post, user), stopReason: "end_turn"},
		"wrong-post-session": {
			live:       live,
			post:       append(post, historyUpdate{SessionID: "other-session", Kind: "agent_message_chunk", Text: frame, ContentType: "text", MetadataDigest: "other"}),
			stopReason: "end_turn",
		},
		"wrong-live-session": {
			live:       []historyUpdate{{SessionID: "other-session", Kind: "agent_message_chunk", Text: frame, ContentType: "text", MetadataDigest: "other"}},
			post:       post,
			stopReason: "end_turn",
		},
		"wrong-message-type": {
			live:       []historyUpdate{{SessionID: "session-1", Kind: "agent_message_chunk", Text: frame, ContentType: "image", MetadataDigest: "image"}},
			post:       post,
			stopReason: "end_turn",
		},
		"preexisting-input": {
			live:       live,
			post:       preexistingPost,
			stopReason: "end_turn",
		},
	} {
		test.Run(name, func(test *testing.T) {
			if _, err := proveNativeReply(record, pre, scenario.live, scenario.post, scenario.stopReason, scenario.permissions, scenario.unsupported); err == nil {
				test.Fatal("invalid native reply proof accepted")
			}
		})
	}
}

func TestProveNativeReplyCanonicalFrameRequiresExactToolPrefix(test *testing.T) {
	record := receipt{ID: "input-1", SessionID: "session-1", NativePrompt: "prompt"}
	old := historyUpdate{SessionID: record.SessionID, Kind: "user_message_chunk", Text: "old", ContentType: "text", MetadataDigest: "old"}
	user := historyUpdate{SessionID: record.SessionID, Kind: "user_message_chunk", Text: record.NativePrompt, ContentType: "text", MetadataDigest: "user"}
	tool := historyUpdate{SessionID: record.SessionID, Kind: "tool_call", ContentType: "tool", MetadataDigest: "tool"}
	frame := `{"forgectl_reply":{"v":1,"in_reply_to":"input-1","body":"reply"}}`
	pre := []historyUpdate{old}
	post := []historyUpdate{old, user, tool, {SessionID: record.SessionID, Kind: "agent_message_chunk", Text: frame, ContentType: "text", MetadataDigest: "assistant"}}
	withLiveText := func(text string, includeTool bool) []historyUpdate {
		updates := make([]historyUpdate, 0, 2)
		if includeTool {
			updates = append(updates, tool)
		}
		updates = append(updates, historyUpdate{SessionID: record.SessionID, Kind: "agent_message_chunk", Text: text, ContentType: "text", MetadataDigest: "assistant-live"})
		return updates
	}
	for name, scenario := range map[string]struct {
		live        []historyUpdate
		durable     []historyUpdate
		permissions uint64
		wantPrefix  int
	}{
		"exact two newlines after tool": {live: withLiveText("\n\n"+frame, true), wantPrefix: 2},
		"no tool":                       {live: withLiveText("\n\n"+frame, false)},
		"one newline":                   {live: withLiveText("\n"+frame, true)},
		"three newlines":                {live: withLiveText("\n\n\n"+frame, true)},
		"four newlines":                 {live: withLiveText("\n\n\n\n"+frame, true)},
		"space":                         {live: withLiveText(" "+frame, true)},
		"tab":                           {live: withLiveText("\t"+frame, true)},
		"durable leading whitespace": {
			live:    withLiveText(frame, true),
			durable: []historyUpdate{old, user, tool, {SessionID: record.SessionID, Kind: "agent_message_chunk", Text: " " + frame, ContentType: "text", MetadataDigest: "assistant"}},
		},
		"altered body": {live: withLiveText(replyFrameForTest(test, record.ID, "changed"), true)},
		"wrong input":  {live: withLiveText(replyFrameForTest(test, "input-2", "reply"), true)},
		"permission":   {live: withLiveText(frame, true), permissions: 1},
	} {
		test.Run(name, func(test *testing.T) {
			durable := scenario.durable
			if durable == nil {
				durable = post
			}
			proof, err := proveNativeReplyEvidence(record, pre, scenario.live, durable, "end_turn", scenario.permissions, 0)
			if scenario.wantPrefix == 0 {
				if err == nil {
					test.Fatalf("invalid canonical-frame proof accepted: body=%q frame=%q", proof.Body, proof.CanonicalFrame)
				}
				return
			}
			if err != nil {
				test.Fatalf("valid canonical-frame proof rejected: %v", err)
			}
			if proof.Body != "reply" || proof.CanonicalFrame != frame || proof.Diagnostics.FixedPrefixCount != scenario.wantPrefix {
				test.Fatalf("canonical proof = %#v", proof)
			}

			recordForJournal := readyReplyForTest(test)
			recordForJournal.Body = proof.Body
			recordForJournal.Frame = proof.CanonicalFrame
			path := filepath.Join(test.TempDir(), "replies.jsonl")
			receiver := &supervisor{
				config: Config{NativeReplies: true, RunID: recordForJournal.RunID, PublishReply: func(context.Context, ReplyPublication) (ReplyPublicationResult, error) {
					return ReplyPublicationResult{MessageID: "outgoing-1", Status: "queued"}, nil
				}},
				sessionID: recordForJournal.SessionID,
				replyPath: path,
				replies:   map[string]replyRecord{},
			}
			receiver.events = eventWriter{writer: &hermesTestWriter{}, mu: &receiver.eventMu}
			if err := receiver.appendReplyLocked(recordForJournal); err != nil {
				test.Fatal(err)
			}
			if err := receiver.recoverReplies(test.Context()); err != nil {
				test.Fatal(err)
			}
			loaded, err := loadReplies(path)
			if err != nil || loaded[recordForJournal.ID].Frame != frame || loaded[recordForJournal.ID].Body != "reply" {
				test.Fatalf("canonical frame did not survive recovery: record=%+v err=%v", loaded[recordForJournal.ID], err)
			}
		})
	}
}

func TestLoadRepliesRequiresMonotonicIdentityAndState(test *testing.T) {
	directory := test.TempDir()
	path := filepath.Join(directory, "replies.jsonl")
	base := replyRecord{ID: "input-1", SessionID: "session-1", RunID: "run-1", Text: "input", Digest: digest("input"), State: replyIntent}
	write := func(record replyRecord) {
		test.Helper()
		receiver := &supervisor{replyPath: path, replies: map[string]replyRecord{}}
		if err := receiver.appendReplyLocked(record); err != nil {
			test.Fatal(err)
		}
	}
	write(base)
	loaded, err := loadReplies(path)
	if err != nil || loaded[base.ID].State != replyIntent {
		test.Fatalf("loaded intent = %#v, %v", loaded, err)
	}
	prepared := base
	prepared.State = replyPrepared
	prepared.Prompt = "prompt"
	prepared.PromptDigest = digest(prepared.Prompt)
	prepared.SourceDigest = digest("source")
	prepared.Recipient = "worker"
	prepared.RecipientRunID = "worker-run"
	write(prepared)
	if loaded, err = loadReplies(path); err != nil || loaded[base.ID].State != replyPrepared {
		test.Fatalf("loaded prepared = %#v, %v", loaded, err)
	}
	conflict := prepared
	conflict.Text = strings.Repeat("x", 3)
	write(conflict)
	if _, err := loadReplies(path); err == nil {
		test.Fatal("conflicting identity accepted")
	}
}

func TestReplyJournalRejectsAmbiguousAndUnsafeJSON(test *testing.T) {
	for name, data := range map[string][]byte{
		"case-folded duplicate": []byte(`{"id":"input-1","ID":"input-1"}`),
		"invalid UTF-8":         []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'},
		"unpaired surrogate":    []byte(`{"x":"\ud800"}`),
	} {
		test.Run(name, func(test *testing.T) {
			if err := scanReplyJSON(bytes.NewReader(data)); err == nil {
				test.Fatal("unsafe JSON was accepted")
			}
		})
	}
	deep := bytes.Repeat([]byte{'['}, maxReplyJSONDepth+2)
	deep = append(deep, bytes.Repeat([]byte{']'}, maxReplyJSONDepth+2)...)
	if err := scanReplyJSON(bytes.NewReader(deep)); err == nil {
		test.Fatal("deeply nested JSON was accepted")
	}
}
