package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nativeInput(service *Service, id, from, fromRun string) Message {
	return Message{
		V: 1, ID: id, From: from, To: "hermes", FromRunID: fromRun, ToRunID: "hermes-run",
		Body: "request", Priority: PriorityNext, CreatedAt: service.now(), ReplyBinary: service.ReplyBinary,
	}
}

func nativeSourceForInput(test *testing.T, service *Service, hermes Worker, input Message) NativeReplySource {
	test.Helper()
	enqueueNativeInput(test, service, input)
	source, err := service.PrepareNativeReply(context.Background(), hermes, input.ID, Render(input))
	if err != nil {
		test.Fatal(err)
	}
	return source
}

func appendMailboxRecords(test *testing.T, service *Service, records ...record) {
	test.Helper()
	data := make([]byte, 0, len(records)*128)
	for _, item := range records {
		line, err := json.Marshal(item)
		if err != nil {
			test.Fatal(err)
		}
		data = append(data, line...)
		data = append(data, '\n')
	}
	if err := os.WriteFile(service.Box.logPath(), data, 0o600); err != nil {
		test.Fatal(err)
	}
}

func validMailboxMessage(id, from, to, fromRun, toRun, body string, created time.Time) Message {
	return Message{
		V: 1, ID: id, From: from, To: to, FromRunID: fromRun, ToRunID: toRun,
		Body: body, Priority: PriorityNext, CreatedAt: created, ReplyBinary: "/tmp/forgectl",
	}
}

func TestNativeReplyPolicyPreservesCoordinatorAndPeerBoundaries(test *testing.T) {
	service, hermes, _ := nativeReplyService(test)
	service.Policy.PeerMessages = false
	coordInput := nativeInput(service, "coord-input", "coord", "coord-run")
	coordSource := nativeSourceForInput(test, service, hermes, coordInput)
	if _, err := service.EnqueueNativeReply(context.Background(), hermes, coordSource, "coordinator reply"); err != nil {
		test.Fatalf("coordinator reply refused: %v", err)
	}

	peer := Worker{
		Name: "peer", Harness: HarnessClaude, Worktree: hermes.Worktree, RunID: "peer-run",
		Socket: filepath.Join(service.Box.Dir, "peer.sock"), ThreadID: "peer-session",
	}
	if err := service.Roster.Put(peer); err != nil {
		test.Fatal(err)
	}
	peerInput := nativeInput(service, "peer-input", peer.Name, peer.RunID)
	peerSource := nativeSourceForInput(test, service, hermes, peerInput)
	if _, err := service.EnqueueNativeReply(context.Background(), hermes, peerSource, "peer reply"); !errors.Is(err, ErrPeerBlocked) {
		test.Fatalf("peer reply error = %v, want ErrPeerBlocked", err)
	}
}

func TestNativeReplyPreservesBodyRateAndDedupeCaps(test *testing.T) {
	test.Run("body cap", func(test *testing.T) {
		service, hermes, _ := nativeReplyService(test)
		service.Policy.MaxBody = 3
		source := nativeSourceForInput(test, service, hermes, nativeInput(service, "cap-input", "coord", "coord-run"))
		if _, err := service.EnqueueNativeReply(context.Background(), hermes, source, "four"); !errors.Is(err, ErrTooLarge) {
			test.Fatalf("body cap error = %v, want ErrTooLarge", err)
		}
	})

	test.Run("rate cap", func(test *testing.T) {
		service, hermes, _ := nativeReplyService(test)
		service.Policy.PerPairPerMinute = 2
		now := service.now()
		prior := make([]record, 0, 2)
		for index := 0; index < 2; index++ {
			prior = append(prior, record{Op: opMsg, At: now, Msg: messagePointer(validMailboxMessage(fmt.Sprintf("prior-%d", index), hermes.Name, "coord", hermes.RunID, "coord-run", fmt.Sprintf("prior %d", index), now))})
		}
		appendMailboxRecords(test, service, prior...)
		source := nativeSourceForInput(test, service, hermes, nativeInput(service, "rate-input", "coord", "coord-run"))
		if _, err := service.EnqueueNativeReply(context.Background(), hermes, source, "rate limited"); !errors.Is(err, ErrRateLimited) {
			test.Fatalf("rate cap error = %v, want ErrRateLimited", err)
		}
	})

	test.Run("dedupe", func(test *testing.T) {
		service, hermes, _ := nativeReplyService(test)
		now := service.now()
		prior := validMailboxMessage("prior-dedupe", hermes.Name, "coord", hermes.RunID, "coord-run", "same reply", now)
		appendMailboxRecords(test, service, record{Op: opMsg, At: now, Msg: messagePointer(prior)})
		source := nativeSourceForInput(test, service, hermes, nativeInput(service, "dedupe-input", "coord", "coord-run"))
		if _, err := service.EnqueueNativeReply(context.Background(), hermes, source, "same reply"); !errors.Is(err, ErrDuplicate) {
			test.Fatalf("dedupe error = %v, want ErrDuplicate", err)
		}
	})
}

