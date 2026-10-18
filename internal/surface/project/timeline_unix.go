//go:build unix

package project

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/privdir"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

const (
	maxMailboxBytes    = 8 << 20
	maxMailboxRecord   = 1 << 20
	maxTimelineEntries = 50000
)

type mailboxRecord struct {
	Op      string          `json:"op"`
	At      time.Time       `json:"at"`
	Msg     json.RawMessage `json:"msg"`
	ID      string          `json:"id"`
	Status  mail.Status     `json:"status"`
	Detail  string          `json:"detail"`
	Attempt bool            `json:"attempt"`
}

type mailboxEntry struct {
	Message mail.Message
	Status  mail.Status
	At      time.Time
}

func (store Store) MailTimeline(ctx context.Context, projectID string) (MailTimeline, error) {
	if err := checkContext(ctx); err != nil {
		return MailTimeline{}, err
	}
	root, err := validateRoot(store.Root)
	if err != nil {
		return MailTimeline{}, err
	}
	if err := validateID(projectID, "project ID"); err != nil {
		return MailTimeline{}, err
	}
	directoryFD, err := openRoot(root, false)
	if errors.Is(err, privdir.ErrAbsent) {
		return MailTimeline{}, fmt.Errorf("%w: project %s", ErrNotFound, projectID)
	}
	if err != nil {
		return MailTimeline{}, err
	}
	defer func() { _ = unix.Close(directoryFD) }()
	_, target, summaries, err := inspectProjectAt(directoryFD, projectID)
	if err != nil {
		return MailTimeline{}, err
	}
	result := MailTimeline{
		ProjectID:       projectID,
		ProjectName:     safeTimelineValue(target.Name),
		ObservedAt:      time.Now().UTC(),
		ThreadSummaries: summaries,
		Entries:         make([]MailObservation, 0),
		Evidence:        target.Evidence,
	}
	projectFD, err := openPrivateDirectory(directoryFD, projectID)
	if errors.Is(err, privdir.ErrAbsent) {
		return result, nil
	}
	if err != nil {
		return MailTimeline{}, stateError("open project ledger: %s", privdir.Reason(err))
	}
	defer func() { _ = unix.Close(projectFD) }()
	teamFD, err := openPrivateDirectory(projectFD, projectDirectoryName)
	if errors.Is(err, privdir.ErrAbsent) {
		return result, nil
	}
	if err != nil {
		return MailTimeline{}, stateError("open team ledger: %s", privdir.Reason(err))
	}
	defer func() { _ = unix.Close(teamFD) }()
	mailbox, err := privdir.OpenFileChecked(teamFD, "mailbox.jsonl", unix.O_RDONLY, stateMode)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return MailTimeline{}, stateError("open mailbox: %s", privdir.Reason(err))
	}
	defer func() { _ = mailbox.Close() }()
	data, err := io.ReadAll(io.LimitReader(mailbox, maxMailboxBytes+1))
	if err != nil {
		return MailTimeline{}, stateError("read mailbox: %v", err)
	}
	if len(data) > maxMailboxBytes {
		result.Partial = true
		data = data[:maxMailboxBytes]
		if last := bytes.LastIndexByte(data, '\n'); last >= 0 {
			data = data[:last+1]
		}
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		result.Held = true
		if last := bytes.LastIndexByte(data, '\n'); last >= 0 {
			data = data[:last+1]
		} else {
			data = nil
		}
	}

	entries, skipped, capped := foldMailbox(data)
	result.Skipped = skipped
	result.Partial = result.Partial || capped || result.Held || skipped > 0
	for _, entry := range entries {
		observation := projectMailObservation(entry, target)
		result.Entries = append(result.Entries, observation)
	}
	return result, nil
}

