package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/cameronsjo/forgectl/internal/module"
	"github.com/cameronsjo/forgectl/internal/surface/hermes"
	"github.com/cameronsjo/forgectl/internal/surface/mail"
)

func configureHermesReplies(config *hermes.Config, deps module.Deps, registered mail.Worker) error {
	getenv := func(name string) string {
		if name == mail.EnvLedger {
			return config.Ledger
		}
		return os.Getenv(name)
	}
	session, err := openMailSession(deps, getenv)
	if err != nil {
		return err
	}
	if session == nil || session.svc == nil || filepath.Clean(session.ledger) != filepath.Clean(config.Ledger) || filepath.Clean(session.svc.Box.Dir) != filepath.Clean(config.Ledger) {
		return errors.New("native reply service does not match the enrolled ledger")
	}
	bindHermesReplies(config, session.svc, registered)
	return nil
}

func bindHermesReplies(config *hermes.Config, service *mail.Service, registered mail.Worker) {
	service.Box.Private = true
	config.NativeReplies = true
	config.PrepareReply = func(ctx context.Context, input hermes.Request) (hermes.ReplyPreparation, error) {
		expected := registered
		expected.ThreadID = input.SessionID
		if input.RunID != expected.RunID || input.SessionID == "" || registered.ThreadID != "" && input.SessionID != registered.ThreadID {
			return hermes.ReplyPreparation{}, errors.New("native reply input does not match the original run and session")
		}
		source, err := service.PrepareNativeReply(ctx, expected, input.ID, input.Text)
		if err != nil {
			return hermes.ReplyPreparation{}, err
		}
		return hermes.ReplyPreparation{
			Prompt: source.Prompt, SourceDigest: source.InputDigest,
			Recipient: source.Recipient, RecipientRunID: source.RecipientRunID,
		}, nil
	}
	config.PublishReply = func(ctx context.Context, publication hermes.ReplyPublication) (hermes.ReplyPublicationResult, error) {
		expected := registered
		expected.ThreadID = publication.Input.SessionID
		if publication.Input.RunID != expected.RunID || publication.Input.SessionID == "" || registered.ThreadID != "" && publication.Input.SessionID != registered.ThreadID {
			return hermes.ReplyPublicationResult{}, errors.New("native reply publication does not match the original run and session")
		}
		source := mail.NativeReplySource{
			InputID: publication.Input.ID, InputDigest: publication.Prepared.SourceDigest,
			Recipient: publication.Prepared.Recipient, RecipientRunID: publication.Prepared.RecipientRunID,
			SessionID: publication.Input.SessionID, RunID: publication.Input.RunID, Prompt: publication.Prepared.Prompt,
		}
		if _, err := service.EnqueueNativeReply(ctx, expected, source, publication.Body); err != nil {
			return hermes.ReplyPublicationResult{}, err
		}
		if _, err := service.Flush(ctx); err != nil {
			return hermes.ReplyPublicationResult{}, err
		}
		result, err := service.EnqueueNativeReply(ctx, expected, source, publication.Body)
		if err != nil {
			return hermes.ReplyPublicationResult{}, err
		}
		return hermes.ReplyPublicationResult{MessageID: result.ID, Status: string(result.Status)}, nil
	}
}