func TestNativeReplyExactDuplicateSkipsPolicyAndAdapter(test *testing.T) {
	service, hermes, adapter := nativeReplyService(test)
	source := nativeSourceForInput(test, service, hermes, nativeInput(service, "duplicate-input", "coord", "coord-run"))
	first, err := service.EnqueueNativeReply(context.Background(), hermes, source, "same answer")
	if err != nil {
		test.Fatal(err)
	}
	service.Policy.MaxBody = 1
	service.Adapters = nil
	second, err := service.EnqueueNativeReply(context.Background(), hermes, source, "same answer")
	if err != nil {
		test.Fatalf("exact duplicate error = %v", err)
	}
	if second.ID != first.ID || second.Status != first.Status {
		test.Fatalf("duplicate result = %#v, first = %#v", second, first)
	}
	if adapter.calls != 0 {
		test.Fatalf("duplicate invoked adapter %d times", adapter.calls)
	}
}

func TestNativeReplyRejectsSourceAndRosterDrift(test *testing.T) {
	test.Run("expected hermes replacement", func(test *testing.T) {
		service, hermes, _ := nativeReplyService(test)
		source := nativeSourceForInput(test, service, hermes, nativeInput(service, "expected-drift-input", "coord", "coord-run"))
		replaced := hermes
		replaced.RunID = "hermes-replaced-run"
		replaced.ThreadID = "hermes-replaced-session"
		if err := service.Roster.Put(replaced); err != nil {
			test.Fatal(err)
		}
		if _, err := service.EnqueueNativeReply(context.Background(), hermes, source, "answer"); !errors.Is(err, ErrNativeReplyStale) {
			test.Fatalf("expected hermes drift error = %v", err)
		}
	})

	test.Run("original recipient replacement", func(test *testing.T) {
		service, hermes, _ := nativeReplyService(test)
		source := nativeSourceForInput(test, service, hermes, nativeInput(service, "recipient-drift-input", "coord", "coord-run"))
		replaced := Worker{
			Name: "coord", Harness: HarnessClaude, Coordinator: true, Worktree: hermes.Worktree,
			RunID: "coord-replaced-run", Socket: filepath.Join(service.Box.Dir, "coord-replaced.sock"), ThreadID: "coord-replaced-session",
		}
		if err := service.Roster.Put(replaced); err != nil {
			test.Fatal(err)
		}
		if _, err := service.EnqueueNativeReply(context.Background(), hermes, source, "answer"); !errors.Is(err, ErrNativeReplyStale) {
			test.Fatalf("original recipient drift error = %v", err)
		}
	})

	test.Run("source recipient run mutation", func(test *testing.T) {
		service, hermes, _ := nativeReplyService(test)
		source := nativeSourceForInput(test, service, hermes, nativeInput(service, "source-drift-input", "coord", "coord-run"))
		source.RecipientRunID = "coord-other-run"
		if _, err := service.EnqueueNativeReply(context.Background(), hermes, source, "answer"); !errors.Is(err, ErrNativeReplyStale) {
			test.Fatalf("source recipient drift error = %v", err)
		}
	})
}

