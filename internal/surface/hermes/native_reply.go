//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package hermes

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	maxReplyFrameBytes = 256 << 10
	maxReplyBodyBytes  = 32 << 10
	maxReplyJSONDepth  = 128
)

const (
	replyIntent    = "intent"
	replyPrepared  = "prepared"
	replyReady     = "ready"
	replyPublished = "published"
	replyUnknown   = "unknown"
)

func requestForReceipt(record receipt) Request {
	return Request{V: 1, Type: "deliver", ID: record.ID, RunID: record.RunID, SessionID: record.SessionID, Text: record.Text}
}

func validateReplyPreparation(preparation ReplyPreparation) error {
	if preparation.Prompt == "" || len(preparation.Prompt) > maxLine {
		return errors.New("native reply prompt is empty or too large")
	}
	if !validDigest(preparation.SourceDigest) {
		return errors.New("native reply source digest is invalid")
	}
	if !validToken(preparation.Recipient) || !validToken(preparation.RecipientRunID) {
		return errors.New("native reply recipient identity is invalid")
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func metadataDigest(raw []byte) string {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return digest(string(raw))
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return digest(string(raw))
	}
	return digest(string(canonical))
}

func (receiver *supervisor) prepareNativeReply(ctx context.Context, record receipt) (ReplyPreparation, error) {
	if !receiver.config.NativeReplies || receiver.config.PrepareReply == nil {
		return ReplyPreparation{}, errors.New("native replies are not enabled")
	}
	preparation, err := receiver.config.PrepareReply(ctx, requestForReceipt(record))
	if err != nil {
		return ReplyPreparation{}, err
	}
	if err := validateReplyPreparation(preparation); err != nil {
		return ReplyPreparation{}, err
	}
	return preparation, nil
}

func parseStrictObject(raw []byte, fields map[string]json.RawMessage) error {
	if len(raw) == 0 || raw[0] != '{' || raw[len(raw)-1] != '}' || !utf8.Valid(raw) {
		return errors.New("value is not an exact JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return errors.New("value is not a JSON object")
	}
	seen := make(map[string]struct{}, len(fields))
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("JSON object key is not a string")
		}
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate JSON field %q", key)
		}
		seen[key] = struct{}{}
		if _, ok := fields[key]; !ok {
			return fmt.Errorf("unknown JSON field %q", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		fields[key] = value
	}
	token, err = decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '}' {
		return errors.New("JSON object did not close")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func parseNativeReplyFrame(frame, inputID string) (string, error) {
	if len(frame) == 0 || len(frame) > maxReplyFrameBytes || frame[0] != '{' || frame[len(frame)-1] != '}' {
		return "", errors.New("native reply frame has invalid size or delimiters")
	}
	if strings.TrimSpace(frame) != frame || !utf8.ValidString(frame) {
		return "", errors.New("native reply frame has surrounding whitespace or invalid UTF-8")
	}
	outer := map[string]json.RawMessage{"forgectl_reply": nil}
	if err := parseStrictObject([]byte(frame), outer); err != nil {
		return "", err
	}
	if outer["forgectl_reply"] == nil {
		return "", errors.New("native reply frame lacks forgectl_reply")
	}
	inner := map[string]json.RawMessage{"v": nil, "in_reply_to": nil, "body": nil}
	if err := parseStrictObject(outer["forgectl_reply"], inner); err != nil {
		return "", err
	}
	var version int
	if err := json.Unmarshal(inner["v"], &version); err != nil || version != 1 {
		return "", errors.New("native reply frame has unsupported version")
	}
	var replyID string
	if err := json.Unmarshal(inner["in_reply_to"], &replyID); err != nil || replyID != inputID {
		return "", errors.New("native reply frame has the wrong input id")
	}
	body, err := strictJSONString(inner["body"])
	if err != nil || !utf8.ValidString(body) || len(body) == 0 || len(body) > maxReplyBodyBytes {
		return "", errors.New("native reply frame body is empty, oversized, or invalid UTF-8")
	}
	return body, nil
}

func strictJSONString(raw []byte) (string, error) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' || !utf8.Valid(raw) {
		return "", errors.New("value is not a valid JSON string")
	}
	for index := 1; index < len(raw)-1; {
		if raw[index] == '\\' {
			if index+1 >= len(raw)-1 {
				return "", errors.New("JSON string has a truncated escape")
			}
			if raw[index+1] != 'u' {
				index += 2
				continue
			}
			if index+6 >= len(raw) {
				return "", errors.New("JSON string has a truncated Unicode escape")
			}
			code, err := parseHex16(raw[index+2 : index+6])
			if err != nil {
				return "", err
			}
			if code >= 0xdc00 && code <= 0xdfff {
				return "", errors.New("JSON string has an unpaired low surrogate")
			}
			if code >= 0xd800 && code <= 0xdbff {
				if index+12 >= len(raw) || raw[index+6] != '\\' || raw[index+7] != 'u' {
					return "", errors.New("JSON string has an unpaired high surrogate")
				}
				low, err := parseHex16(raw[index+8 : index+12])
				if err != nil || low < 0xdc00 || low > 0xdfff {
					return "", errors.New("JSON string has an invalid surrogate pair")
				}
				index += 12
				continue
			}
			index += 6
			continue
		}
		if raw[index] < 0x20 {
			return "", errors.New("JSON string contains a control character")
		}
		_, size := utf8.DecodeRune(raw[index:])
		if size == 1 && raw[index] >= utf8.RuneSelf {
			return "", errors.New("JSON string contains invalid UTF-8")
		}
		index += size
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

func parseHex16(value []byte) (uint16, error) {
	if len(value) != 4 {
		return 0, errors.New("Unicode escape has the wrong length")
	}
	var result uint16
	for _, character := range value {
		result <<= 4
		switch {
		case character >= '0' && character <= '9':
			result += uint16(character - '0')
		case character >= 'a' && character <= 'f':
			result += uint16(character-'a') + 10
		case character >= 'A' && character <= 'F':
			result += uint16(character-'A') + 10
		default:
			return 0, errors.New("Unicode escape is not hexadecimal")
		}
	}
	return result, nil
}

func sameHistoryUpdate(left, right historyUpdate) bool {
	return left.SessionID == right.SessionID && left.Kind == right.Kind && left.Text == right.Text && left.ContentType == right.ContentType && left.MetadataDigest == right.MetadataDigest
}

func historyPrefix(prefix, full []historyUpdate) bool {
	if len(full) < len(prefix) {
		return false
	}
	for index, update := range prefix {
		if !sameHistoryUpdate(update, full[index]) {
			return false
		}
	}
	return true
}

func replyHistoryUpdates(updates []historyUpdate, sessionID string) ([]historyUpdate, error) {
	if sessionID == "" {
		return nil, errors.New("native reply history lacks a session id")
	}
	result := make([]historyUpdate, 0, len(updates))
	for _, update := range updates {
		if update.SessionID != sessionID {
			return nil, errors.New("native reply history contains another session")
		}
		switch update.Kind {
		case "user_message_chunk", "agent_message_chunk", "agent_thought_chunk":
			if update.ContentType != "text" {
				return nil, errors.New("native reply history contains a non-text message chunk")
			}
			result = append(result, update)
		case "tool_call", "tool_call_update", "plan":
			result = append(result, update)
		case "usage_update", "session_info_update", "available_commands_update":
		default:
			return nil, fmt.Errorf("native reply history contains unknown update %q", update.Kind)
		}
	}
	return result, nil
}

func replyHistoryBody(updates []historyUpdate, nativePrompt string) (string, error) {
	users := 0
	assistants := 0
	body := ""
	seenUser := false
	toolSeen := false
	for _, update := range updates {
		switch update.Kind {
		case "user_message_chunk":
			users++
			if users != 1 || update.Text != nativePrompt {
				return "", errors.New("native reply history has an ambiguous user turn")
			}
			seenUser = true
		case "agent_message_chunk":
			if !seenUser {
				return "", errors.New("native reply history has an ambiguous assistant turn")
			}
			if !toolSeen && assistants != 0 {
				return "", errors.New("native reply history has an ambiguous assistant turn")
			}
			assistants++
			body += update.Text
		case "agent_thought_chunk":
		case "tool_call", "tool_call_update", "plan":
			toolSeen = true
			assistants = 0
			body = ""
		default:
			return "", fmt.Errorf("native reply history contains unsupported update %q", update.Kind)
		}
	}
	if users != 1 || assistants != 1 {
		return "", errors.New("native reply history lacks one complete user and assistant turn")
	}
	return body, nil
}

func replyLiveBody(updates []historyUpdate, nativePrompt string) (string, error) {
	assistants := 0
	users := 0
	body := ""
	started := false
	for _, update := range updates {
		switch update.Kind {
		case "user_message_chunk":
			users++
			if users != 1 || started || update.Text != nativePrompt {
				return "", errors.New("native reply live updates contain a second user turn")
			}
		case "agent_thought_chunk":
		case "agent_message_chunk":
			started = true
			assistants++
			body += update.Text
		case "tool_call", "tool_call_update", "plan":
			started = false
			assistants = 0
			body = ""
		case "usage_update", "session_info_update", "available_commands_update":
		default:
			return "", fmt.Errorf("native reply live updates contain unsupported update %q", update.Kind)
		}
	}
	if assistants == 0 || len(body) == 0 {
		return "", errors.New("native reply live updates lack an assistant reply")
	}
	return body, nil
}

func liveReplyFrame(updates []historyUpdate) string {
	var builder strings.Builder
	for _, update := range updates {
		if update.Kind == "tool_call" || update.Kind == "tool_call_update" || update.Kind == "plan" {
			builder.Reset()
		} else if update.Kind == "agent_message_chunk" {
			builder.WriteString(update.Text)
		}
	}
	return builder.String()
}

func proveNativeReply(record receipt, pre, live, post []historyUpdate, stopReason string, permissionCount, unsupportedCount uint64) (string, error) {
	if stopReason != "end_turn" {
		return "", fmt.Errorf("native reply stop reason %q is not complete", stopReason)
	}
	if permissionCount != 0 {
		return "", errors.New("native reply turn requested permission")
	}
	if unsupportedCount != 0 {
		return "", errors.New("native reply turn requested an unsupported client operation")
	}
	relevantPre, err := replyHistoryUpdates(pre, record.SessionID)
	if err != nil {
		return "", err
	}
	relevantPost, err := replyHistoryUpdates(post, record.SessionID)
	if err != nil {
		return "", err
	}
	relevantLive, err := replyHistoryUpdates(live, record.SessionID)
	if err != nil {
		return "", err
	}
	if !historyPrefix(relevantPre, relevantPost) {
		return "", errors.New("native reply durable history changed before the turn")
	}
	matchingUsers := 0
	for _, update := range relevantPost {
		if update.Kind == "user_message_chunk" && update.Text == record.NativePrompt {
			matchingUsers++
		}
	}
	if matchingUsers != 1 {
		return "", errors.New("native reply durable history has an ambiguous input turn")
	}
	durable, err := replyHistoryBody(relevantPost[len(relevantPre):], record.NativePrompt)
	if err != nil {
		return "", err
	}
	liveBody, err := replyLiveBody(relevantLive, record.NativePrompt)
	if err != nil {
		return "", err
	}
	if liveBody != durable {
		return "", errors.New("native reply live and durable assistant text disagree")
	}
	return parseNativeReplyFrame(liveBody, record.ID)
}

func (receiver *supervisor) markReplyUnknownLocked(id, reason string) error {
	record, ok := receiver.replies[id]
	if !ok || record.State == replyPublished || record.State == replyUnknown {
		return nil
	}
	record.State = replyUnknown
	record.Status = reason
	if err := receiver.appendReplyLocked(record); err != nil {
		return err
	}
	receiver.replies[id] = record
	receiver.events.write(map[string]any{
		"type": "hermes-native-reply", "message_id": record.ID, "session_id": record.SessionID,
		"run_id": record.RunID, "state": replyUnknown, "status": reason, "error": true,
	})
	return nil
}

func (receiver *supervisor) publishReady(ctx context.Context, record replyRecord) error {
	if record.State != replyReady {
		return errors.New("native reply is not ready")
	}
	publication := ReplyPublication{Input: Request{V: 1, Type: "deliver", ID: record.ID, RunID: record.RunID, SessionID: record.SessionID, Text: record.Text}, Prepared: ReplyPreparation{Prompt: record.Prompt, SourceDigest: record.SourceDigest, Recipient: record.Recipient, RecipientRunID: record.RecipientRunID}, Body: record.Body}
	result, err := receiver.config.PublishReply(ctx, publication)
	if err != nil {
		receiver.events.write(map[string]any{"type": "hermes-native-reply", "message_id": record.ID, "session_id": record.SessionID, "run_id": record.RunID, "state": replyReady, "error": true})
		return err
	}
	if result.MessageID == "" || result.Status == "" || !validToken(result.MessageID) || !validToken(result.Status) {
		receiver.events.write(map[string]any{"type": "hermes-native-reply", "message_id": record.ID, "session_id": record.SessionID, "run_id": record.RunID, "state": replyReady, "error": true})
		return errors.New("native reply publication result is invalid")
	}
	receiver.mu.Lock()
	current, ok := receiver.replies[record.ID]
	if !ok || current.State != replyReady || current.Frame != record.Frame || current.Body != record.Body {
		receiver.mu.Unlock()
		return errors.New("native reply was changed while publishing")
	}
	current.State, current.MessageID, current.Status = replyPublished, result.MessageID, result.Status
	err = receiver.appendReplyLocked(current)
	if err == nil {
		receiver.replies[record.ID] = current
	}
	receiver.mu.Unlock()
	if err != nil {
		return err
	}
	receiver.events.write(map[string]any{"type": "hermes-native-reply", "message_id": record.ID, "session_id": record.SessionID, "run_id": record.RunID, "state": replyPublished, "outgoing_id": result.MessageID, "status": result.Status, "body_sha256": digest(record.Body), "body_bytes": len(record.Body)})
	return nil
}

func (receiver *supervisor) recoverReplies(ctx context.Context) error {
	if !receiver.config.NativeReplies {
		return nil
	}
	var ready []replyRecord
	receiver.mu.Lock()
	ids := make([]string, 0, len(receiver.replies))
	for id := range receiver.replies {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		record := receiver.replies[id]
		if record.State != replyIntent && record.State != replyPrepared && record.State != replyReady {
			continue
		}
		if record.RunID != receiver.config.RunID || record.SessionID != receiver.sessionID {
			if err := receiver.markReplyUnknownLocked(id, "replacement-run-or-session"); err != nil {
				receiver.mu.Unlock()
				return err
			}
			continue
		}
		if record.State == replyReady {
			ready = append(ready, record)
			continue
		}
		if err := receiver.markReplyUnknownLocked(id, "interrupted-before-ready"); err != nil {
			receiver.mu.Unlock()
			return err
		}
	}
	receiver.mu.Unlock()
	for _, record := range ready {
		if err := receiver.publishReady(ctx, record); err != nil {
			continue
		}
	}
	return nil
}

func (receiver *supervisor) appendReplyLocked(record replyRecord) error {
	if receiver.replyPath == "" {
		return errors.New("native reply journal is not configured")
	}
	file, err := openLedgerFile(receiver.replyPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	receiver.replies[record.ID] = record
	return nil
}

func loadReplies(path string) (map[string]replyRecord, error) {
	file, err := openLedgerFile(path, os.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]replyRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	result := map[string]replyRecord{}
	scanner := newBoundedScanner(file)
	for scanner.Scan() {
		var record replyRecord
		if err := decodeReplyRecord(scanner.Bytes(), &record); err != nil {
			return nil, errors.New("native reply journal is invalid")
		}
		if err := validateReplyRecord(record); err != nil {
			return nil, err
		}
		if previous, ok := result[record.ID]; ok {
			if !sameReplyIdentity(previous, record) || !replyTransitionAllowed(previous.State, record.State) {
				return nil, errors.New("native reply journal contains conflicting records")
			}
			if previous.Prompt != "" && (previous.Prompt != record.Prompt || previous.SourceDigest != record.SourceDigest || previous.Recipient != record.Recipient || previous.RecipientRunID != record.RecipientRunID) {
				return nil, errors.New("native reply journal changed prepared routing")
			}
			if previous.Frame != "" && (previous.Frame != record.Frame || previous.Body != record.Body) {
				return nil, errors.New("native reply journal changed ready bytes")
			}
			if previous.State == record.State && !sameReplyRecord(previous, record) {
				return nil, errors.New("native reply journal contains conflicting duplicate records")
			}
		}
		result[record.ID] = record
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func decodeReplyRecord(data []byte, record *replyRecord) error {
	if err := scanReplyJSON(bytes.NewReader(data)); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(record); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func scanReplyJSON(reader io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(reader, maxReplyFrameBytes+maxLine+1))
	if err != nil {
		return err
	}
	if len(data) > maxReplyFrameBytes+maxLine {
		return errors.New("reply journal record is too large")
	}
	if err := validateReplyJSONLexemes(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanReplyJSONValue(decoder, 0); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func validateReplyJSONLexemes(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("reply journal JSON is not valid UTF-8")
	}
	for index := 0; index < len(data); index++ {
		if data[index] != '"' {
			continue
		}
		index++
		for index < len(data) {
			switch data[index] {
			case '"':
				goto stringComplete
			case '\\':
				if index+1 >= len(data) {
					return errors.New("reply journal JSON has a truncated escape")
				}
				switch data[index+1] {
				case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
					index += 2
				case 'u':
					if index+6 > len(data) {
						return errors.New("reply journal JSON has a truncated Unicode escape")
					}
					code, err := parseHex16(data[index+2 : index+6])
					if err != nil {
						return err
					}
					if code >= 0xdc00 && code <= 0xdfff {
						return errors.New("reply journal JSON has an unpaired low surrogate")
					}
					if code >= 0xd800 && code <= 0xdbff {
						if index+12 > len(data) || data[index+6] != '\\' || data[index+7] != 'u' {
							return errors.New("reply journal JSON has an unpaired high surrogate")
						}
						low, err := parseHex16(data[index+8 : index+12])
						if err != nil || low < 0xdc00 || low > 0xdfff {
							return errors.New("reply journal JSON has an invalid surrogate pair")
						}
						index += 12
					} else {
						index += 6
					}
				default:
					return errors.New("reply journal JSON has an invalid escape")
				}
			default:
				if data[index] < 0x20 {
					return errors.New("reply journal JSON contains a control character")
				}
				_, size := utf8.DecodeRune(data[index:])
				if size == 0 {
					return errors.New("reply journal JSON contains an invalid rune")
				}
				index += size
			}
		}
		return errors.New("reply journal JSON has an unterminated string")
	stringComplete:
	}
	return nil
}

func scanReplyJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxReplyJSONDepth {
		return errors.New("reply journal nesting exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("reply journal object key is not a string")
			}
			name = strings.ToLower(name)
			if _, ok := seen[name]; ok {
				return fmt.Errorf("duplicate reply journal field %q", name)
			}
			seen[name] = struct{}{}
			if err := scanReplyJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := scanReplyJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("unexpected reply journal delimiter")
	}
}

func newBoundedScanner(file *os.File) *bufio.Scanner {
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), maxReplyFrameBytes+maxLine)
	return scanner
}

func validateReplyRecord(record replyRecord) error {
	if record.ID == "" || record.SessionID == "" || record.RunID == "" || record.Text == "" || record.Digest != digest(record.Text) {
		return errors.New("native reply journal has invalid identity")
	}
	switch record.State {
	case replyIntent:
		return nil
	case replyPrepared:
		if err := validateReplyPreparation(ReplyPreparation{Prompt: record.Prompt, SourceDigest: record.SourceDigest, Recipient: record.Recipient, RecipientRunID: record.RecipientRunID}); err != nil {
			return err
		}
		if record.PromptDigest != digest(record.Prompt) {
			return errors.New("native reply journal has an invalid prompt digest")
		}
	case replyReady:
		if err := validateReplyPreparation(ReplyPreparation{Prompt: record.Prompt, SourceDigest: record.SourceDigest, Recipient: record.Recipient, RecipientRunID: record.RecipientRunID}); err != nil {
			return err
		}
		if record.PromptDigest != digest(record.Prompt) {
			return errors.New("native reply journal has an invalid prompt digest")
		}
		body, err := parseNativeReplyFrame(record.Frame, record.ID)
		if err != nil || body != record.Body {
			return errors.New("native reply journal has invalid ready bytes")
		}
	case replyPublished:
		if err := validateReplyPreparation(ReplyPreparation{Prompt: record.Prompt, SourceDigest: record.SourceDigest, Recipient: record.Recipient, RecipientRunID: record.RecipientRunID}); err != nil {
			return err
		}
		if record.PromptDigest != digest(record.Prompt) || record.MessageID == "" || record.Status == "" || !validToken(record.MessageID) || !validToken(record.Status) {
			return errors.New("native reply journal has invalid publication")
		}
	case replyUnknown:
		if record.Status == "" || !validToken(record.Status) {
			return errors.New("native reply journal has invalid unknown status")
		}
	default:
		return errors.New("native reply journal has unknown state")
	}
	return nil
}

func sameReplyIdentity(left, right replyRecord) bool {
	return left.ID == right.ID && left.SessionID == right.SessionID && left.RunID == right.RunID && left.Digest == right.Digest && left.Text == right.Text
}

func sameReplyRecord(left, right replyRecord) bool {
	return left == right
}

func replyTransitionAllowed(from, to string) bool {
	if from == to {
		return true
	}
	if to == replyUnknown && from != replyPublished && from != replyUnknown {
		return true
	}
	switch from {
	case replyIntent:
		return to == replyPrepared
	case replyPrepared:
		return to == replyReady
	case replyReady:
		return to == replyPublished
	default:
		return false
	}
}
