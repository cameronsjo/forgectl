//go:build unix

package project

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cameronsjo/forgectl/internal/surface/mail"
)

func TestMailTimelineIsTransportOnlyWithoutInventedAttribution(test *testing.T) {
	store, worktree := testStore(test)
	_, created, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
	if err != nil {
		test.Fatal(err)
	}
	_, workerThread, err := store.CreateThread(context.Background(), created.ID, "worker", "")
	if err != nil {
		test.Fatal(err)
	}
	teamPath := filepath.Join(store.Root, created.ID, projectDirectoryName)
	enrollTestRoster(test, store, created.ID,
		testWorker("lead", true, "run-lead", worktree, "session-lead"),
		testWorker("worker", false, "run-worker", test.TempDir(), "session-worker"),
	)
	if _, _, err := store.Bind(context.Background(), created.ID, created.CoordinatorThreadID, "lead", "run-lead", ""); err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.Bind(context.Background(), created.ID, workerThread.ID, "worker", "run-worker", ""); err != nil {
		test.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	box := mail.Mailbox{Dir: teamPath}
	if err := box.Locked(func(tx *mail.Tx) error {
		message := mail.Message{V: 1, ID: "m-1", From: "lead", To: "worker", FromRunID: "run-lead", ToRunID: "run-worker", Body: "private prompt", Priority: mail.PriorityNext, CreatedAt: now}
		if err := tx.Enqueue(message, now); err != nil {
			return err
		}
		return tx.Mark(message.ID, mail.StatusSent, "secret delivery detail", false, now.Add(time.Second))
	}); err != nil {
		test.Fatal(err)
	}

	timeline, err := store.MailTimeline(context.Background(), created.ID)
	if err != nil {
		test.Fatal(err)
	}
	if len(timeline.Entries) != 1 {
		test.Fatalf("entries = %#v", timeline.Entries)
	}
	entry := timeline.Entries[0]
	if entry.Status != MailTransportAccepted || entry.From != "lead" || entry.To != "worker" || entry.FromRunID != "run-lead" || entry.ToRunID != "run-worker" {
		test.Fatalf("entry = %#v", entry)
	}
	if len(timeline.ThreadSummaries) != 2 || timeline.ThreadSummaries[0].Enrollment != EnrollmentCurrent || timeline.ThreadSummaries[1].Enrollment != EnrollmentCurrent {
		test.Fatalf("thread summaries = %#v", timeline.ThreadSummaries)
	}
	if strings.Contains(string(mustJSON(test, timeline)), "private prompt") || strings.Contains(string(mustJSON(test, timeline)), "secret delivery detail") {
		test.Fatalf("timeline exposed mailbox content: %s", mustJSON(test, timeline))
	}
}

func TestMailTimelineAcceptsNativeReplyProvenanceAsTransportOnly(test *testing.T) {
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	validMessage := mail.Message{
		V:                 1,
		ID:                "reply-1",
		From:              "lead",
		To:                "worker",
		Body:              "private native reply",
		Priority:          mail.PriorityNext,
		CreatedAt:         now,
		ReplyTo:           "input-1",
		ReplySessionID:    "session-1",
		ReplySourceDigest: strings.Repeat("a", 64),
	}
	writeMailbox := func(test *testing.T, malformed []byte) MailTimeline {
		test.Helper()
		store, worktree := testStore(test)
		_, created, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
		if err != nil {
			test.Fatal(err)
		}
		teamPath := filepath.Join(store.Root, created.ID, projectDirectoryName)
		box := mail.Mailbox{Dir: teamPath}
		if err := box.Locked(func(tx *mail.Tx) error {
			if err := tx.Enqueue(validMessage, now); err != nil {
				return err
			}
			return tx.Mark(validMessage.ID, mail.StatusSent, "", false, now.Add(time.Second))
		}); err != nil {
			test.Fatal(err)
		}
		if malformed != nil {
			mailboxPath := filepath.Join(teamPath, "mailbox.jsonl")
			contents := append(readTestFile(test, mailboxPath), malformed...)
			writeTestFile(test, mailboxPath, append(contents, '\n'), 0o600)
		}
		timeline, err := store.MailTimeline(context.Background(), created.ID)
		if err != nil {
			test.Fatal(err)
		}
		return timeline
	}

	valid := writeMailbox(test, nil)
	if len(valid.Entries) != 1 || valid.Skipped != 0 || valid.Partial {
		test.Fatalf("valid native reply timeline = %#v", valid)
	}
	if entry := valid.Entries[0]; entry.MessageID != validMessage.ID || entry.From != validMessage.From || entry.To != validMessage.To || entry.Status != MailTransportAccepted {
		test.Fatalf("valid native reply transport observation = %#v", entry)
	}
	if got := string(mustJSON(test, valid)); strings.Contains(got, "private native reply") || strings.Contains(got, "input-1") || strings.Contains(got, "session-1") || strings.Contains(got, strings.Repeat("a", 64)) {
		test.Fatalf("timeline exposed native reply content or provenance: %s", got)
	}

	encodeMessage := func(id string, mutate func(map[string]any)) []byte {
		messageFields := map[string]any{
			"v": 1, "id": id, "from": validMessage.From, "to": validMessage.To,
			"body": validMessage.Body, "priority": validMessage.Priority, "created_at": validMessage.CreatedAt,
			"reply_to": validMessage.ReplyTo, "reply_session_id": validMessage.ReplySessionID, "reply_source_digest": validMessage.ReplySourceDigest,
		}
		if mutate != nil {
			mutate(messageFields)
		}
		encoded, err := json.Marshal(map[string]any{"op": "msg", "at": now, "msg": messageFields})
		if err != nil {
			test.Fatal(err)
		}
		return encoded
	}
	cases := map[string][]byte{
		"only reply target":   encodeMessage("reply-only-target", func(fields map[string]any) { delete(fields, "reply_session_id"); delete(fields, "reply_source_digest") }),
		"only reply session":  encodeMessage("reply-only-session", func(fields map[string]any) { delete(fields, "reply_to"); delete(fields, "reply_source_digest") }),
		"only reply digest":   encodeMessage("reply-only-digest", func(fields map[string]any) { delete(fields, "reply_to"); delete(fields, "reply_session_id") }),
		"target and session":  encodeMessage("reply-target-session", func(fields map[string]any) { delete(fields, "reply_source_digest") }),
		"target and digest":   encodeMessage("reply-target-digest", func(fields map[string]any) { delete(fields, "reply_session_id") }),
		"session and digest":  encodeMessage("reply-session-digest", func(fields map[string]any) { delete(fields, "reply_to") }),
		"empty target":        encodeMessage("reply-empty-target", func(fields map[string]any) { fields["reply_to"] = "" }),
		"null target":         encodeMessage("reply-null-target", func(fields map[string]any) { fields["reply_to"] = nil }),
		"invalid target type": encodeMessage("reply-invalid-target", func(fields map[string]any) { fields["reply_to"] = 7 }),
		"malformed digest":    encodeMessage("reply-malformed-digest", func(fields map[string]any) { fields["reply_source_digest"] = "ABC" }),
		"uppercase digest":    encodeMessage("reply-uppercase-digest", func(fields map[string]any) { fields["reply_source_digest"] = strings.Repeat("A", 64) }),
		"oversized target":    encodeMessage("reply-oversized-target", func(fields map[string]any) { fields["reply_to"] = strings.Repeat("a", 129) }),
		"oversized session":   encodeMessage("reply-oversized-session", func(fields map[string]any) { fields["reply_session_id"] = strings.Repeat("a", 129) }),
		"control target":      encodeMessage("reply-control-target", func(fields map[string]any) { fields["reply_to"] = "bad\nid" }),
		"control session":     encodeMessage("reply-control-session", func(fields map[string]any) { fields["reply_session_id"] = "bad\nid" }),
		"extra field":         encodeMessage("reply-extra-field", func(fields map[string]any) { fields["unexpected"] = true }),
		"duplicate field":     []byte(strings.Replace(string(encodeMessage("reply-duplicate-field", nil)), `"reply_to":"input-1"`, `"reply_to":"input-1","reply_to":"input-2"`, 1)),
	}
	for name, malformed := range cases {
		test.Run(name, func(test *testing.T) {
			if _, _, ok := decodeMailboxRecord(malformed); ok {
				test.Fatalf("malformed %s record decoded successfully", name)
			}
			timeline := writeMailbox(test, malformed)
			if len(timeline.Entries) != 1 || timeline.Skipped != 1 || !timeline.Partial {
				test.Fatalf("malformed %s timeline = %#v", name, timeline)
			}
		})
	}
}

func TestMailTimelineKeepsMalformedAndHeldInputVisible(test *testing.T) {
	store, worktree := testStore(test)
	_, created, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
	if err != nil {
		test.Fatal(err)
	}
	teamPath := filepath.Join(store.Root, created.ID, projectDirectoryName)
	if err := os.MkdirAll(teamPath, 0o700); err != nil {
		test.Fatal(err)
	}
	mailboxPath := filepath.Join(teamPath, "mailbox.jsonl")
	if err := os.WriteFile(mailboxPath, []byte("{\"op\":\"msg\",\"op\":\"msg\"}\n{\"op\":\"msg\"}"), 0o600); err != nil {
		test.Fatal(err)
	}
	timeline, err := store.MailTimeline(context.Background(), created.ID)
	if err != nil {
		test.Fatal(err)
	}
	if timeline.Skipped == 0 || !timeline.Held || len(timeline.Entries) != 0 {
		test.Fatalf("timeline = %#v", timeline)
	}
}

func TestMailTimelineRequiresExactEnvelopeRunIdentity(test *testing.T) {
	store, worktree := testStore(test)
	_, created, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
	if err != nil {
		test.Fatal(err)
	}
	roster := enrollTestRoster(test, store, created.ID, testWorker("lead", true, "run-one", worktree, "session-one"))
	if _, _, err := store.Bind(context.Background(), created.ID, created.CoordinatorThreadID, "lead", "run-one", ""); err != nil {
		test.Fatal(err)
	}
	if err := roster.Put(testWorker("lead", true, "run-two", worktree, "session-two")); err != nil {
		test.Fatal(err)
	}
	if _, _, err := store.Bind(context.Background(), created.ID, created.CoordinatorThreadID, "lead", "run-two", "run-one"); err != nil {
		test.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	box := mail.Mailbox{Dir: filepath.Join(store.Root, created.ID, projectDirectoryName)}
	if err := box.Locked(func(tx *mail.Tx) error {
		messages := []mail.Message{
			{V: 1, ID: "historical", From: "lead", To: "lead", FromRunID: "run-one", ToRunID: "run-one", CreatedAt: now},
			{V: 1, ID: "unknown", From: "lead", To: "lead", FromRunID: "not-bound", ToRunID: "not-bound", CreatedAt: now.Add(time.Second)},
			{V: 1, ID: "legacy", From: "lead", To: "lead", CreatedAt: now.Add(2 * time.Second)},
		}
		for _, message := range messages {
			if err := tx.Enqueue(message, message.CreatedAt); err != nil {
				return err
			}
			if err := tx.Mark(message.ID, mail.StatusSent, "", false, message.CreatedAt.Add(time.Second)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	timeline, err := store.MailTimeline(context.Background(), created.ID)
	if err != nil {
		test.Fatal(err)
	}
	if len(timeline.Entries) != 3 {
		test.Fatalf("entries = %#v", timeline.Entries)
	}
	if timeline.Entries[0].FromRunID != "run-one" || timeline.Entries[0].ToRunID != "run-one" {
		test.Fatalf("historical attribution = %#v", timeline.Entries[0])
	}
	for _, entry := range timeline.Entries[1:] {
		if entry.FromRunID != "" || entry.ToRunID != "" || entry.FromThreadID != "" || entry.ToThreadID != "" {
			test.Fatalf("unproven attribution = %#v", entry)
		}
	}
}

func TestMailTimelineMissingRootDoesNotCreate(test *testing.T) {
	store, _ := testStore(test)
	_, err := store.MailTimeline(context.Background(), "00000000-0000-4000-8000-000000000000")
	if !errors.Is(err, ErrNotFound) {
		test.Fatalf("MailTimeline = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(store.Root); !errors.Is(err, os.ErrNotExist) {
		test.Fatalf("MailTimeline created root: %v", err)
	}
}

func TestMailTimelineRefusesUnsafeMailboxWithoutMutation(test *testing.T) {
	const contents = `{"op":"msg","at":"2026-10-17T00:00:00Z","msg":{"v":1,"id":"mail-1","from":"lead","to":"worker","body":"body","priority":"next","created_at":"2026-10-17T00:00:00Z"}}
`
	cases := []struct {
		name  string
		setup func(test *testing.T, mailboxPath string) string
		check func(test *testing.T, mailboxPath, targetPath string, before []byte)
	}{
		{
			name: "public mailbox",
			setup: func(test *testing.T, mailboxPath string) string {
				if err := os.Chmod(mailboxPath, 0o644); err != nil { //nolint:gosec // G302: hostile fixture proves read refusal
					test.Fatal(err)
				}
				return mailboxPath
			},
			check: func(test *testing.T, mailboxPath, _ string, before []byte) {
				info, err := os.Stat(mailboxPath)
				if err != nil {
					test.Fatal(err)
				}
				if info.Mode().Perm() != 0o644 {
					test.Fatalf("mailbox mode = %o, want unchanged 644", info.Mode().Perm())
				}
				assertTestFileBytes(test, mailboxPath, before)
			},
		},
		{
			name: "mailbox symlink",
			setup: func(test *testing.T, mailboxPath string) string {
				target := filepath.Join(test.TempDir(), "mailbox-target")
				if err := os.WriteFile(target, []byte(contents), 0o600); err != nil {
					test.Fatal(err)
				}
				if err := os.Remove(mailboxPath); err != nil {
					test.Fatal(err)
				}
				if err := os.Symlink(target, mailboxPath); err != nil {
					test.Fatal(err)
				}
				return target
			},
			check: func(test *testing.T, mailboxPath, targetPath string, before []byte) {
				if info, err := os.Lstat(mailboxPath); err != nil {
					test.Fatal(err)
				} else if info.Mode()&os.ModeSymlink == 0 {
					test.Fatalf("mailbox was replaced: mode=%v", info.Mode())
				}
				assertTestFileBytes(test, targetPath, before)
			},
		},
		{
			name: "mailbox hardlink",
			setup: func(test *testing.T, mailboxPath string) string {
				target := filepath.Join(test.TempDir(), "mailbox-target")
				if err := os.WriteFile(target, []byte(contents), 0o600); err != nil {
					test.Fatal(err)
				}
				if err := os.Remove(mailboxPath); err != nil {
					test.Fatal(err)
				}
				if err := os.Link(target, mailboxPath); err != nil {
					test.Fatal(err)
				}
				return target
			},
			check: func(test *testing.T, mailboxPath, targetPath string, before []byte) {
				if got := testNlink(test, targetPath); got != 2 {
					test.Fatalf("target link count = %d, want 2", got)
				}
				if info, err := os.Stat(mailboxPath); err != nil {
					test.Fatal(err)
				} else if info.Mode().Perm() != 0o600 {
					test.Fatalf("hardlinked mailbox mode = %o, want unchanged 600", info.Mode().Perm())
				}
				assertTestFileBytes(test, targetPath, before)
			},
		},
	}
	for _, testCase := range cases {
		test.Run(testCase.name, func(test *testing.T) {
			store, worktree := testStore(test)
			_, created, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
			if err != nil {
				test.Fatal(err)
			}
			teamPath := filepath.Join(store.Root, created.ID, projectDirectoryName)
			if err := os.MkdirAll(teamPath, 0o700); err != nil {
				test.Fatal(err)
			}
			mailboxPath := filepath.Join(teamPath, "mailbox.jsonl")
			if err := os.WriteFile(mailboxPath, []byte(contents), 0o600); err != nil {
				test.Fatal(err)
			}
			targetPath := testCase.setup(test, mailboxPath)
			before := readTestFile(test, targetPath)
			if _, err := store.MailTimeline(context.Background(), created.ID); !errors.Is(err, ErrInvalidState) {
				test.Fatalf("MailTimeline = %v, want ErrInvalidState", err)
			}
			testCase.check(test, mailboxPath, targetPath, before)
		})
	}
}

func TestMailTimelineReportsMailboxCaps(test *testing.T) {
	valid := mailboxMessageLine(test, "cap-valid", time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC))
	cases := []struct {
		name     string
		contents []byte
		wantSize int
		wantSkip bool
	}{
		{
			name:     "oversized line",
			contents: append(append(bytes.Repeat([]byte("x"), maxMailboxRecord+1), '\n'), valid...),
			wantSize: 1,
			wantSkip: true,
		},
		{
			name:     "oversized file",
			contents: append(append([]byte(nil), valid...), bytes.Repeat([]byte("x"), maxMailboxBytes+1)...),
			wantSize: 1,
		},
	}
	for _, testCase := range cases {
		test.Run(testCase.name, func(test *testing.T) {
			store, worktree := testStore(test)
			_, created, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
			if err != nil {
				test.Fatal(err)
			}
			teamPath := filepath.Join(store.Root, created.ID, projectDirectoryName)
			if err := os.MkdirAll(teamPath, 0o700); err != nil {
				test.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(teamPath, "mailbox.jsonl"), testCase.contents, 0o600); err != nil {
				test.Fatal(err)
			}
			timeline, err := store.MailTimeline(context.Background(), created.ID)
			if err != nil {
				test.Fatal(err)
			}
			if len(timeline.Entries) != testCase.wantSize || !timeline.Partial || timeline.Held {
				test.Fatalf("timeline = %#v", timeline)
			}
			if testCase.wantSkip && timeline.Skipped != 1 {
				test.Fatalf("skipped = %d, want 1", timeline.Skipped)
			}
		})
	}
}

func TestMailTimelineKeepsHeldSuffixVisible(test *testing.T) {
	valid := mailboxMessageLine(test, "held-valid", time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC))
	contents := append(valid, []byte(`{"op":"msg"}`)...)
	store, worktree := testStore(test)
	_, created, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
	if err != nil {
		test.Fatal(err)
	}
	teamPath := filepath.Join(store.Root, created.ID, projectDirectoryName)
	if err := os.MkdirAll(teamPath, 0o700); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamPath, "mailbox.jsonl"), contents, 0o600); err != nil {
		test.Fatal(err)
	}
	timeline, err := store.MailTimeline(context.Background(), created.ID)
	if err != nil {
		test.Fatal(err)
	}
	if len(timeline.Entries) != 1 || !timeline.Held || !timeline.Partial || timeline.Skipped != 0 {
		test.Fatalf("timeline = %#v", timeline)
	}
}

func TestMailTimelineSkipsUnknownStatusAndDuplicateMessage(test *testing.T) {
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	message := mailboxMessageLine(test, "fold-once", now)
	unknownStatus := mailboxStatusLine(test, "fold-once", "delivered", now.Add(time.Second))
	contents := bytes.Join([][]byte{message, message, unknownStatus}, nil)
	store, worktree := testStore(test)
	_, created, err := store.CreateProject(context.Background(), "Alpha", worktree, "")
	if err != nil {
		test.Fatal(err)
	}
	teamPath := filepath.Join(store.Root, created.ID, projectDirectoryName)
	if err := os.MkdirAll(teamPath, 0o700); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamPath, "mailbox.jsonl"), contents, 0o600); err != nil {
		test.Fatal(err)
	}
	timeline, err := store.MailTimeline(context.Background(), created.ID)
	if err != nil {
		test.Fatal(err)
	}
	if len(timeline.Entries) != 1 || timeline.Skipped != 2 || timeline.Entries[0].Status != MailWaiting {
		test.Fatalf("timeline = %#v", timeline)
	}
}

func mailboxMessageLine(test *testing.T, id string, at time.Time) []byte {
	test.Helper()
	record := map[string]any{
		"op":  "msg",
		"at":  at,
		"msg": mail.Message{V: 1, ID: id, From: "lead", To: "worker", Body: "body", Priority: mail.PriorityNext, CreatedAt: at},
	}
	line, err := json.Marshal(record)
	if err != nil {
		test.Fatal(err)
	}
	return append(line, '\n')
}

func mailboxStatusLine(test *testing.T, id, status string, at time.Time) []byte {
	test.Helper()
	record := map[string]any{"op": "status", "at": at, "id": id, "status": status}
	line, err := json.Marshal(record)
	if err != nil {
		test.Fatal(err)
	}
	return append(line, '\n')
}

func assertTestFileBytes(test *testing.T, path string, want []byte) {
	test.Helper()
	directory, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		test.Fatal(err)
	}
	defer func() { _ = directory.Close() }()
	got, err := directory.ReadFile(filepath.Base(path))
	if err != nil {
		test.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		test.Fatalf("%s changed from %q to %q", path, want, got)
	}
}

func testNlink(test *testing.T, path string) uint64 {
	test.Helper()
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		test.Fatal(err)
	}
	return uint64(stat.Nlink)
}

func mustJSON(test *testing.T, value any) []byte {
	test.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	return data
}