func TestNativeReplyStrictMailboxCorruption(test *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	valid := validMailboxMessage("strict-input", "coord", "hermes", "coord-run", "hermes-run", "request", now)
	cases := map[string]func(*testing.T, *Service){
		"duplicate message": func(test *testing.T, service *Service) {
			item := record{Op: opMsg, At: now, Msg: messagePointer(valid)}
			appendMailboxRecords(test, service, item, item)
		},
		"orphan status": func(test *testing.T, service *Service) {
			appendMailboxRecords(test, service, record{Op: opStatus, At: now, ID: "missing", Status: StatusSent})
		},
		"invalid status": func(test *testing.T, service *Service) {
			appendMailboxRecords(test, service, record{Op: opStatus, At: now, ID: valid.ID, Status: Status("unknown")})
		},
		"duplicate JSON key": func(test *testing.T, service *Service) {
			data := []byte(`{"op":"msg","op":"msg","at":"2026-10-17T12:00:00Z","msg":{"v":1,"id":"strict-input","from":"coord","to":"hermes","from_run_id":"coord-run","to_run_id":"hermes-run","body":"request","priority":"next","created_at":"2026-10-17T12:00:00Z","reply_binary":"/tmp/forgectl"}}` + "\n")
			if err := os.WriteFile(service.Box.logPath(), data, 0o600); err != nil {
				test.Fatal(err)
			}
		},
		"unsupported message version": func(test *testing.T, service *Service) {
			invalid := valid
			invalid.V = 2
			appendMailboxRecords(test, service, record{Op: opMsg, At: now, Msg: messagePointer(invalid)})
		},
	}
	for name, plant := range cases {
		test.Run(name, func(test *testing.T) {
			service, hermes, _ := nativeReplyService(test)
			plant(test, service)
			if _, err := service.PrepareNativeReply(context.Background(), hermes, valid.ID, Render(valid)); !errors.Is(err, ErrNativeReplyCorrupt) {
				test.Fatalf("corruption error = %v, want ErrNativeReplyCorrupt", err)
			}
		})
	}
}

func TestPrivateFlushRejectsCorruptionBeforeAdapter(test *testing.T) {
	service, _, adapter := nativeReplyService(test)
	appendMailboxRecords(test, service, record{Op: opStatus, At: service.now(), ID: "missing", Status: StatusSent})
	if _, err := service.Flush(context.Background()); !errors.Is(err, ErrNativeReplyCorrupt) {
		test.Fatalf("private Flush error = %v, want ErrNativeReplyCorrupt", err)
	}
	if adapter.calls != 0 {
		test.Fatalf("corrupt private Flush invoked adapter %d times", adapter.calls)
	}
}

func TestPrivateJSONValidationHandlesEscapesKeysAndDepth(test *testing.T) {
	for name, data := range map[string][]byte{
		"literal backslash text": []byte(`{"body":"\\ud800"}`),
		"paired surrogate":       []byte(`{"body":"\ud83d\ude00"}`),
	} {
		test.Run(name, func(test *testing.T) {
			if err := validatePrivateJSONBytes(data); err != nil {
				test.Fatalf("validatePrivateJSONBytes = %v", err)
			}
		})
	}
	for name, data := range map[string][]byte{
		"unpaired high surrogate": []byte(`{"body":"\ud800"}`),
		"unpaired low surrogate":  []byte(`{"body":"\udc00"}`),
		"case folded duplicate":   []byte(`{"op":"msg","Op":"status"}`),
	} {
		test.Run(name, func(test *testing.T) {
			if err := rejectDuplicateJSONKeys(data); err == nil {
				test.Fatal("invalid private JSON was accepted")
			}
		})
	}
	deep := []byte(strings.Repeat("[", maxPrivateJSONDepth+2) + "0" + strings.Repeat("]", maxPrivateJSONDepth+2))
	if err := rejectDuplicateJSONKeys(deep); err == nil {
		test.Fatal("deep private JSON was accepted")
	}
}