// inspectProjectAt performs the project/roster read through the caller's
// already-pinned root descriptor. It mirrors the existing inspection rules
// without reopening the root between identity and mailbox reads.
func inspectProjectAt(directoryFD int, projectID string) (Registry, Project, []MailThreadSummary, error) {
	if err := checkRegistryLock(directoryFD); err != nil {
		return Registry{}, Project{}, nil, err
	}
	registry, err := readRegistry(directoryFD)
	if err != nil {
		return Registry{}, Project{}, nil, err
	}
	projectIndex := -1
	for index := range registry.Projects {
		if registry.Projects[index].ID == projectID {
			projectIndex = index
			break
		}
	}
	if projectIndex < 0 {
		return Registry{}, Project{}, nil, fmt.Errorf("%w: project %s", ErrNotFound, projectID)
	}
	roster, err := readRoster(directoryFD, projectID)
	if err != nil {
		return Registry{}, Project{}, nil, err
	}
	project := registry.Projects[projectIndex]
	observedAt := time.Now().UTC()
	summaries := make([]MailThreadSummary, 0, len(project.Threads))
	for _, thread := range project.Threads {
		observation := Observation{
			ThreadID:        thread.ID,
			Enrollment:      EnrollmentUnbound,
			ProcessLiveness: ProcessLivenessUnknown,
			ObservedAt:      observedAt,
		}
		if thread.ActiveRunID != "" {
			activeIndex := activeBindingIndex(thread)
			if activeIndex < 0 {
				return Registry{}, Project{}, nil, fmt.Errorf("%w: thread %s has no active binding", ErrInvalidState, thread.ID)
			}
			active := thread.Runs[activeIndex]
			observation.NativeSessionKnown = active.NativeSessionID != ""
			observation.Enrollment = EnrollmentStale
			if !roster.present {
				observation.Reason = "roster-missing"
			} else if worker, found := roster.byName[active.Member]; !found {
				observation.Reason = "member-missing"
			} else {
				candidate := bindingFromWorker(worker)
				switch {
				case worker.RunID != active.RunID:
					observation.Reason = "run-mismatch"
				case !roleMatches(thread.Role, worker):
					observation.Reason = "role-mismatch"
				case !sameBindingIdentity(active, candidate):
					observation.Reason = "identity-mismatch"
				case active.NativeSessionID != candidate.NativeSessionID:
					observation.Reason = "native-session-mismatch"
				default:
					observation.Enrollment = EnrollmentCurrent
				}
			}
		}
		summaries = append(summaries, MailThreadSummary{
			ThreadID:           safeTimelineValue(thread.ID),
			Name:               safeTimelineValue(thread.Name),
			ActiveRunID:        safeTimelineValue(thread.ActiveRunID),
			Role:               thread.Role,
			Enrollment:         observation.Enrollment,
			NativeSessionKnown: observation.NativeSessionKnown,
			ProcessLiveness:    observation.ProcessLiveness,
			Reason:             safeTimelineValue(observation.Reason),
		})
	}
	return registry, project, summaries, nil
}

func foldMailbox(data []byte) ([]mailboxEntry, int, bool) {
	var entries []mailboxEntry
	indices := make(map[string]int)
	skipped := 0
	capped := false
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) > maxMailboxRecord {
			skipped++
			capped = true
			continue
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		record, msg, ok := decodeMailboxRecord(line)
		if !ok {
			skipped++
			continue
		}
		switch record.Op {
		case "msg":
			if len(entries) >= maxTimelineEntries {
				skipped++
				capped = true
				continue
			}
			if _, exists := indices[msg.ID]; exists {
				skipped++
				continue
			}
			indices[msg.ID] = len(entries)
			entries = append(entries, mailboxEntry{Message: msg, Status: mail.StatusQueued, At: record.At})
		case "status":
			index, exists := indices[record.ID]
			if !exists || !validMailboxStatus(record.Status) {
				skipped++
				continue
			}
			entries[index].Status = record.Status
			entries[index].At = record.At
		default:
			skipped++
		}
	}
	return entries, skipped, capped
}

