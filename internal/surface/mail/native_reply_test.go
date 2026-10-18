package mail

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nativeReplyService(test *testing.T) (*Service, Worker, *fakeAdapter) {
	test.Helper()
	dir := test.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		test.Fatal(err)
	}
	worktree, err := os.Getwd()
	if err != nil {
		test.Fatal(err)
	}
	worktree = filepath.Clean(worktree)
	coordinator := Worker{Name: "coord", Harness: HarnessClaude, Worktree: worktree, Coordinator: true, RunID: "coord-run", Socket: filepath.Join(dir, "coord.sock"), ThreadID: "coord-session"}
	hermes := Worker{Name: "hermes", Harness: HarnessHermes, Worktree: worktree, RunID: "hermes-run", Socket: filepath.Join(dir, "hermes.sock"), ThreadID: "hermes-session"}
	roster := FileRoster{Dir: dir}
	if err := roster.Put(coordinator); err != nil {
		test.Fatal(err)
	}
	if err := roster.Put(hermes); err != nil {
		test.Fatal(err)
	}
	adapter := &fakeAdapter{}
	service := &Service{
		Box:         Mailbox{Dir: dir, Private: true},
		Roster:      roster,
		Adapters:    map[Harness]Adapter{HarnessClaude: adapter, HarnessHermes: adapter},
		Policy:      DefaultPolicy(),
		ReplyBinary: filepath.Join(dir, "forgectl"),
	}
	return service, hermes, adapter
}

func enqueueNativeInput(test *testing.T, service *Service, input Message) {
	test.Helper()
	err := service.Box.Locked(func(tx *Tx) error {
		return tx.Enqueue(input, input.CreatedAt)
	})
	if err != nil {
		test.Fatal(err)
	}
}

func TestPrepareAndEnqueueNativeReplyIsIdempotent(test *testing.T) {
	service, hermes, adapter := nativeReplyService(test)
	input := Message{
		V: 1, ID: "input-1", From: "coord", To: "hermes", FromRunID: "coord-run", ToRunID: "hermes-run",
		Body: "request", Priority: PriorityNext, CreatedAt: service.now(), ReplyBinary: service.ReplyBinary,
	}
	enqueueNativeInput(test, service, input)
	source, err := service.PrepareNativeReply(context.Background(), hermes, input.ID, Render(input))
	if err != nil {
		test.Fatal(err)
	}
	if source.Recipient != input.From || source.RecipientRunID != input.FromRunID {
		test.Fatalf("source destination = %s/%s, want %s/%s", source.Recipient, source.RecipientRunID, input.From, input.FromRunID)
	}
	if strings.Contains(source.Prompt, "surface send") || !strings.Contains(source.Prompt, `"forgectl_reply"`) || !strings.Contains(source.Prompt, input.ID) || !strings.Contains(source.Prompt, "not by your operator") {
		test.Fatalf("native prompt = %q", source.Prompt)
	}
	first, err := service.EnqueueNativeReply(context.Background(), hermes, source, "answer")
	if err != nil {
		test.Fatal(err)
	}
	second, err := service.EnqueueNativeReply(context.Background(), hermes, source, "answer")
	if err != nil {
		test.Fatal(err)
	}
	if first.ID == "" || first.ID != second.ID || first.Status != StatusQueued || second.Status != StatusQueued {
		test.Fatalf("results = %#v and %#v", first, second)
	}
	if adapter.calls != 0 {
		test.Fatalf("enqueue invoked adapter %d times", adapter.calls)
	}
}

func TestEnqueueNativeReplyRejectsContradictorySource(test *testing.T) {
	service, hermes, _ := nativeReplyService(test)
	input := Message{
		V: 1, ID: "input-2", From: "coord", To: "hermes", FromRunID: "coord-run", ToRunID: "hermes-run",
		Body: "request", Priority: PriorityNext, CreatedAt: service.now(), ReplyBinary: service.ReplyBinary,
	}
	enqueueNativeInput(test, service, input)
	source, err := service.PrepareNativeReply(context.Background(), hermes, input.ID, Render(input))
	if err != nil {
		test.Fatal(err)
	}
	for name, mutate := range map[string]func(*NativeReplySource){
		"digest":    func(value *NativeReplySource) { value.InputDigest = "different" },
		"recipient": func(value *NativeReplySource) { value.Recipient = "other" },
		"prompt":    func(value *NativeReplySource) { value.Prompt += "changed" },
	} {
		test.Run(name, func(test *testing.T) {
			mutated := source
			mutate(&mutated)
			if _, err := service.EnqueueNativeReply(context.Background(), hermes, mutated, "answer"); err == nil {
				test.Fatal("contradictory source was accepted")
			}
		})
	}
}

func TestNativeReplyRequiresPrivateStore(test *testing.T) {
	service, hermes, _ := nativeReplyService(test)
	service.Box.Private = false
	if _, err := service.PrepareNativeReply(context.Background(), hermes, "input", "text"); !errors.Is(err, ErrNativeReplyUnavailable) {
		test.Fatalf("PrepareNativeReply error = %v", err)
	}
}