func TestNativeReplyStrictPrivateObjects(test *testing.T) {
	cases := map[string]func(*testing.T, *Service){
		"public directory": func(test *testing.T, service *Service) {
			if err := os.Chmod(service.Box.Dir, 0o755); err != nil {
				test.Fatal(err)
			}
		},
		"public roster": func(test *testing.T, service *Service) {
			makePublic(test, filepath.Join(service.Box.Dir, "roster.json"))
		},
		"public mailbox": func(test *testing.T, service *Service) {
			makePublic(test, filepath.Join(service.Box.Dir, "mailbox.jsonl"))
		},
		"public lock": func(test *testing.T, service *Service) {
			makePublic(test, filepath.Join(service.Box.Dir, "mailbox.lock"))
		},
		"roster symlink": func(test *testing.T, service *Service) {
			makeSymlink(test, filepath.Join(service.Box.Dir, "roster.json"))
		},
		"mailbox symlink": func(test *testing.T, service *Service) {
			makeSymlink(test, filepath.Join(service.Box.Dir, "mailbox.jsonl"))
		},
		"lock symlink": func(test *testing.T, service *Service) {
			makeSymlink(test, filepath.Join(service.Box.Dir, "mailbox.lock"))
		},
		"roster hardlink": func(test *testing.T, service *Service) {
			makeHardlink(test, filepath.Join(service.Box.Dir, "roster.json"))
		},
		"mailbox hardlink": func(test *testing.T, service *Service) {
			makeHardlink(test, filepath.Join(service.Box.Dir, "mailbox.jsonl"))
		},
		"lock hardlink": func(test *testing.T, service *Service) {
			makeHardlink(test, filepath.Join(service.Box.Dir, "mailbox.lock"))
		},
	}
	for name, plant := range cases {
		test.Run(name, func(test *testing.T) {
			service, hermes, _ := nativeReplyService(test)
			plant(test, service)
			_, err := service.PrepareNativeReply(context.Background(), hermes, "missing", "render")
			if err == nil {
				test.Fatal("unsafe private object was accepted")
			}
		})
	}
}

func makePublic(test *testing.T, path string) {
	test.Helper()
	if err := os.Chmod(path, 0o644); err != nil && !errors.Is(err, os.ErrNotExist) {
		test.Fatal(err)
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			test.Fatal(err)
		}
	} else if err != nil {
		test.Fatal(err)
	}
}

func makeSymlink(test *testing.T, path string) {
	test.Helper()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		test.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(test.TempDir(), "target"), path); err != nil {
		test.Fatal(err)
	}
}

func makeHardlink(test *testing.T, path string) {
	test.Helper()
	other := filepath.Join(test.TempDir(), "other")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		test.Fatal(err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		test.Fatal(err)
	}
	if err := os.Link(other, path); err != nil {
		test.Fatal(err)
	}
}

func TestLegacySendStillUsesPublicMailboxAndShellReply(test *testing.T) {
	adapter := &fakeAdapter{}
	service, _ := newTestService(test, adapter)
	result, err := service.Send(context.Background(), "coord", "pi-1", "legacy", PriorityNext)
	if err != nil {
		test.Fatal(err)
	}
	if result.Status != StatusSent || adapter.calls != 1 {
		test.Fatalf("legacy send result = %#v, calls = %d", result, adapter.calls)
	}
	if !strings.Contains(adapter.texts[0], "surface send coord '<text>'") {
		test.Fatalf("legacy render lost shell reply hint: %q", adapter.texts[0])
	}
}

func TestQueuedNativeReplyRejectsReplacedOriginBeforeAdapter(test *testing.T) {
	service, hermes, adapter := nativeReplyService(test)
	source := nativeSourceForInput(test, service, hermes, nativeInput(service, "origin-replaced-input", "coord", "coord-run"))
	if _, err := service.EnqueueNativeReply(context.Background(), hermes, source, "answer"); err != nil {
		test.Fatal(err)
	}
	replaced := hermes
	replaced.RunID = "hermes-new-run"
	replaced.ThreadID = "hermes-new-session"
	if err := service.Roster.Put(replaced); err != nil {
		test.Fatal(err)
	}
	if _, err := service.Flush(context.Background()); err != nil {
		test.Fatal(err)
	}
	entries, err := service.Messages(nil)
	if err != nil {
		test.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Msg.ReplyTo == source.InputID {
			if entry.Status != StatusFailed || !strings.Contains(entry.Detail, "stale") {
				test.Fatalf("stale native reply entry = %#v", entry)
			}
			if adapter.calls != 0 {
				test.Fatalf("stale native reply invoked adapter %d times", adapter.calls)
			}
			return
		}
	}
	test.Fatal("queued native reply was not found")
}

func messagePointer(message Message) *Message {
	return &message
}
