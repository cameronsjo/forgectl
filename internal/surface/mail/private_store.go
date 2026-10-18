package mail

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/termsafe"
)

var (
	ErrNativeReplyUnavailable = errors.New("native replies require a private mailbox")
	ErrNativeReplyConflict    = errors.New("native reply conflicts with an existing publication")
	ErrNativeReplyStale       = errors.New("native reply source is stale or no longer enrolled")
	ErrNativeReplyCorrupt     = errors.New("native reply source mailbox is corrupt")
)

type NativeReplySource struct {
	InputID        string
	InputDigest    string
	Recipient      string
	RecipientRunID string
	SessionID      string
	RunID          string
	Prompt         string
}

type privateMailbox struct {
	dirFD int
}

func decodePrivateRecord(data []byte, target *record) error {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	if err := validatePrivateJSONBytes(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(decoder, 0); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func validatePrivateJSONBytes(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("private JSON is not valid UTF-8")
	}
	inString := false
	for index := 0; index < len(data); index++ {
		if !inString {
			if data[index] == '"' {
				inString = true
			}
			continue
		}
		switch data[index] {
		case '"':
			inString = false
		case '\\':
			if index+1 >= len(data) {
				continue
			}
			if data[index+1] != 'u' {
				index++
				continue
			}
			if index+5 >= len(data) {
				return errors.New("private JSON contains an invalid Unicode escape")
			}
			code, ok := decodeHex16(data[index+2 : index+6])
			if !ok {
				return errors.New("private JSON contains an invalid Unicode escape")
			}
			switch {
			case code >= 0xd800 && code <= 0xdbff:
				if index+11 >= len(data) || data[index+6] != '\\' || data[index+7] != 'u' {
					return errors.New("private JSON contains an unpaired high surrogate")
				}
				low, valid := decodeHex16(data[index+8 : index+12])
				if !valid || low < 0xdc00 || low > 0xdfff {
					return errors.New("private JSON contains an unpaired high surrogate")
				}
				index += 11
			case code >= 0xdc00 && code <= 0xdfff:
				return errors.New("private JSON contains an unpaired low surrogate")
			default:
				index += 5
			}
		}
	}
	return nil
}

func decodeHex16(value []byte) (uint16, bool) {
	if len(value) != 4 {
		return 0, false
	}
	var result uint16
	for _, digit := range value {
		result <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			result |= uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			result |= uint16(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			result |= uint16(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return result, true
}

const maxPrivateJSONDepth = 128

func scanJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxPrivateJSONDepth {
		return errors.New("private JSON nesting exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			normalized := strings.ToLower(name)
			if _, exists := seen[normalized]; exists {
				return fmt.Errorf("duplicate JSON key %q", name)
			}
			seen[normalized] = struct{}{}
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("unexpected JSON delimiter")
	}
}

func decodePrivateRoster(data []byte, target *rosterFile) error {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	if target.V != 1 {
		return errors.New("unsupported roster version")
	}
	seen := make(map[string]struct{}, len(target.Workers))
	for _, worker := range target.Workers {
		if _, exists := seen[worker.Name]; exists {
			return fmt.Errorf("duplicate roster worker %q", worker.Name)
		}
		seen[worker.Name] = struct{}{}
		if err := validateRosterWorker(worker); err != nil {
			return err
		}
	}
	return nil
}

func validateRosterWorker(worker Worker) error {
	if err := ValidateName(worker.Name); err != nil {
		return err
	}
	switch worker.Harness {
	case HarnessClaude, HarnessCodex, HarnessHermes, HarnessPi, HarnessPane:
	default:
		return fmt.Errorf("unknown worker harness %q", worker.Harness)
	}
	if worker.RunID != "" && !threadPattern.MatchString(worker.RunID) {
		return errors.New("invalid worker run identity")
	}
	if worker.ThreadID != "" && !threadPattern.MatchString(worker.ThreadID) {
		return errors.New("invalid worker session identity")
	}
	if worker.Worktree != "" && (!filepath.IsAbs(worker.Worktree) || filepath.Clean(worker.Worktree) != worker.Worktree) {
		return errors.New("invalid worker worktree")
	}
	if worker.Socket != "" && (!filepath.IsAbs(worker.Socket) || filepath.Clean(worker.Socket) != worker.Socket) {
		return errors.New("invalid worker socket")
	}
	switch worker.State {
	case "", StateUnknown, StateAbsent, StateIdle, StateBusy, StateWaiting:
	default:
		return errors.New("invalid worker state")
	}
	return nil
}

func (transaction *Tx) openLogRead() (*os.File, error) {
	if transaction.private != nil {
		return transaction.private.openRead("mailbox.jsonl")
	}
	return os.Open(transaction.box.logPath())
}

func (transaction *Tx) openLogAppend() (*os.File, error) {
	if transaction.private != nil {
		return transaction.private.openAppend("mailbox.jsonl")
	}
	return os.OpenFile(transaction.box.logPath(), os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
}

func (service *Service) PrepareNativeReply(ctx context.Context, expected Worker, inputID, originalRenderedText string) (NativeReplySource, error) {
	if err := contextErr(ctx); err != nil {
		return NativeReplySource{}, err
	}
	if err := validateNativeWorker(expected); err != nil {
		return NativeReplySource{}, err
	}
	if inputID == "" || originalRenderedText == "" {
		return NativeReplySource{}, errors.New("native reply input id and original render are required")
	}
	if err := service.validateNativeStore(); err != nil {
		return NativeReplySource{}, err
	}
	var source NativeReplySource
	err := service.Box.Locked(func(transaction *Tx) error {
		entries, err := transaction.Load()
		if err != nil {
			return err
		}
		if transaction.Skipped != 0 {
			return fmt.Errorf("%w: %d skipped records", ErrNativeReplyCorrupt, transaction.Skipped)
		}
		actual, err := service.workerForTx(transaction, expected.Name)
		if err != nil {
			return fmt.Errorf("native reply recipient: %w", err)
		}
		if !sameWorkerIdentity(actual, expected) {
			return ErrNativeReplyStale
		}
		entry, err := findNativeInput(entries, inputID)
		if err != nil {
			return err
		}
		if err := validateNativeInput(service, transaction, expected, entry, originalRenderedText); err != nil {
			return err
		}
		digest := nativeMessageDigest(entry.Msg)
		source = NativeReplySource{
			InputID:        entry.Msg.ID,
			InputDigest:    digest,
			Recipient:      entry.Msg.From,
			RecipientRunID: entry.Msg.FromRunID,
			SessionID:      expected.ThreadID,
			RunID:          expected.RunID,
			Prompt:         RenderNative(entry.Msg),
		}
		return nil
	})
	if err != nil {
		return NativeReplySource{}, err
	}
	return source, nil
}

func (service *Service) EnqueueNativeReply(ctx context.Context, expected Worker, source NativeReplySource, body string) (SendResult, error) {
	if err := contextErr(ctx); err != nil {
		return SendResult{}, err
	}
	if err := validateNativeWorker(expected); err != nil {
		return SendResult{}, err
	}
	if err := service.validateNativeStore(); err != nil {
		return SendResult{}, err
	}
	if err := ValidateReplyBinary(service.ReplyBinary); err != nil {
		return SendResult{}, err
	}
	if err := validateNativeSource(source, expected); err != nil {
		return SendResult{}, err
	}
	clean, err := CleanBody(body)
	if err != nil {
		return SendResult{}, err
	}
	if clean != body {
		return SendResult{}, errors.New("native reply body changes under mailbox cleaning")
	}
	var result SendResult
	err = service.Box.Locked(func(transaction *Tx) error {
		entries, err := transaction.Load()
		if err != nil {
			return err
		}
		if transaction.Skipped != 0 {
			return fmt.Errorf("%w: %d skipped records", ErrNativeReplyCorrupt, transaction.Skipped)
		}
		actual, err := service.workerForTx(transaction, expected.Name)
		if err != nil {
			return fmt.Errorf("native reply sender: %w", err)
		}
		if !sameWorkerIdentity(actual, expected) {
			return ErrNativeReplyStale
		}
		entry, err := findNativeInput(entries, source.InputID)
		if err != nil {
			return err
		}
		if err := validateNativeInput(service, transaction, expected, entry, Render(entry.Msg)); err != nil {
			return err
		}
		if got := nativeMessageDigest(entry.Msg); got != source.InputDigest {
			return ErrNativeReplyStale
		}
		if source.Recipient != entry.Msg.From || source.RecipientRunID != entry.Msg.FromRunID {
			return ErrNativeReplyStale
		}
		if RenderNative(entry.Msg) != source.Prompt {
			return ErrNativeReplyStale
		}
		id := nativeReplyID(source.InputID, source.SessionID, source.RunID)
		if existing, found := findNativeReply(entries, id); found {
			if !sameNativeReply(existing.Msg, expected, source, entry.Msg, clean, service.ReplyBinary) {
				return fmt.Errorf("%w: %s", ErrNativeReplyConflict, quoteTrunc(id))
			}
			result = SendResult{ID: existing.Msg.ID, Status: existing.Status, Detail: existing.Detail}
			return nil
		}
		for _, existing := range entries {
			if existing.Msg.ID == id {
				return fmt.Errorf("%w: message id %s is already in use", ErrNativeReplyConflict, quoteTrunc(id))
			}
		}
		now := service.now()
		recipient, err := service.workerForTx(transaction, entry.Msg.From)
		if err != nil {
			return fmt.Errorf("native reply destination: %w", err)
		}
		if err := service.Policy.Check(expected, recipient, clean, entries, now); err != nil {
			return err
		}
		msg := Message{
			V:                 1,
			ID:                id,
			From:              expected.Name,
			To:                entry.Msg.From,
			FromRunID:         expected.RunID,
			ToRunID:           entry.Msg.FromRunID,
			Body:              clean,
			Priority:          PriorityNext,
			CreatedAt:         now,
			ReplyBinary:       service.ReplyBinary,
			ReplyTo:           source.InputID,
			ReplySessionID:    source.SessionID,
			ReplySourceDigest: source.InputDigest,
		}
		if err := transaction.Enqueue(msg, now); err != nil {
			return err
		}
		result = SendResult{ID: msg.ID, Status: StatusQueued}
		return nil
	})
	return result, err
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func validateNativeWorker(worker Worker) error {
	if err := ValidateName(worker.Name); err != nil {
		return err
	}
	if worker.Harness != HarnessHermes || worker.RunID == "" || !threadPattern.MatchString(worker.RunID) || worker.ThreadID == "" || !threadPattern.MatchString(worker.ThreadID) {
		return errors.New("native reply worker lacks a valid Hermes run and session identity")
	}
	if !filepath.IsAbs(worker.Worktree) || filepath.Clean(worker.Worktree) != worker.Worktree || worker.Socket == "" || !filepath.IsAbs(worker.Socket) || filepath.Clean(worker.Socket) != worker.Socket {
		return errors.New("native reply worker lacks clean absolute route paths")
	}
	return nil
}

func sameWorkerIdentity(left, right Worker) bool {
	return left.Name == right.Name && left.Harness == right.Harness && filepath.Clean(left.Worktree) == filepath.Clean(right.Worktree) && left.Coordinator == right.Coordinator && left.RunID == right.RunID && filepath.Clean(left.Socket) == filepath.Clean(right.Socket) && left.ThreadID == right.ThreadID
}

func validateNativeSource(source NativeReplySource, expected Worker) error {
	if source.InputID == "" || source.InputDigest == "" || source.Recipient == "" || source.RecipientRunID == "" || source.SessionID == "" || source.RunID == "" || source.Prompt == "" {
		return errors.New("native reply source is incomplete")
	}
	if source.SessionID != expected.ThreadID || source.RunID != expected.RunID {
		return ErrNativeReplyStale
	}
	return nil
}

func findNativeInput(entries []Entry, id string) (Entry, error) {
	for _, entry := range entries {
		if entry.Msg.ID == id {
			switch entry.Status {
			case StatusQueued, StatusUnconfirmed, StatusSent:
				return entry, nil
			default:
				return Entry{}, fmt.Errorf("%w: input %s has status %s", ErrNativeReplyStale, quoteTrunc(id), quoteTrunc(string(entry.Status)))
			}
		}
	}
	return Entry{}, fmt.Errorf("%w: input %s is not in the mailbox", ErrNativeReplyStale, quoteTrunc(id))
}

func validateNativeInput(service *Service, transaction *Tx, expected Worker, entry Entry, rendered string) error {
	message := entry.Msg
	if message.V != 1 || message.ID == "" || !threadPattern.MatchString(message.ID) || message.From == "" || message.To != expected.Name || message.ToRunID != expected.RunID || message.FromRunID == "" || !threadPattern.MatchString(message.FromRunID) || !threadPattern.MatchString(message.ToRunID) || message.Priority != PriorityNow && message.Priority != PriorityNext && message.Priority != PriorityLater || message.CreatedAt.IsZero() || message.Body == "" {
		return ErrNativeReplyStale
	}
	if err := ValidateName(message.From); err != nil {
		return err
	}
	if clean, err := CleanBody(message.Body); err != nil || clean != message.Body {
		return ErrNativeReplyStale
	}
	if err := ValidateReplyBinary(message.ReplyBinary); err != nil {
		return err
	}
	if Render(message) != rendered {
		return ErrNativeReplyStale
	}
	sender, err := service.workerForTx(transaction, message.From)
	if err != nil || sender.RunID != message.FromRunID {
		return ErrNativeReplyStale
	}
	return nil
}

func nativeMessageDigest(message Message) string {
	value := struct {
		V                 int       `json:"v"`
		ID                string    `json:"id"`
		From              string    `json:"from"`
		To                string    `json:"to"`
		FromRunID         string    `json:"from_run_id"`
		ToRunID           string    `json:"to_run_id"`
		Body              string    `json:"body"`
		Priority          Priority  `json:"priority"`
		CreatedAt         time.Time `json:"created_at"`
		ReplyBinary       string    `json:"reply_binary"`
		ReplyTo           string    `json:"reply_to"`
		ReplySessionID    string    `json:"reply_session_id"`
		ReplySourceDigest string    `json:"reply_source_digest"`
	}{message.V, message.ID, message.From, message.To, message.FromRunID, message.ToRunID, message.Body, message.Priority, message.CreatedAt, message.ReplyBinary, message.ReplyTo, message.ReplySessionID, message.ReplySourceDigest}
	var data bytes.Buffer
	if err := termsafe.JSONEncoder(&data).Encode(value); err != nil {
		return ""
	}
	sum := sha256.Sum256(bytes.TrimSuffix(data.Bytes(), []byte{'\n'}))
	return hex.EncodeToString(sum[:])
}

func nativeReplyID(inputID, sessionID, senderRun string) string {
	sum := sha256.Sum256([]byte("forgectl-native-reply\x00" + inputID + "\x00" + sessionID + "\x00" + senderRun))
	return "reply-" + hex.EncodeToString(sum[:])
}

func findNativeReply(entries []Entry, id string) (Entry, bool) {
	for _, entry := range entries {
		if entry.Msg.ID == id {
			return entry, true
		}
	}
	return Entry{}, false
}

func sameNativeReply(message Message, expected Worker, source NativeReplySource, input Message, body, replyBinary string) bool {
	return message.V == 1 && message.Priority == PriorityNext && message.ReplyBinary == replyBinary && message.ReplyTo == source.InputID && message.ReplySessionID == source.SessionID && message.ReplySourceDigest == source.InputDigest && message.From == expected.Name && message.FromRunID == expected.RunID && message.To == input.From && message.ToRunID == input.FromRunID && message.Body == body
}

func (service *Service) workerForTx(transaction *Tx, name string) (Worker, error) {
	if transaction != nil && transaction.private != nil {
		switch roster := service.Roster.(type) {
		case FileRoster:
			if filepath.Clean(roster.Dir) == filepath.Clean(service.Box.Dir) {
				return transaction.worker(name)
			}
		case *FileRoster:
			if roster != nil && filepath.Clean(roster.Dir) == filepath.Clean(service.Box.Dir) {
				return transaction.worker(name)
			}
		}
	}
	return service.worker(name)
}

func (transaction *Tx) worker(name string) (Worker, error) {
	if transaction.private == nil {
		return Worker{}, errors.New("private roster is unavailable")
	}
	return transaction.private.worker(name)
}

func (service *Service) nativeReplyAttemptAllowed(transaction *Tx, message Message) error {
	if message.ReplyTo == "" {
		return nil
	}
	origin, err := service.workerForTx(transaction, message.From)
	if err != nil || origin.RunID != message.FromRunID || origin.ThreadID != message.ReplySessionID {
		return ErrNativeReplyStale
	}
	return nil
}

func (service *Service) validateNativeStore() error {
	if !service.Box.Private {
		return ErrNativeReplyUnavailable
	}
	switch roster := service.Roster.(type) {
	case FileRoster:
		if filepath.Clean(roster.Dir) == filepath.Clean(service.Box.Dir) {
			return nil
		}
	case *FileRoster:
		if roster != nil && filepath.Clean(roster.Dir) == filepath.Clean(service.Box.Dir) {
			return nil
		}
	}
	return errors.New("native replies require a file roster in the private mailbox")
}
