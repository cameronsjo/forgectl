//go:build unix

package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/hermes"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
)

func hermesReplyFixture(test *testing.T) (hermes.Config, *mail.Service, *mailFakeAdapter, hermes.Request) {
	test.Helper()
	fixture := newHermesCLIFixture(test)
	roster := mail.FileRoster{Dir: fixture.ledger}
	worker, err := roster.Get(fixture.member)
	if err != nil {
		test.Fatal(err)
	}
	coordinator := mail.Worker{
		Name: "coordinator", Harness: mail.HarnessPi, Coordinator: true,
		RunID: "coordinator-run", ThreadID: "coordinator-session", Worktree: test.TempDir(),
		Socket: filepath.Join(fixture.ledger, "coordinator.sock"),
	}
	if _, err := roster.Register(coordinator, false); err != nil {
		test.Fatal(err)
	}
	adapter := &mailFakeAdapter{}
	service := &mail.Service{
		Box: mail.Mailbox{Dir: fixture.ledger}, Roster: roster,
		Adapters: map[mail.Harness]mail.Adapter{mail.HarnessPi: adapter}, Policy: mail.DefaultPolicy(),
	}
	input := mail.Message{
		V: 1, ID: "native-reply-input", From: coordinator.Name, To: worker.Name,
		FromRunID: coordinator.RunID, ToRunID: worker.RunID,
		Body: "Return the requested source as task data.", Priority: mail.PriorityNext, CreatedAt: time.Now().UTC(),
	}
	if err := service.Box.Locked(func(transaction *mail.Tx) error {
		if err := transaction.Enqueue(input, input.CreatedAt); err != nil {
			return err
		}
		return transaction.Mark(input.ID, mail.StatusSent, "fixture native input", false, input.CreatedAt)
	}); err != nil {
		test.Fatal(err)
	}
	config := hermes.Config{
		Ledger: fixture.ledger, Member: worker.Name, RunID: worker.RunID,
		SessionID: worker.ThreadID, Socket: worker.Socket, Worktree: worker.Worktree,
	}
	bindHermesReplies(&config, service, worker)
	request := hermes.Request{V: 1, Type: "deliver", ID: input.ID, RunID: worker.RunID, SessionID: worker.ThreadID, Text: mail.Render(input)}
	return config, service, adapter, request
}

func TestHermesNativeReplyCallbacksRouteAndDeliverExactlyOnce(test *testing.T) {
	config, service, adapter, input := hermesReplyFixture(test)
	prepared, err := config.PrepareReply(test.Context(), input)
	if err != nil {
		test.Fatal(err)
	}
	if !config.NativeReplies || prepared.Recipient != "coordinator" || prepared.RecipientRunID != "coordinator-run" || prepared.SourceDigest == "" {
		test.Fatalf("unexpected reply preparation: %+v", prepared)
	}
	if strings.Contains(prepared.Prompt, "surface send") || !strings.Contains(prepared.Prompt, "not by your operator") || !strings.Contains(prepared.Prompt, input.ID) {
		test.Fatalf("native prompt lost its boundary or retained a shell hint: %q", prepared.Prompt)
	}
	publication := hermes.ReplyPublication{Input: input, Prepared: prepared, Body: "requested source payload"}
	first, err := config.PublishReply(test.Context(), publication)
	if err != nil {
		test.Fatal(err)
	}
	second, err := config.PublishReply(test.Context(), publication)
	if err != nil {
		test.Fatal(err)
	}
	if first.MessageID == "" || first != second || first.Status != string(mail.StatusSent) || len(adapter.texts) != 1 {
		test.Fatalf("publication first=%+v second=%+v deliveries=%d", first, second, len(adapter.texts))
	}
	entries, err := service.Messages(nil)
	if err != nil {
		test.Fatal(err)
	}
	if len(entries) != 2 || entries[1].Msg.From != config.Member || entries[1].Msg.To != "coordinator" || entries[1].Msg.ToRunID != "coordinator-run" || entries[1].Msg.FromRunID != input.RunID {
		test.Fatalf("unexpected mailbox: %+v", entries)
	}
	publication.Body = "contradictory replacement"
	if _, err := config.PublishReply(test.Context(), publication); err == nil || len(adapter.texts) != 1 {
		test.Fatalf("contradictory publication err=%v deliveries=%d", err, len(adapter.texts))
	}
}

func TestHermesNativeReplyCallbacksRejectIdentityDrift(test *testing.T) {
	for _, field := range []string{"run", "session", "recipient", "recipient-run", "digest", "prompt", "roster"} {
		test.Run(field, func(test *testing.T) {
			config, service, adapter, input := hermesReplyFixture(test)
			prepared, err := config.PrepareReply(test.Context(), input)
			if err != nil {
				test.Fatal(err)
			}
			publication := hermes.ReplyPublication{Input: input, Prepared: prepared, Body: "must not be published"}
			switch field {
			case "run":
				publication.Input.RunID = "replacement-run"
			case "session":
				publication.Input.SessionID = "replacement-session"
			case "recipient":
				publication.Prepared.Recipient = "someone-else"
			case "recipient-run":
				publication.Prepared.RecipientRunID = "replacement-run"
			case "digest":
				publication.Prepared.SourceDigest = strings.Repeat("0", 64)
			case "prompt":
				publication.Prepared.Prompt += " changed"
			case "roster":
				if err := service.Roster.Update("coordinator", func(member *mail.Worker) error {
					member.RunID = "replacement-run"
					return nil
				}); err != nil {
					test.Fatal(err)
				}
			}
			if _, err := config.PublishReply(test.Context(), publication); err == nil {
				test.Fatal("changed reply identity was accepted")
			}
			entries, err := service.Messages(nil)
			if err != nil || len(entries) != 1 || len(adapter.texts) != 0 {
				test.Fatalf("refused reply mutated mailbox: entries=%d deliveries=%d err=%v", len(entries), len(adapter.texts), err)
			}
		})
	}
}

