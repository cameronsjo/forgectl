package mail

import (
	"context"
	"path/filepath"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cameronsjo/forgectl/internal/surface/activity"
	"github.com/cameronsjo/forgectl/internal/termsafe"
)

// PiTurnObservation is the last settled turn for which Pi exposed an exact
// assistant stop reason and native session entry ID. A
// missing observation is intentional: agent_settled alone does not distinguish
// completion from failure.
type PiTurnObservation struct {
	ID         string    `json:"id"`
	Status     string    `json:"status"`
	ObservedAt time.Time `json:"observed_at"`
	Error      string    `json:"error,omitempty"`
}

// PiObservation is a read-only snapshot of Pi's current native session. Known
// is false when the extension cannot prove all required native metadata.
type PiObservation struct {
	Known       bool               `json:"known"`
	Provider    string             `json:"provider,omitempty"`
	ModelID     string             `json:"model_id,omitempty"`
	ActiveTools []string           `json:"active_tools,omitempty"`
	SessionID   string             `json:"session_id,omitempty"`
	SessionPath string             `json:"session_path,omitempty"`
	Turn        *PiTurnObservation `json:"turn,omitempty"`
	Activity    *activity.Snapshot `json:"activity,omitempty"`
}

// Inspect asks the identity-bound Pi extension for current native metadata.
// It never sends a prompt or changes the session. A response that is valid
// but cannot prove the metadata is returned as Known=false with no error;
// transport and protocol failures remain errors.
func (adapter PiAdapter) Inspect(ctx context.Context, worker Worker) (PiObservation, error) {
	if worker.RunID == "" || worker.ThreadID == "" {
		return PiObservation{}, Unconfirmed("pi inspection requires a run and native session identity")
	}
	resp, err := adapter.call(ctx, worker, piRequest{
		V:         piIdentityProtocol,
		Type:      "inspect",
		RunID:     worker.RunID,
		SessionID: worker.ThreadID,
	}, false)
	if err != nil {
		return PiObservation{}, err
	}
	if resp.Unknown || !resp.OK || !resp.Known || resp.Retry || resp.Unconfirmed {
		return PiObservation{}, nil
	}
	if resp.Activity != nil {
		if resp.Activity.Source != activity.SourcePi {
			return PiObservation{}, Unconfirmed("pi inspection returned activity from another source")
		}
		if err := activity.Validate(*resp.Activity); err != nil {
			return PiObservation{}, Unconfirmed("pi inspection returned invalid native activity")
		}
	}
	if !safePiObservationString(resp.Provider, 256) || !safePiObservationString(resp.ModelID, 256) || resp.SessionID != worker.ThreadID || !safePiObservationPath(resp.SessionPath) || resp.ActiveTools == nil || len(resp.ActiveTools) > 64 {
		return PiObservation{}, nil
	}
	for _, tool := range resp.ActiveTools {
		if !safePiObservationString(tool, 128) {
			return PiObservation{}, nil
		}
	}
	if resp.Turn != nil {
		if resp.Turn.Status != "completed" && resp.Turn.Status != "failed" {
			return PiObservation{}, Unconfirmed("pi inspection returned an invalid turn status")
		}
		if !threadPattern.MatchString(resp.Turn.ID) || resp.Turn.ObservedAt.IsZero() {
			return PiObservation{}, Unconfirmed("pi inspection returned an invalid turn identity or timestamp")
		}
		if resp.Turn.Status == "completed" && resp.Turn.Error != "" {
			return PiObservation{}, Unconfirmed("pi inspection returned a completion error category")
		}
		if resp.Turn.Status == "failed" && resp.Turn.Error != "error" && resp.Turn.Error != "aborted" {
			return PiObservation{}, Unconfirmed("pi inspection returned an invalid failure category")
		}
	}
	return PiObservation{
		Known:       true,
		Provider:    resp.Provider,
		ModelID:     resp.ModelID,
		ActiveTools: append([]string(nil), resp.ActiveTools...),
		SessionID:   resp.SessionID,
		SessionPath: resp.SessionPath,
		Turn:        resp.Turn,
		Activity:    resp.Activity,
	}, nil
}

func safePiObservationString(value string, maximum int) bool {
	if value == "" || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.In(character, unicode.Cf) || termsafe.IsInvisibleRune(character) || termsafe.IsUnsafeTerminalRune(character) {
			return false
		}
	}
	return true
}

func safePiObservationPath(path string) bool {
	return filepath.IsAbs(path) && safePiObservationString(path, 4096)
}
