//go:build unix

package mail

import (
	"strings"
	"testing"
	"time"

	"github.com/cameronsjo/forgectl/internal/surface/activity"
	"github.com/cameronsjo/forgectl/internal/surface/hermes"
)

func TestHermesInspectionRequiresExactIdentityAndBoundedEvidence(test *testing.T) {
	valid := func() *hermes.Inspection {
		now := time.Now().UTC()
		since := now.Add(-time.Second)
		return &hermes.Inspection{Provider: "openai-codex", Model: "test-model", Mode: "default", ModelEvidence: "native-startup-handshake", Idle: true, Turn: &hermes.TurnObservation{MessageID: "message-1", Status: "completed", ObservedAt: now}, Activity: &activity.Snapshot{Version: activity.Version, Source: activity.SourceHermes, Coverage: activity.CoverageCurrentProcess, ObservedAt: now, Since: &since, Spans: []activity.Span{}}}
	}
	path, requests := startHermes(test, shortTempDir(test), func(hermesRequest) hermesResponse {
		return hermesResponse{V: 1, OK: true, Inspection: valid()}
	})
	observation, err := (HermesAdapter{}).Inspect(test.Context(), hermesWorker(path))
	if err != nil || observation.Provider != "openai-codex" || observation.Turn == nil || observation.Turn.Status != "completed" || observation.Activity == nil || observation.Activity.Source != activity.SourceHermes {
		test.Fatalf("inspection = %#v, %v", observation, err)
	}
	request := <-requests
	if request.Type != "inspect" || request.Text != "" || request.RunID != "run-1" || request.SessionID != "session-1" {
		test.Fatalf("inspect sent mutation or wrong identity: %#v", request)
	}
	for _, scenario := range []struct {
		name   string
		mutate func(*hermesResponse)
	}{
		{"wrong run", func(response *hermesResponse) { response.RunID = "other-run" }},
		{"wrong session", func(response *hermesResponse) { response.SessionID = "other-session" }},
		{"missing inspection", func(response *hermesResponse) { response.Inspection = nil }},
		{"unknown source", func(response *hermesResponse) { response.Inspection.ModelEvidence = "roster" }},
		{"control model", func(response *hermesResponse) { response.Inspection.Model = "model\u202e" }},
		{"oversized model", func(response *hermesResponse) { response.Inspection.Model = strings.Repeat("x", 257) }},
		{"missing message", func(response *hermesResponse) { response.Inspection.Turn.MessageID = "" }},
		{"unknown turn", func(response *hermesResponse) { response.Inspection.Turn.Status = "sent" }},
		{"zero time", func(response *hermesResponse) { response.Inspection.Turn.ObservedAt = time.Time{} }},
		{"wrong activity source", func(response *hermesResponse) { response.Inspection.Activity.Source = activity.SourceCodex }},
		{"invalid activity", func(response *hermesResponse) { response.Inspection.Activity.Since = nil }},
		{"conflicting outcome", func(response *hermesResponse) { response.Unconfirmed = true }},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			path, _ := startHermes(test, shortTempDir(test), func(hermesRequest) hermesResponse {
				response := hermesResponse{V: 1, OK: true, Inspection: valid()}
				scenario.mutate(&response)
				return response
			})
			if _, err := (HermesAdapter{}).Inspect(test.Context(), hermesWorker(path)); err == nil {
				test.Fatal("invalid native inspection accepted")
			}
		})
	}
}