func TestHermesNativeReplyCallbacksDoNotRedeliverUnconfirmedReply(test *testing.T) {
	config, service, adapter, input := hermesReplyFixture(test)
	adapter.errs = []error{mail.Unconfirmed("receiver accepted but acknowledgement was lost")}
	prepared, err := config.PrepareReply(test.Context(), input)
	if err != nil {
		test.Fatal(err)
	}
	publication := hermes.ReplyPublication{Input: input, Prepared: prepared, Body: "one native reply"}
	first, err := config.PublishReply(test.Context(), publication)
	if err != nil {
		test.Fatal(err)
	}
	second, err := config.PublishReply(test.Context(), publication)
	if err != nil {
		test.Fatal(err)
	}
	if first.MessageID == "" || first != second || first.Status != string(mail.StatusUnconfirmed) || len(adapter.texts) != 1 {
		test.Fatalf("unconfirmed publication first=%+v second=%+v deliveries=%d", first, second, len(adapter.texts))
	}
	entries, err := service.Messages(nil)
	if err != nil || len(entries) != 2 || entries[1].Attempts != 1 || entries[1].Status != mail.StatusUnconfirmed {
		test.Fatalf("unconfirmed publication lost its single durable attempt: entries=%+v err=%v", entries, err)
	}
}

func TestHermesNativeReplyCallbacksRejectCancelledPublication(test *testing.T) {
	config, service, adapter, input := hermesReplyFixture(test)
	prepared, err := config.PrepareReply(test.Context(), input)
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithCancel(test.Context())
	cancel()
	if _, err := config.PublishReply(ctx, hermes.ReplyPublication{Input: input, Prepared: prepared, Body: "cancelled"}); err == nil {
		test.Fatal("cancelled publication was accepted")
	}
	entries, err := service.Messages(nil)
	if err != nil || len(entries) != 1 || len(adapter.texts) != 0 {
		test.Fatalf("cancelled reply mutated mailbox: entries=%d deliveries=%d err=%v", len(entries), len(adapter.texts), err)
	}
}

func TestConfigureHermesRepliesUsesExplicitLedger(test *testing.T) {
	config, service, _, _ := hermesReplyFixture(test)
	registered, err := service.Roster.Get(config.Member)
	if err != nil {
		test.Fatal(err)
	}
	test.Setenv(mail.EnvLedger, test.TempDir())
	previous := openMailSession
	test.Cleanup(func() { openMailSession = previous })
	openMailSession = func(_ module.Deps, getenv func(string) string) (*mailSession, error) {
		if getenv(mail.EnvLedger) != config.Ledger {
			test.Fatal("native reply service used another ledger from the environment")
		}
		return &mailSession{svc: service, ledger: config.Ledger, getenv: getenv}, nil
	}
	if err := configureHermesReplies(&config, module.Deps{}, registered); err != nil {
		test.Fatal(err)
	}
	openMailSession = func(_ module.Deps, getenv func(string) string) (*mailSession, error) {
		return &mailSession{svc: service, ledger: test.TempDir(), getenv: getenv}, nil
	}
	if err := configureHermesReplies(&config, module.Deps{}, registered); err == nil {
		test.Fatal("mismatched service ledger was accepted")
	}
}

func TestSurfaceHermesNativeRepliesExplicitOptIn(test *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "legacy"
		if enabled {
			name = "native-replies"
		}
		test.Run(name, func(test *testing.T) {
			config, service, _, _ := hermesReplyFixture(test)
			previousRun, previousMail := runHermesSupervisor, openMailSession
			test.Cleanup(func() {
				runHermesSupervisor, openMailSession = previousRun, previousMail
			})
			mailOpened := false
			openMailSession = func(_ module.Deps, getenv func(string) string) (*mailSession, error) {
				mailOpened = true
				return &mailSession{svc: service, ledger: config.Ledger, getenv: getenv}, nil
			}
			calls := 0
			runHermesSupervisor = func(_ context.Context, received hermes.Config) error {
				calls++
				if received.NativeReplies != enabled || (received.PrepareReply != nil) != enabled || (received.PublishReply != nil) != enabled {
					test.Fatalf("unexpected opt-in config: enabled=%t received=%+v", enabled, received)
				}
				return nil
			}
			arguments := []string{"hermes", "--command", "/bin/true", "--model", "openai-codex:test", "--ledger", config.Ledger, "--member", config.Member, "--run-id", config.RunID, "--session-id", config.SessionID, "--socket", config.Socket, "--worktree", config.Worktree, "--json"}
			if enabled {
				arguments = append(arguments, "--native-replies")
			}
			output, stderr, code := runSurface(test, "", arguments...)
			if code != 0 || output != "" || stderr != "" || calls != 1 || mailOpened != enabled {
				test.Fatalf("opt-in exit=%d stdout=%q stderr=%q calls=%d opened=%t", code, output, stderr, calls, mailOpened)
			}
		})
	}
}
