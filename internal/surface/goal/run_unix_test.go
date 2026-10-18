//go:build unix

package goal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cameronsjo/forgectl/internal/surface/mail"
)

func TestMessageKindRequiresExactGoalCorrelationAndDirection(t *testing.T) {
	goalID := "goal-1234567890abcdef12345678"
	valid := []struct {
		name     string
		verb     string
		from, to string
	}{
		{name: "assign", verb: "ASSIGN", from: "coordinator", to: "implementation"},
		{name: "fix", verb: "FIX", from: "coordinator", to: "implementation"},
		{name: "review", verb: "REVIEW", from: "coordinator", to: "reviewer"},
		{name: "result", verb: "RESULT", from: "implementation", to: "coordinator"},
		{name: "implementation question", verb: "QUESTION", from: "implementation", to: "coordinator"},
		{name: "reviewer question", verb: "QUESTION", from: "reviewer", to: "coordinator"},
		{name: "review pass", verb: "REVIEW_PASS", from: "reviewer", to: "coordinator"},
		{name: "review changes", verb: "REVIEW_CHANGES", from: "reviewer", to: "coordinator"},
		{name: "implementation answer", verb: "ANSWER", from: "coordinator", to: "implementation"},
		{name: "reviewer answer", verb: "ANSWER", from: "coordinator", to: "reviewer"},
	}
	for _, testCase := range valid {
		t.Run(testCase.name, func(t *testing.T) {
			message := mail.Message{From: testCase.from, To: testCase.to, Body: testCase.verb + " " + goalID + " payload"}
			kind, err := messageKind(message, goalID)
			if err != nil {
				t.Fatalf("messageKind(%q): %v", message.Body, err)
			}
			if kind != testCase.verb {
				t.Fatalf("messageKind = %q, want %q", kind, testCase.verb)
			}
		})
	}

	invalid := []struct {
		name     string
		body     string
		from, to string
	}{
		{name: "missing goal", body: "RESULT", from: "implementation", to: "coordinator"},
		{name: "goal prefix", body: "RESULT " + goalID + "-suffix payload", from: "implementation", to: "coordinator"},
		{name: "goal suffix", body: "RESULT x" + goalID + " payload", from: "implementation", to: "coordinator"},
		{name: "wrong goal", body: "RESULT goal-fedcba9876543210fedcba98 payload", from: "implementation", to: "coordinator"},
		{name: "unknown verb", body: "NOT_A_VERB " + goalID + " payload", from: "implementation", to: "coordinator"},
		{name: "assign reversed", body: "ASSIGN " + goalID + " payload", from: "implementation", to: "coordinator"},
		{name: "review reversed", body: "REVIEW " + goalID + " payload", from: "reviewer", to: "coordinator"},
		{name: "result reversed", body: "RESULT " + goalID + " payload", from: "coordinator", to: "implementation"},
		{name: "review pass reversed", body: "REVIEW_PASS " + goalID + " payload", from: "coordinator", to: "reviewer"},
		{name: "answer to coordinator", body: "ANSWER " + goalID + " payload", from: "implementation", to: "coordinator"},
	}
	for _, testCase := range invalid {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := messageKind(mail.Message{From: testCase.from, To: testCase.to, Body: testCase.body}, goalID)
			if err == nil {
				t.Fatalf("messageKind(%q) succeeded, want refusal", testCase.body)
			}
		})
	}
}

func TestConsumedRequiresExactRenderedUserMessageAndCompletedTurn(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	message := mail.Message{V: 1, ID: "message-1", From: "reviewer", To: "coordinator", Body: "REVIEW_PASS " + fixture.state.ID + " patch"}
	engine := newGoalTestExecution(fixture, Role{Name: "coordinator", NativeSessionID: "thread-coordinator"})
	rendered := mail.Render(message)

	cases := []struct {
		name       string
		content    string
		inputTurn  string
		completeID string
		want       bool
	}{
		{name: "exact but incomplete", content: rendered, inputTurn: "turn-incomplete", want: false},
		{name: "exact completed", content: rendered, inputTurn: "turn-complete", completeID: "turn-complete", want: true},
		{name: "substring completed", content: "prefix " + rendered + " suffix", inputTurn: "turn-substring", completeID: "turn-substring", want: false},
		{name: "completed different turn", content: rendered, inputTurn: "turn-input", completeID: "turn-other", want: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			engine.traces = map[string]*nativeTrace{}
			content, err := json.Marshal([]map[string]string{{"type": "text", "text": testCase.content}})
			if err != nil {
				t.Fatal(err)
			}
			engine.nativeEvent("coordinator", nativeEventJSON(t, "item/completed", "thread-coordinator", testCase.inputTurn, "", map[string]any{
				"type":    "userMessage",
				"content": json.RawMessage(content),
			}))
			if testCase.completeID != "" {
				engine.nativeEvent("coordinator", nativeEventJSON(t, "turn/completed", "thread-coordinator", testCase.completeID, "completed", nil))
			}
			if got := engine.consumed("coordinator", message); got != testCase.want {
				t.Fatalf("consumed = %t, want %t", got, testCase.want)
			}
		})
	}
}

