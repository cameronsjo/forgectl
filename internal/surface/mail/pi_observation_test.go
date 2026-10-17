package mail

import (
	"context"
	"testing"
	"time"
)

func TestPiInspectReturnsCurrentNativeMetadata(test *testing.T) {
	tools := []string{"read", "bash", "find"}
	path, got := startPi(test, shortTempDir(test), func(req piRequest) piResponse {
		if req.V != piIdentityProtocol || req.Type != "inspect" || req.RunID != "run-1" || req.SessionID != "session-1" {
			return piResponse{V: piIdentityProtocol, OK: false, Error: "unexpected inspect identity"}
		}
		return piResponse{
			V:           piIdentityProtocol,
			OK:          true,
			Known:       true,
			RunID:       req.RunID,
			SessionID:   req.SessionID,
			Provider:    "openai-codex",
			ModelID:     "gpt-5.6-sol",
			ActiveTools: tools,
			SessionPath: "/private/tmp/pi-session.jsonl",
			Turn:        &PiTurnObservation{ID: "native-entry-4", Status: "failed", ObservedAt: time.Now().UTC(), Error: "error"},
		}
	})
	adapter := PiAdapter{Timeout: 2 * time.Second}
	observation, err := adapter.Inspect(context.Background(), versionedPiWorker(path))
	if err != nil {
		test.Fatal(err)
	}
	if !observation.Known || observation.Provider != "openai-codex" || observation.ModelID != "gpt-5.6-sol" || observation.SessionID != "session-1" || observation.SessionPath == "" {
		test.Fatalf("observation = %#v", observation)
	}
	if len(observation.ActiveTools) != len(tools) || observation.ActiveTools[0] != "read" {
		test.Fatalf("active tools = %#v", observation.ActiveTools)
	}
	if observation.Turn == nil || observation.Turn.Status != "failed" || observation.Turn.ID != "native-entry-4" {
		test.Fatalf("turn = %#v", observation.Turn)
	}
	if request := <-got; request.Type != "inspect" {
		test.Fatalf("request = %#v", request)
	}
}

func TestPiInspectUnknownMetadataStaysUnknown(test *testing.T) {
	path, _ := startPi(test, shortTempDir(test), func(req piRequest) piResponse {
		return piResponse{V: piIdentityProtocol, OK: false, Unknown: true, RunID: req.RunID, SessionID: req.SessionID, Error: "model unavailable"}
	})
	observation, err := (PiAdapter{Timeout: time.Second}).Inspect(context.Background(), versionedPiWorker(path))
	if err != nil {
		test.Fatal(err)
	}
	if observation.Known {
		test.Fatalf("unknown observation reported known: %#v", observation)
	}
}

func TestPiInspectRequiresRunAndSessionIdentity(test *testing.T) {
	adapter := PiAdapter{Timeout: time.Second}
	for _, worker := range []Worker{
		{Name: "pi-1", Harness: HarnessPi, ThreadID: "session-1"},
		{Name: "pi-1", Harness: HarnessPi, RunID: "run-1"},
	} {
		if _, err := adapter.Inspect(context.Background(), worker); !IsUnconfirmed(err) {
			test.Fatalf("worker %#v: err = %v, want unconfirmed", worker, err)
		}
	}
}

func TestPiInspectRejectsInvalidTurnStatus(test *testing.T) {
	path, _ := startPi(test, shortTempDir(test), func(req piRequest) piResponse {
		return piResponse{V: piIdentityProtocol, OK: true, Known: true, RunID: req.RunID, SessionID: req.SessionID, Provider: "openai-codex", ModelID: "gpt", ActiveTools: []string{"read"}, SessionPath: "/tmp/session", Turn: &PiTurnObservation{Status: "running"}}
	})
	_, err := (PiAdapter{Timeout: time.Second}).Inspect(context.Background(), versionedPiWorker(path))
	if err == nil {
		test.Fatal("invalid turn status was accepted")
	}
}

func TestPiInspectTreatsUnsafeNativeMetadataAsUnknown(test *testing.T) {
	cases := []struct {
		name      string
		make      func() piResponse
		wantError bool
	}{
		{name: "provider control", make: func() piResponse {
			return piResponse{Provider: "openai\n", ModelID: "gpt", ActiveTools: []string{"read"}, SessionPath: "/tmp/session"}
		}},
		{name: "model bidi", make: func() piResponse {
			return piResponse{Provider: "openai", ModelID: "gpt\u202E", ActiveTools: []string{"read"}, SessionPath: "/tmp/session"}
		}},
		{name: "relative session path", make: func() piResponse {
			return piResponse{Provider: "openai", ModelID: "gpt", ActiveTools: []string{"read"}, SessionPath: "session"}
		}},
		{name: "tool control", make: func() piResponse {
			return piResponse{Provider: "openai", ModelID: "gpt", ActiveTools: []string{"re\tad"}, SessionPath: "/tmp/session"}
		}},
		{name: "too many tools", make: func() piResponse {
			tools := make([]string, 65)
			for index := range tools {
				tools[index] = "read"
			}
			return piResponse{Provider: "openai", ModelID: "gpt", ActiveTools: tools, SessionPath: "/tmp/session"}
		}},
		{name: "missing turn identity", wantError: true, make: func() piResponse {
			return piResponse{Provider: "openai", ModelID: "gpt", ActiveTools: []string{"read"}, SessionPath: "/tmp/session", Turn: &PiTurnObservation{Status: "completed", ObservedAt: time.Now().UTC()}}
		}},
		{name: "missing turn timestamp", wantError: true, make: func() piResponse {
			return piResponse{Provider: "openai", ModelID: "gpt", ActiveTools: []string{"read"}, SessionPath: "/tmp/session", Turn: &PiTurnObservation{ID: "entry-1", Status: "completed"}}
		}},
		{name: "raw failure text", wantError: true, make: func() piResponse {
			return piResponse{Provider: "openai", ModelID: "gpt", ActiveTools: []string{"read"}, SessionPath: "/tmp/session", Turn: &PiTurnObservation{ID: "entry-1", Status: "failed", ObservedAt: time.Now().UTC(), Error: "rate limited"}}
		}},
	}
	for _, observationCase := range cases {
		test.Run(observationCase.name, func(subtest *testing.T) {
			path, _ := startPi(subtest, shortTempDir(subtest), func(req piRequest) piResponse {
				response := observationCase.make()
				response.V = piIdentityProtocol
				response.OK = true
				response.Known = true
				response.RunID = req.RunID
				response.SessionID = req.SessionID
				return response
			})
			observation, err := (PiAdapter{Timeout: time.Second}).Inspect(context.Background(), versionedPiWorker(path))
			if (err != nil) != observationCase.wantError {
				subtest.Fatalf("inspect error = %v, wantError=%t", err, observationCase.wantError)
			}
			if observation.Known {
				subtest.Fatalf("unsafe response reported known: %#v", observation)
			}
		})
	}
}
