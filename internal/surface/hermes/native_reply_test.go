//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package hermes

import (
	"bytes"
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