func TestVerifyArtifactsDetectsTamperedPatchAndCheck(t *testing.T) {
	tests := []struct {
		name     string
		artifact string
		contents []byte
		tampered []byte
	}{
		{name: "patch", artifact: "result.patch", contents: []byte("diff --git a/tracked.txt b/tracked.txt\n"), tampered: []byte("tampered patch\n")},
		{name: "check", artifact: "check-01.txt", contents: []byte("stdout:\nok\n"), tampered: []byte("tampered check\n")},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newGoalFixture(t, "src")
			patch := []byte("reviewed patch\n")
			if err := fixture.store.WriteArtifact(fixture.state.ID, "result.patch", patch); err != nil {
				t.Fatalf("WriteArtifact(%q): %v", "result.patch", err)
			}
			fixture.state.Result = &Result{PatchSHA256: Digest(patch)}
			if testCase.artifact == "check-01.txt" {
				if err := fixture.store.WriteArtifact(fixture.state.ID, testCase.artifact, testCase.contents); err != nil {
					t.Fatalf("WriteArtifact(%q): %v", testCase.artifact, err)
				}
				fixture.state.Checks = []CheckResult{{OutputSHA256: Digest(testCase.contents)}}
			}
			if err := fixture.store.Save(context.Background(), fixture.state); err != nil {
				t.Fatalf("Save state: %v", err)
			}
			engine := newGoalTestExecution(fixture)
			if err := engine.verifyArtifacts(context.Background()); err != nil {
				t.Fatalf("verifyArtifacts before tampering: %v", err)
			}

			path := filepath.Join(fixture.state.RunDir, testCase.artifact)
			if err := os.WriteFile(path, testCase.tampered, 0o600); err != nil { //nolint:gosec // G306: private fixture artifact under t.TempDir
				t.Fatal(err)
			}
			err := engine.verifyArtifacts(context.Background())
			if err == nil || !strings.Contains(err.Error(), "retained artifact digest changed") {
				t.Fatalf("verifyArtifacts after tampering = %v, want retained digest refusal", err)
			}
		})
	}
}

func TestNativeEventRequiresMatchingThreadAndRole(t *testing.T) {
	fixture := newGoalFixture(t, "src")
	engine := newGoalTestExecution(fixture,
		Role{Name: "coordinator", NativeSessionID: "thread-coordinator"},
		Role{Name: "reviewer", NativeSessionID: "thread-reviewer"},
	)

	engine.nativeEvent("coordinator", nativeEventJSON(t, "turn/started", "thread-wrong", "turn-wrong", "", nil))
	engine.nativeEvent("coordinator", nativeEventJSON(t, "turn/started", "", "turn-missing", "", nil))
	engine.nativeEvent("reviewer", nativeEventJSON(t, "turn/started", "thread-coordinator", "turn-wrong-role", "", nil))
	if len(engine.traces) != 0 {
		t.Fatalf("wrong or missing thread/role created traces: %#v", engine.traces)
	}

	engine.nativeEvent("coordinator", nativeEventJSON(t, "turn/started", "thread-coordinator", "turn-correct", "", nil))
	engine.nativeEvent("coordinator", nativeEventJSON(t, "item/completed", "thread-coordinator", "turn-correct", "", map[string]any{
		"type": "agentMessage",
		"text": "native answer",
	}))
	engine.nativeEvent("coordinator", nativeEventJSON(t, "turn/completed", "thread-coordinator", "turn-correct", "completed", nil))

	if len(engine.traces) != 1 {
		t.Fatalf("traces = %d, want one correctly correlated role", len(engine.traces))
	}
	trace := engine.traces["coordinator"]
	if trace == nil || trace.Latest != "turn-correct" {
		t.Fatalf("coordinator trace = %#v, want turn-correct latest", trace)
	}
	turn := trace.Turns["turn-correct"]
	if turn.Status != "completed" || turn.Text != "native answer" {
		t.Fatalf("correct turn = %#v, want completed native answer", turn)
	}
	if _, found := trace.Turns["turn-wrong"]; found {
		t.Fatal("wrong-thread turn was captured")
	}
}

func newGoalTestExecution(fixture goalFixture, roles ...Role) *execution {
	state := fixture.state
	state.Roles = roles
	return &execution{
		store:   fixture.store,
		state:   state,
		traces:  map[string]*nativeTrace{},
		roles:   map[string]*nativeRole{},
		failure: make(chan error, 1),
		cancel:  func() {},
	}
}

func nativeEventJSON(t testing.TB, method, threadID, turnID, status string, item map[string]any) json.RawMessage {
	t.Helper()
	params := map[string]any{"threadId": threadID}
	if turnID != "" {
		params["turnId"] = turnID
	}
	if status != "" || strings.HasPrefix(method, "turn/") {
		params["turn"] = map[string]string{"id": turnID, "status": status}
	}
	if item != nil {
		params["item"] = item
	}
	payload, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
