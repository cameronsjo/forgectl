package mail

import (
	"context"
	"errors"
	"unicode"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/surface/activity"
	"github.com/cameronsjo/forgectl/internal/surface/hermes"
)

func (adapter HermesAdapter) Inspect(ctx context.Context, worker Worker) (hermes.Inspection, error) {
	response, err := adapter.call(ctx, worker, hermesRequest{V: 1, Type: "inspect", RunID: worker.RunID, SessionID: worker.ThreadID})
	if err != nil {
		return hermes.Inspection{}, err
	}
	if !response.OK || response.Inspection == nil || response.Retry || response.Unconfirmed {
		return hermes.Inspection{}, errors.New("hermes native inspection is unavailable")
	}
	observation := *response.Inspection
	if !safeHermesObservationToken(observation.Provider, 128) || !safeHermesObservationToken(observation.Model, 256) || observation.Mode != "default" || observation.ModelEvidence != "native-startup-handshake" {
		return hermes.Inspection{}, errors.New("hermes native inspection metadata is invalid")
	}
	if observation.Turn != nil {
		if !threadPattern.MatchString(observation.Turn.MessageID) || observation.Turn.ObservedAt.IsZero() {
			return hermes.Inspection{}, errors.New("hermes turn observation has invalid identity or time")
		}
		switch observation.Turn.Status {
		case "completed", "incomplete", "unknown":
		default:
			return hermes.Inspection{}, errors.New("hermes turn observation has invalid status")
		}
	}
	if observation.Activity != nil {
		if observation.Activity.Source != activity.SourceHermes {
			return hermes.Inspection{}, errors.New("hermes native activity has the wrong source")
		}
		if err := activity.Validate(*observation.Activity); err != nil {
			return hermes.Inspection{}, errors.New("hermes native activity is invalid")
		}
		cloned := activity.Clone(*observation.Activity)
		observation.Activity = &cloned
	}
	return observation, nil
}

func safeHermesObservationToken(value string, limit int) bool {
	if value == "" || len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.In(character, unicode.Cf) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}