func decodeMailboxRecord(line []byte) (mailboxRecord, mail.Message, bool) {
	if !utf8.Valid(line) || scanJSON(line) != nil {
		return mailboxRecord{}, mail.Message{}, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil || raw == nil {
		return mailboxRecord{}, mail.Message{}, false
	}
	if err := checkKeys(raw, "op", "at", "msg", "id", "status", "detail", "attempt"); err != nil {
		return mailboxRecord{}, mail.Message{}, false
	}
	if err := requireFields(raw, "op", "at"); err != nil || hasNullFields(raw) {
		return mailboxRecord{}, mail.Message{}, false
	}
	var record mailboxRecord
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return mailboxRecord{}, mail.Message{}, false
	}
	if record.At.IsZero() {
		return mailboxRecord{}, mail.Message{}, false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return mailboxRecord{}, mail.Message{}, false
	}
	switch record.Op {
	case "msg":
		if _, ok := raw["msg"]; !ok || len(raw) != 3 {
			return mailboxRecord{}, mail.Message{}, false
		}
		var messageFields map[string]json.RawMessage
		if err := json.Unmarshal(raw["msg"], &messageFields); err != nil || messageFields == nil {
			return mailboxRecord{}, mail.Message{}, false
		}
		if err := checkKeys(messageFields, "v", "id", "from", "to", "from_run_id", "to_run_id", "body", "priority", "created_at", "reply_binary", "reply_to", "reply_session_id", "reply_source_digest"); err != nil {
			return mailboxRecord{}, mail.Message{}, false
		}
		if err := requireFields(messageFields, "v", "id", "from", "to", "body", "priority", "created_at"); err != nil || hasNullFields(messageFields) {
			return mailboxRecord{}, mail.Message{}, false
		}
		replyFieldCount := 0
		for _, field := range []string{"reply_to", "reply_session_id", "reply_source_digest"} {
			if _, present := messageFields[field]; present {
				replyFieldCount++
			}
		}
		if replyFieldCount != 0 && replyFieldCount != 3 {
			return mailboxRecord{}, mail.Message{}, false
		}
		var msg mail.Message
		msgDecoder := json.NewDecoder(bytes.NewReader(raw["msg"]))
		msgDecoder.DisallowUnknownFields()
		if err := msgDecoder.Decode(&msg); err != nil || msg.V != 1 || msg.ID == "" || msg.CreatedAt.IsZero() {
			return mailboxRecord{}, mail.Message{}, false
		}
		if err := mail.ValidateName(msg.From); err != nil && msg.From != mail.SystemSender {
			return mailboxRecord{}, mail.Message{}, false
		}
		if err := mail.ValidateName(msg.To); err != nil {
			return mailboxRecord{}, mail.Message{}, false
		}
		if err := validateOptionalToken(msg.FromRunID, "message from run ID"); err != nil {
			return mailboxRecord{}, mail.Message{}, false
		}
		if err := validateOptionalToken(msg.ToRunID, "message to run ID"); err != nil {
			return mailboxRecord{}, mail.Message{}, false
		}
		if _, err := mail.ParsePriority(string(msg.Priority)); err != nil {
			return mailboxRecord{}, mail.Message{}, false
		}
		if err := mail.ValidateReplyBinary(msg.ReplyBinary); err != nil {
			return mailboxRecord{}, mail.Message{}, false
		}
		if replyFieldCount == 3 {
			if err := validateRunID(msg.ReplyTo, "message reply target"); err != nil {
				return mailboxRecord{}, mail.Message{}, false
			}
			if err := validateRunID(msg.ReplySessionID, "message reply session"); err != nil || !validDigest(msg.ReplySourceDigest) {
				return mailboxRecord{}, mail.Message{}, false
			}
		}
		return record, msg, true
	case "status":
		if _, ok := raw["id"]; !ok {
			return mailboxRecord{}, mail.Message{}, false
		}
		if _, ok := raw["status"]; !ok || len(raw) < 4 || len(raw) > 6 {
			return mailboxRecord{}, mail.Message{}, false
		}
		if _, hasMessage := raw["msg"]; hasMessage {
			return mailboxRecord{}, mail.Message{}, false
		}
		if err := requireFields(raw, "id", "status"); err != nil {
			return mailboxRecord{}, mail.Message{}, false
		}
		if record.ID == "" {
			return mailboxRecord{}, mail.Message{}, false
		}
		return record, mail.Message{}, true
	default:
		return record, mail.Message{}, false
	}
}

func hasNullFields(object map[string]json.RawMessage) bool {
	for _, value := range object {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return true
		}
	}
	return false
}

func validMailboxStatus(status mail.Status) bool {
	switch status {
	case mail.StatusQueued, mail.StatusSent, mail.StatusUnconfirmed, mail.StatusFailed, mail.StatusExpired:
		return true
	default:
		return false
	}
}

func projectMailObservation(entry mailboxEntry, target Project) MailObservation {
	status := MailDeliveryUnknown
	switch entry.Status {
	case mail.StatusQueued:
		status = MailWaiting
	case mail.StatusFailed, mail.StatusExpired:
		status = MailDeliveryFailed
	case mail.StatusSent:
		status = MailTransportAccepted
	case mail.StatusUnconfirmed:
		status = MailDeliveryUnknown
	}
	observation := MailObservation{
		MessageID: safeTimelineValue(entry.Message.ID),
		From:      safeTimelineValue(entry.Message.From),
		To:        safeTimelineValue(entry.Message.To),
		At:        entry.At,
		Status:    status,
	}
	if binding, threadID, ok := exactBinding(target, entry.Message.From, entry.Message.FromRunID); ok {
		observation.FromThreadID = safeTimelineValue(threadID)
		observation.FromRunID = safeTimelineValue(binding.RunID)
	}
	if binding, threadID, ok := exactBinding(target, entry.Message.To, entry.Message.ToRunID); ok {
		observation.ToThreadID = safeTimelineValue(threadID)
		observation.ToRunID = safeTimelineValue(binding.RunID)
	}
	return observation
}

func exactBinding(target Project, member, runID string) (Binding, string, bool) {
	if member == "" || runID == "" {
		return Binding{}, "", false
	}
	var match Binding
	var threadID string
	for _, thread := range target.Threads {
		for _, binding := range thread.Runs {
			if binding.Member != member || binding.RunID != runID {
				continue
			}
			if threadID != "" {
				return Binding{}, "", false
			}
			match = binding
			threadID = thread.ID
		}
	}
	return match, threadID, threadID != ""
}

func safeTimelineValue(value string) string {
	return termsafe.SafeLineMax(value, 128)
}
